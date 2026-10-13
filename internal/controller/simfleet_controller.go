/*
Copyright 2026 NVIDIA.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

const (
	labelFleetName = "scale.dpu.nvidia.com/fleet-name"
	labelFleetUID  = "scale.dpu.nvidia.com/fleet-uid"
	labelOrdinal   = "scale.dpu.nvidia.com/ordinal"
	dpuDeviceField = "spec.dpuDeviceName"
)

const numberOfPFsDefault = 1

const simFleetTeardownFinalizer = "scale.dpu.nvidia.com/simfleet-teardown"

// SimFleetReconciler reconciles a SimFleet object
type SimFleetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// MaxConcurrentReconciles bounds parallel fleet reconciles. Zero uses
	// the controller-runtime default.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=simfleets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=simfleets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=simfleets/finalizers,verbs=update
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=provisioning.dpu.nvidia.com,resources=dpunodes,verbs=get;list;watch;create;update;patch;delete

// Reconcile rolls the fleet out through one SimInstance per ordinal: it
// retires scaled-away and Failed instances, admits ordinals a batch at a time,
// recreates any admitted ordinal whose instance is missing, groups created
// devices into DPUNodes, and aggregates instance status. SimInstance events
// drive the next pass, so nothing is requeued. A deleted fleet is held by its
// teardown finalizer until every instance is gone; see reconcileDelete.
func (r *SimFleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fleet := &scalev1alpha1.SimFleet{}
	if err := r.Get(ctx, req.NamespacedName, fleet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !fleet.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, fleet)
	}
	if err := r.ensureFleetFinalizer(ctx, fleet); err != nil {
		return ctrl.Result{}, err
	}

	instances, err := r.listInstances(ctx, fleet)
	if err != nil {
		return r.reconcileError(ctx, fleet, err)
	}
	if err := r.deleteScaledAwayInstances(ctx, fleet, instances); err != nil {
		return r.reconcileError(ctx, fleet, err)
	}
	admitted := min(fleet.Status.AdmittedReplicas, fleet.Spec.Replicas)
	if err := r.deleteFailedInstances(ctx, fleet, instances); err != nil {
		return r.reconcileError(ctx, fleet, err)
	}
	if next := nextAdmitted(fleet, admitted, instances); next > admitted {
		if err := r.persistAdmitted(ctx, fleet, next); err != nil {
			return ctrl.Result{}, err
		}
		admitted = next
	}
	conflict, err := r.createMissingInstances(ctx, fleet, admitted, instances)
	if err != nil {
		return r.reconcileError(ctx, fleet, err)
	}
	dpuNodes, err := r.reconcileDPUNodes(ctx, fleet, admitted, instances)
	if err != nil {
		return r.reconcileError(ctx, fleet, err)
	}
	if err := r.patchFleetStatus(ctx, fleet, admitted, instances, dpuNodes, conflict); err != nil {
		return ctrl.Result{}, err
	}

	logf.FromContext(ctx).V(1).Info("Reconciled SimFleet",
		"replicas", fleet.Spec.Replicas,
		"admittedReplicas", fleet.Status.AdmittedReplicas,
		"createdInstances", fleet.Status.CreatedInstances,
		"provisionedInstances", fleet.Status.ProvisionedInstances,
		"failedInstances", fleet.Status.FailedInstances,
		"dpuNodes", dpuNodes,
	)
	return ctrl.Result{}, nil
}

// ensureFleetFinalizer persists the teardown finalizer before the fleet
// creates anything, so no instance or DPUNode can outlive a fleet that was
// deleted without it.
func (r *SimFleetReconciler) ensureFleetFinalizer(ctx context.Context, fleet *scalev1alpha1.SimFleet) error {
	if controllerutil.ContainsFinalizer(fleet, simFleetTeardownFinalizer) {
		return nil
	}
	base := fleet.DeepCopy()
	controllerutil.AddFinalizer(fleet, simFleetTeardownFinalizer)
	if err := r.Patch(ctx, fleet, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("add SimFleet %q finalizer: %w", fleet.Name, err)
	}
	return nil
}

// reconcileDelete deletes every instance of a deleted fleet and releases the
// fleet only once they are all gone. Each instance's teardown then runs while
// the fleet is still readable, so quarantine knows the selection labels to
// strip, and the DPUNodes, which garbage collection removes only after the
// fleet, outlive every DPU on them. Instance delete events bring the fleet
// back, so nothing is requeued. Nothing is admitted, created, or reported
// while the fleet is deleting.
func (r *SimFleetReconciler) reconcileDelete(ctx context.Context, fleet *scalev1alpha1.SimFleet) error {
	if !controllerutil.ContainsFinalizer(fleet, simFleetTeardownFinalizer) {
		return nil
	}
	instances, err := r.listInstances(ctx, fleet)
	if err != nil {
		return err
	}
	for _, ordinal := range sortedOrdinals(instances) {
		if err := r.deleteInstance(ctx, instances[ordinal], "SimFleetDeleted"); err != nil {
			return err
		}
	}
	if len(instances) > 0 {
		logf.FromContext(ctx).V(1).Info("Waiting for SimInstances of deleted SimFleet", "remaining", len(instances))
		return nil
	}
	base := fleet.DeepCopy()
	controllerutil.RemoveFinalizer(fleet, simFleetTeardownFinalizer)
	if err := r.Patch(ctx, fleet, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove SimFleet %q finalizer: %w", fleet.Name, err)
	}
	logf.FromContext(ctx).Info("Tore down SimFleet")
	return nil
}

// aggregateStatus summarizes the instance statuses of the ordinals the fleet
// still wants. The rollout index is read from fleet.Status.AdmittedReplicas,
// which the caller has already brought to this pass's value. Instances at or
// past spec.replicas are being torn down and are not counted.
func aggregateStatus(
	fleet *scalev1alpha1.SimFleet,
	instances map[int]*scalev1alpha1.SimInstance,
	dpuNodes int,
) scalev1alpha1.SimFleetStatus {
	status := *fleet.Status.DeepCopy()
	replicas := fleet.Spec.Replicas
	admitted := status.AdmittedReplicas
	templateErr := validateVirtualMachineTemplate(fleet.Spec.VirtualMachineTemplate)

	var created, vmReady, provisioned, failed int
	observation := dpuObservation{}
	bootstrap := bootstrapOutcome{cloudInit: map[int]struct{}{}, err: templateErr}
	for _, ordinal := range sortedOrdinals(instances) {
		if ordinal >= replicas {
			continue
		}
		instance := instances[ordinal]
		deleting := !instance.DeletionTimestamp.IsZero()
		if instancesCreated(instances, ordinal, ordinal+1) {
			created++
		}
		if !deleting && instanceConditionTrue(instance, scalev1alpha1.SimInstanceVMReady) {
			vmReady++
		}
		if !deleting && instanceConditionTrue(instance, scalev1alpha1.SimInstanceDPUProvisioned) {
			provisioned++
		}
		if instance.Status.DPUName != "" {
			observation.count++
		}
		if instance.Status.Phase == scalev1alpha1.SimInstanceFailed {
			failed++
		}
		credentials := meta.FindStatusCondition(instance.Status.Conditions, scalev1alpha1.SimInstanceCredentialsReady)
		switch {
		case instanceConditionTrue(instance, scalev1alpha1.SimInstanceCredentialsReady):
			bootstrap.cloudInit[ordinal] = struct{}{}
		case credentials != nil && credentials.Reason != "Progressing" && bootstrap.blocked == nil:
			bootstrap.blocked = &bootstrapConditionError{reason: credentials.Reason, message: credentials.Message}
		}
		provisioned := meta.FindStatusCondition(instance.Status.Conditions, scalev1alpha1.SimInstanceDPUProvisioned)
		if provisioned != nil && provisioned.Reason == "DuplicateDPU" && observation.duplicateDevice == "" {
			observation.duplicateDevice = utils.DeviceName(fleet.Name, ordinal)
		}
	}

	status.ObservedGeneration = fleet.Generation
	status.CreatedInstances = created
	status.VMReadyInstances = vmReady
	status.ProvisionedInstances = provisioned
	status.FailedInstances = failed
	status.DPUNodes = dpuNodes

	inventoryReady := admitted == replicas && instancesCreated(instances, 0, admitted)
	meta.SetStatusCondition(&status.Conditions, inventoryReadyCondition(fleet, inventoryReady))
	meta.SetStatusCondition(&status.Conditions, dpusObservedCondition(fleet, observation))
	meta.SetStatusCondition(&status.Conditions, bootstrapReadyCondition(fleet, bootstrap, replicas))
	meta.SetStatusCondition(&status.Conditions,
		virtualMachinesReadyCondition(fleet, created, vmReady, replicas, templateErr))
	return status
}

// patchFleetStatus writes the aggregate status for this pass's rollout index,
// and only when it changed. A stale pass cannot roll a newer index back: the
// merge patch omits an admittedReplicas this pass did not change, and
// persistAdmitted raises it under an optimistic lock. The clamp below only
// stops a caller from passing an index under min(stored, spec.replicas),
// which Reconcile never does. A non-empty conflict names the instance
// another owner holds, which keeps the inventory from ever becoming ready.
func (r *SimFleetReconciler) patchFleetStatus(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	admitted int,
	instances map[int]*scalev1alpha1.SimInstance,
	dpuNodes int,
	conflict string,
) error {
	base := fleet.DeepCopy()
	stored := base.Status.AdmittedReplicas
	fleet.Status.AdmittedReplicas = max(admitted, min(stored, fleet.Spec.Replicas))
	fleet.Status = aggregateStatus(fleet, instances, dpuNodes)
	if conflict != "" {
		meta.SetStatusCondition(&fleet.Status.Conditions, metav1.Condition{
			Type:               scalev1alpha1.ConditionInventoryReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: fleet.Generation,
			Reason:             "OwnershipConflict",
			Message:            conflict,
		})
	}
	if equality.Semantic.DeepEqual(base.Status, fleet.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, fleet, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("update SimFleet %q status: %w", fleet.Name, err)
	}
	return nil
}

func inventoryMetadataUnchanged(before, after client.Object) bool {
	return equality.Semantic.DeepEqual(before.GetLabels(), after.GetLabels()) &&
		equality.Semantic.DeepEqual(before.GetOwnerReferences(), after.GetOwnerReferences())
}

// dpuObservation is what the fleet's instances say about its DPUs: how many
// instances have one, and the first device two DPUs claim.
type dpuObservation struct {
	count           int
	duplicateDevice string
}

func (r *SimFleetReconciler) reconcileError(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	reconcileErr error,
) (ctrl.Result, error) {
	condition := metav1.Condition{
		Type:               scalev1alpha1.ConditionInventoryReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: fleet.Generation,
		Reason:             "ReconcileError",
		Message:            reconcileErr.Error(),
	}
	base := fleet.DeepCopy()
	meta.SetStatusCondition(&fleet.Status.Conditions, condition)
	if equality.Semantic.DeepEqual(base.Status, fleet.Status) {
		return ctrl.Result{}, reconcileErr
	}
	if err := r.Status().Patch(ctx, fleet, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("%w; update ReconcileError status: %v", reconcileErr, err)
	}
	return ctrl.Result{}, reconcileErr
}

func inventoryReadyCondition(fleet *scalev1alpha1.SimFleet, ready bool) metav1.Condition {
	condition := metav1.Condition{
		Type:               scalev1alpha1.ConditionInventoryReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: fleet.Generation,
		Reason:             "Progressing",
		Message:            "Inventory objects are being created",
	}
	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Ready"
		condition.Message = "Inventory objects match the desired counts"
	}
	return condition
}

func dpusObservedCondition(fleet *scalev1alpha1.SimFleet, observation dpuObservation) metav1.Condition {
	condition := metav1.Condition{
		Type:               scalev1alpha1.ConditionDPUsObserved,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: fleet.Generation,
		Reason:             "WaitingForDPUs",
		Message:            fmt.Sprintf("Observed %d of %d desired DPUs", observation.count, fleet.Spec.Replicas),
	}
	switch {
	case observation.duplicateDevice != "":
		condition.Reason = "MultipleDPUs"
		condition.Message = fmt.Sprintf("Multiple DPUs reference DPUDevice %q", observation.duplicateDevice)
	case observation.count == fleet.Spec.Replicas:
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Ready"
		condition.Message = "All desired DPUs are observed"
	}
	return condition
}

// bootstrapReadyCondition reports whether the material a guest boots with is
// in place: one cloud-init Secret per bootable ordinal, carrying that
// ordinal's identity, mock configuration, and management credential.
//
// Nothing fleet-wide is left to report. The controller mints no tenant
// bootstrap token and reads no tenant cluster, so this condition can no
// longer be false because of the DPUCluster; it is false only for a
// configuration this controller refuses or a pass that has not finished
// writing those Secrets.
func bootstrapReadyCondition(
	fleet *scalev1alpha1.SimFleet,
	bootstrap bootstrapOutcome,
	desiredVMs int,
) metav1.Condition {
	condition := metav1.Condition{
		Type:               scalev1alpha1.ConditionBootstrapReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: fleet.Generation,
		Reason:             "Ready",
		Message:            "Guest bootstrap Secrets are in place",
	}
	switch {
	case bootstrap.err != nil:
		condition.Status = metav1.ConditionFalse
		// conditionReason covers template validation, which is the only
		// configuration this pass still refuses outright.
		condition.Reason = conditionReason(bootstrap.err)
		condition.Message = bootstrap.err.Error()
	case fleet.Spec.VirtualMachineTemplate == nil:
		condition.Message = "No guest bootstrap is required"
	case desiredVMs == 0:
		condition.Message = "No device is ready to boot a guest"
	case bootstrap.blocked != nil:
		condition.Status = metav1.ConditionFalse
		condition.Reason = bootstrap.blocked.reason
		condition.Message = bootstrap.blocked.message
	case len(bootstrap.cloudInit) < desiredVMs:
		condition.Status = metav1.ConditionFalse
		condition.Reason = "Progressing"
		condition.Message = fmt.Sprintf(
			"Wrote %d of %d guest bootstrap Secrets",
			len(bootstrap.cloudInit), desiredVMs,
		)
	}
	return condition
}

func virtualMachinesReadyCondition(
	fleet *scalev1alpha1.SimFleet,
	vmCount int,
	vmReady int,
	desiredVMs int,
	vmErr error,
) metav1.Condition {
	if vmErr != nil {
		return metav1.Condition{
			Type:               scalev1alpha1.ConditionVirtualMachinesReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: fleet.Generation,
			Reason:             conditionReason(vmErr),
			Message:            vmErr.Error(),
		}
	}
	condition := metav1.Condition{
		Type:               scalev1alpha1.ConditionVirtualMachinesReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: fleet.Generation,
		Reason:             "Progressing",
		Message:            fmt.Sprintf("Created %d of %d desired VirtualMachines", vmCount, desiredVMs),
	}
	switch {
	case fleet.Spec.VirtualMachineTemplate == nil || desiredVMs == 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Ready"
		condition.Message = "No VirtualMachines are required"
	case vmCount == desiredVMs && vmReady == vmCount:
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Ready"
		condition.Message = "All desired VirtualMachines are ready"
	case vmCount < desiredVMs:
		condition.Reason = "Progressing"
	default:
		condition.Reason = "WaitingForVMs"
		condition.Message = fmt.Sprintf("%d/%d VirtualMachines are ready", vmReady, desiredVMs)
	}
	return condition
}

func desiredDevice(fleet *scalev1alpha1.SimFleet, ordinal int) *provisioningv1.DPUDevice {
	numberOfPFs := numberOfPFsDefault
	if fleet.Spec.DPUDeviceTemplate != nil && fleet.Spec.DPUDeviceTemplate.Spec.NumberOfPFs != nil {
		numberOfPFs = *fleet.Spec.DPUDeviceTemplate.Spec.NumberOfPFs
	}
	return &provisioningv1.DPUDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.DeviceName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    deviceLabels(fleet, ordinal),
		},
		Spec: provisioningv1.DPUDeviceSpec{
			SerialNumber: utils.SerialNumber(fleet.Name, ordinal),
			NumberOfPFs:  &numberOfPFs,
		},
	}
}

// withoutReservedDeviceLabels copies labels, dropping the keys only the
// controller may set. It never mutates its argument, which is fleet spec.
// The CRD rejects a selector that uses one, so filtering here is what covers
// the DPUDevice template, which CEL does not reach, and any object admitted
// before that rule existed.
func withoutReservedDeviceLabels(labels map[string]string) map[string]string {
	allowed := make(map[string]string, len(labels))
	maps.Copy(allowed, labels)
	for _, reserved := range scalev1alpha1.ReservedDeviceLabelKeys {
		delete(allowed, reserved)
	}
	return allowed
}

// deviceLabels is everything a SimInstance puts on its DPUDevice when it
// creates or repairs it: who owns it, which ordinal it is, and which DPUNode
// it belongs to. It deliberately excludes spec.deviceSelectionLabels. Those
// are what DPF's DPUSet matches on, and a device DPF claims is a device it
// immediately opens Redfish against, so they are published by
// ensureInstanceBMCIP together with the address that makes Redfish reachable.
func deviceLabels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	labels := map[string]string{}
	if fleet.Spec.DPUDeviceTemplate != nil {
		maps.Copy(labels, withoutReservedDeviceLabels(fleet.Spec.DPUDeviceTemplate.Metadata.Labels))
	}
	maps.Copy(labels, inventoryLabels(fleet, fmt.Sprintf("%06d", ordinal)))
	labels[provisioningv1.DPUNodeNameLabel] = utils.NodeName(
		fleet.Name,
		utils.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
	)
	return labels
}

// deviceSelectionLabels is the selector DPF's DPUSet matches. Only
// ensureInstanceBMCIP writes it, and only alongside spec.bmcIP. A selector is
// fleet-author input, so it cannot carry a reserved identity key.
func deviceSelectionLabels(fleet *scalev1alpha1.SimFleet) map[string]string {
	return withoutReservedDeviceLabels(fleet.Spec.DeviceSelectionLabels)
}

func inventoryLabels(fleet *scalev1alpha1.SimFleet, ordinal string) map[string]string {
	return map[string]string{
		labelFleetName: fleet.Name,
		labelFleetUID:  string(fleet.UID),
		labelOrdinal:   ordinal,
	}
}

func mergeLabels(object client.Object, desired map[string]string) {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, desired)
	object.SetLabels(labels)
}

// SetupWithManager sets up the controller with the Manager. SimInstance
// events are the only children events the rollout needs: each instance
// reports its own children through its phase and conditions.
func (r *SimFleetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalev1alpha1.SimFleet{}).
		Owns(&scalev1alpha1.SimInstance{}, builder.WithPredicates(simInstanceWatchPredicate())).
		Owns(&provisioningv1.DPUNode{}, builder.WithPredicates(inventoryChildPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("simfleet").
		Complete(r)
}
