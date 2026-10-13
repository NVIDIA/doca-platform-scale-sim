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
	"errors"
	"fmt"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

const simInstanceTeardownFinalizer = "scale.dpu.nvidia.com/siminstance-teardown"

// SimInstanceReconciler converges the children of one SimFleet ordinal.
type SimInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the API server directly. The cache holds only
	// fleet-labelled Secrets, ServiceAccounts, Roles, and RoleBindings, so a
	// child that lost its labels, or that the cache has not seen yet, is
	// found only through it.
	APIReader client.Reader

	// ServiceAccountTokenIssuer mints per-DPU management API credentials.
	ServiceAccountTokenIssuer ServiceAccountTokenIssuer
	// ManagementCA authenticates the management API from simulated DPUs.
	ManagementCA []byte

	// Recorder emits instance Events. A nil value skips Events.
	Recorder events.EventRecorder
	// Now is the teardown clock. A nil value uses time.Now.
	Now func() time.Time
	// MaxConcurrentReconciles bounds parallel instance reconciles. Zero uses
	// the controller-runtime default.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=provisioning.dpu.nvidia.com,resources=dpudevices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=provisioning.dpu.nvidia.com,resources=dpus,verbs=get;list;watch;delete;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch;delete;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile converges one ordinal's children in order and reports progress
// through the instance conditions and phase.
func (r *SimInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	instance := &scalev1alpha1.SimInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := logf.FromContext(ctx).WithValues("simInstance", req.NamespacedName, "ordinal", instance.Spec.Ordinal)
	ctx = logf.IntoContext(ctx, log)

	if !instance.DeletionTimestamp.IsZero() {
		fleet, err := r.owningFleet(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		return r.reconcileDelete(ctx, fleet, instance)
	}
	if instance.Status.Phase == scalev1alpha1.SimInstanceFailed {
		return ctrl.Result{}, nil
	}
	fleet, err := r.owningFleet(ctx, instance)
	if err != nil || fleet == nil {
		return ctrl.Result{}, err
	}
	if !fleet.DeletionTimestamp.IsZero() {
		// The fleet's finalizer deletes this instance next; anything
		// converged now would only be torn down again.
		return ctrl.Result{}, nil
	}
	if err := r.ensureInstanceFinalizer(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	if drifted, err := r.failDriftedInstance(ctx, fleet, instance); err != nil || drifted {
		return ctrl.Result{}, err
	}

	base := instance.DeepCopy()
	children, convergeErr := r.converge(ctx, fleet, instance)
	setInstancePhase(fleet, instance, children)
	if errors.Is(convergeErr, errOwnershipConflict) {
		// The child is someone else's. It is never modified; the SimFleet
		// reconciler replaces this instance instead.
		instance.Status.Phase = scalev1alpha1.SimInstanceFailed
		instance.Status.FailureReason = "OwnershipConflict"
		log.Error(convergeErr, "Could not converge SimInstance child")
		if r.Recorder != nil {
			r.Recorder.Eventf(instance, nil, corev1.EventTypeWarning, "OwnershipConflict", "Converge", "%s", convergeErr.Error())
		}
		convergeErr = nil
	}
	if err := r.patchInstanceStatus(ctx, base, instance); err != nil {
		if convergeErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w; %v", convergeErr, err)
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, convergeErr
}

// owningFleet is the SimFleet that controls instance, or nil when it is gone.
// A fleet recreated under the same name is not the owner: garbage collection
// removes the instance instead.
func (r *SimInstanceReconciler) owningFleet(
	ctx context.Context,
	instance *scalev1alpha1.SimInstance,
) (*scalev1alpha1.SimFleet, error) {
	key, ok := controllerOwnerKey(instance, scalev1alpha1.GroupVersion.String(), "SimFleet")
	if !ok {
		return nil, nil
	}
	fleet := &scalev1alpha1.SimFleet{}
	if err := r.Get(ctx, key, fleet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get SimFleet %q: %w", key.Name, err)
	}
	if metav1.GetControllerOf(instance).UID != fleet.UID {
		return nil, nil
	}
	return fleet, nil
}

func (r *SimInstanceReconciler) ensureInstanceFinalizer(ctx context.Context, instance *scalev1alpha1.SimInstance) error {
	if controllerutil.ContainsFinalizer(instance, simInstanceTeardownFinalizer) {
		return nil
	}
	base := instance.DeepCopy()
	controllerutil.AddFinalizer(instance, simInstanceTeardownFinalizer)
	if err := r.Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("add SimInstance %q finalizer: %w", instance.Name, err)
	}
	return nil
}

func (r *SimInstanceReconciler) mapDPUToInstance(ctx context.Context, object client.Object) []ctrl.Request {
	dpu, ok := object.(*provisioningv1.DPU)
	if !ok || dpu.Spec.DPUDeviceName == "" {
		return nil
	}
	device := &provisioningv1.DPUDevice{}
	key := client.ObjectKey{Name: dpu.Spec.DPUDeviceName, Namespace: dpu.Namespace}
	if err := r.Get(ctx, key, device); err != nil {
		return nil
	}
	instanceKey, ok := controllerOwnerKey(device, scalev1alpha1.GroupVersion.String(), "SimInstance")
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: instanceKey}}
}

// mapFleetToInstances enqueues every instance the fleet controls, so a spec
// edit reaches the children each instance converges from it.
func (r *SimInstanceReconciler) mapFleetToInstances(ctx context.Context, object client.Object) []ctrl.Request {
	fleet, ok := object.(*scalev1alpha1.SimFleet)
	if !ok {
		return nil
	}
	list := &scalev1alpha1.SimInstanceList{}
	if err := r.List(ctx, list,
		client.InNamespace(fleet.Namespace),
		client.MatchingLabels{labelFleetName: fleet.Name},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list SimInstances of SimFleet", "simFleet", fleet.Name)
		return nil
	}
	var requests []ctrl.Request
	for i := range list.Items {
		instance := &list.Items[i]
		if owner := metav1.GetControllerOf(instance); owner == nil || owner.UID != fleet.UID {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	}
	return requests
}

// mapVMIToInstance resolves only KubeVirt-owned VMIs through their VM owner;
// malformed or unrelated controller references use KubeVirt's same-name VM.
func (r *SimInstanceReconciler) mapVMIToInstance(ctx context.Context, object client.Object) []ctrl.Request {
	vmi, ok := object.(*virtv1.VirtualMachineInstance)
	if !ok {
		return nil
	}
	vmName := vmi.Name
	if owner := metav1.GetControllerOf(vmi); owner != nil &&
		owner.APIVersion == virtv1.GroupVersion.String() &&
		owner.Kind == "VirtualMachine" {
		vmName = owner.Name
	}
	vm := &virtv1.VirtualMachine{}
	if err := r.Get(ctx, client.ObjectKey{Name: vmName, Namespace: vmi.Namespace}, vm); err != nil {
		return nil
	}
	instanceKey, ok := controllerOwnerKey(vm, scalev1alpha1.GroupVersion.String(), "SimInstance")
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: instanceKey}}
}

// SetupWithManager sets up the controller with the Manager. The DPU
// spec.dpuDeviceName index it reads is registered by SetupIndexes.
func (r *SimInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalev1alpha1.SimInstance{}).
		Owns(&provisioningv1.DPUDevice{}, builder.WithPredicates(inventoryChildPredicate())).
		Owns(&virtv1.VirtualMachine{}, builder.WithPredicates(virtualMachineWatchPredicate())).
		Owns(&corev1.Secret{}, builder.WithPredicates(secretWatchPredicate())).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(
			&provisioningv1.DPU{},
			handler.EnqueueRequestsFromMapFunc(r.mapDPUToInstance),
			builder.WithPredicates(dpuWatchPredicate()),
		).
		Watches(
			&virtv1.VirtualMachineInstance{},
			handler.EnqueueRequestsFromMapFunc(r.mapVMIToInstance),
			builder.WithPredicates(vmiWatchPredicate()),
		).
		Watches(
			&scalev1alpha1.SimFleet{},
			handler.EnqueueRequestsFromMapFunc(r.mapFleetToInstances),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("siminstance").
		Complete(r)
}

func setInstanceCondition(
	instance *scalev1alpha1.SimInstance,
	conditionType string,
	ok bool,
	reason, message string,
) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: instance.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func instanceConditionTrue(instance *scalev1alpha1.SimInstance, conditionType string) bool {
	return meta.IsStatusConditionTrue(instance.Status.Conditions, conditionType)
}

// failInstanceCondition records err on the step that returned it and passes
// it on.
func failInstanceCondition(instance *scalev1alpha1.SimInstance, conditionType string, err error) error {
	reason := conditionReason(err)
	if errors.Is(err, errOwnershipConflict) {
		reason = "OwnershipConflict"
	}
	setInstanceCondition(instance, conditionType, false, reason, err.Error())
	return err
}

// requiredInstanceConditions are the conditions a Ready instance holds. A
// fleet without a VM template boots no guest, so it has no VM to be ready and
// no BMC address to make its device selectable.
func requiredInstanceConditions(fleet *scalev1alpha1.SimFleet) []string {
	required := []string{
		scalev1alpha1.SimInstanceCredentialsReady,
		scalev1alpha1.SimInstanceDeviceCreated,
		scalev1alpha1.SimInstanceChildrenCreated,
		scalev1alpha1.SimInstanceDPUProvisioned,
	}
	if fleet.Spec.VirtualMachineTemplate != nil {
		required = append(required, scalev1alpha1.SimInstanceVMReady, scalev1alpha1.SimInstanceSelectable)
	}
	return required
}

// setInstancePhase records the UIDs of the children a Ready instance
// converged; those are the identities replacement detection compares against.
func setInstancePhase(fleet *scalev1alpha1.SimFleet, instance *scalev1alpha1.SimInstance, children instanceChildren) {
	for _, conditionType := range requiredInstanceConditions(fleet) {
		if !instanceConditionTrue(instance, conditionType) {
			instance.Status.Phase = scalev1alpha1.SimInstanceProvisioning
			return
		}
	}
	instance.Status.ObservedUIDs = children.observedUIDs()
	instance.Status.Phase = scalev1alpha1.SimInstanceReady
}

func (r *SimInstanceReconciler) patchInstanceStatus(
	ctx context.Context,
	base, instance *scalev1alpha1.SimInstance,
) error {
	instance.Status.ObservedGeneration = instance.Generation
	if equality.Semantic.DeepEqual(base.Status, instance.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, instance, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("update SimInstance %q status: %w", instance.Name, err)
	}
	return nil
}

// Teardown steps are the reasons of the Teardown condition. The persisted
// reason is the step a restarted controller resumes at, and each step's
// force deadline runs from status.teardownStartedAt, which each step resets.
const (
	teardownQuarantine        = "Quarantine"
	teardownDeleteDPU         = "DeleteDPU"
	teardownDeleteDPUDevice   = "DeleteDPUDevice"
	teardownDeleteVM          = "DeleteVM"
	teardownDeleteVMI         = "DeleteVMI"
	teardownDeleteCredentials = "DeleteCredentials"
)

// teardownPoll is the shortest teardown requeue, and the interval of a step
// whose objects' events cannot reach the instance.
const teardownPoll = time.Second

const lifecycleForceTimeout = 5 * time.Minute

// failDriftedInstance fails an instance whose recorded child was replaced.
// A Failed instance is never converged again; the SimFleet reconciler
// replaces it.
func (r *SimInstanceReconciler) failDriftedInstance(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (bool, error) {
	if instance.Status.ObservedUIDs.DPUDevice == "" {
		return false, nil
	}
	reason, err := r.detectDrift(ctx, fleet, instance)
	if err != nil || reason == "" {
		return false, err
	}
	base := instance.DeepCopy()
	instance.Status.Phase = scalev1alpha1.SimInstanceFailed
	instance.Status.FailureReason = reason
	if err := r.patchInstanceStatus(ctx, base, instance); err != nil {
		return true, err
	}
	logf.FromContext(ctx).Info("Failed SimInstance with a replaced child", "reason", reason)
	if r.Recorder != nil {
		r.Recorder.Eventf(instance, nil, corev1.EventTypeNormal, "SimInstanceFailed", "DetectDrift",
			"ordinal %d failed: %s", instance.Spec.Ordinal, reason)
	}
	return true, nil
}

// detectDrift reports whether a recorded child is missing or replaced. The
// DPU is found the way observeInstanceDPU found it, through the device it
// names, since DPF and not this controller chooses its name.
func (r *SimInstanceReconciler) detectDrift(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (reason string, err error) {
	recorded := instance.Status.ObservedUIDs
	ordinal := instance.Spec.Ordinal
	if recorded.DPU != "" {
		found, err := r.deviceDPUExists(ctx, instance.Namespace, utils.DeviceName(fleet.Name, ordinal), recorded.DPU)
		if err != nil {
			return "", err
		}
		if !found {
			return "DPUReplaced", nil
		}
	}
	vmName := utils.VirtualMachineName(fleet.Name, ordinal)
	for _, child := range []struct {
		uid, name, kind string
		obj             client.Object
	}{
		{recorded.DPUDevice, utils.DeviceName(fleet.Name, ordinal), "DPUDevice", &provisioningv1.DPUDevice{}},
		{recorded.VirtualMachine, vmName, "VirtualMachine", &virtv1.VirtualMachine{}},
		{recorded.VirtualMachineInstance, vmName, "VirtualMachineInstance", &virtv1.VirtualMachineInstance{}},
	} {
		if child.uid == "" {
			continue
		}
		key := client.ObjectKey{Name: child.name, Namespace: instance.Namespace}
		found, err := r.getChild(ctx, key, child.obj, child.kind)
		if err != nil {
			return "", err
		}
		if !found || string(child.obj.GetUID()) != child.uid {
			return child.kind + "Replaced", nil
		}
	}
	return "", nil
}

// deviceDPUExists reports whether a DPU with uid names deviceName.
func (r *SimInstanceReconciler) deviceDPUExists(ctx context.Context, namespace, deviceName, uid string) (bool, error) {
	list := &provisioningv1.DPUList{}
	if err := r.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingFields{dpuDeviceField: deviceName},
	); err != nil {
		return false, fmt.Errorf("list DPUs for DPUDevice %q: %w", deviceName, err)
	}
	for i := range list.Items {
		if string(list.Items[i].UID) == uid {
			return true, nil
		}
	}
	return false, nil
}

// teardownTarget names one ordinal's children. The fleet may already be
// gone, so the names come from the instance's label and spec then.
type teardownTarget struct {
	fleet          *scalev1alpha1.SimFleet
	namespace      string
	deviceName     string
	dpuName        string
	vmName         string
	cloudInitName  string
	credentialName string
}

func newTeardownTarget(fleet *scalev1alpha1.SimFleet, instance *scalev1alpha1.SimInstance) (teardownTarget, bool) {
	ordinal := instance.Spec.Ordinal
	if fleet != nil {
		return teardownTarget{
			fleet:          fleet,
			namespace:      instance.Namespace,
			deviceName:     utils.DeviceName(fleet.Name, ordinal),
			dpuName:        utils.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, ordinal),
			vmName:         utils.VirtualMachineName(fleet.Name, ordinal),
			cloudInitName:  utils.CloudInitSecretName(fleet.Name, ordinal),
			credentialName: dpuAgentCredentialName(fleet, ordinal),
		}, true
	}
	name := instance.Labels[labelFleetName]
	if name == "" {
		key, ok := controllerOwnerKey(instance, scalev1alpha1.GroupVersion.String(), "SimFleet")
		if !ok {
			return teardownTarget{}, false
		}
		name = key.Name
	}
	dpuName := utils.DPUNameForHost(name, instance.Spec.HostIndex, ordinal)
	return teardownTarget{
		namespace:      instance.Namespace,
		deviceName:     utils.DeviceName(name, ordinal),
		dpuName:        dpuName,
		vmName:         utils.VirtualMachineName(name, ordinal),
		cloudInitName:  utils.CloudInitSecretName(name, ordinal),
		credentialName: dpuName,
	}, true
}

func (t teardownTarget) key(name string) client.ObjectKey {
	return client.ObjectKey{Name: name, Namespace: t.namespace}
}

type teardownStep struct {
	name string
	run  func(context.Context, teardownTarget, *scalev1alpha1.SimInstance, time.Time) (bool, error)
	// poll is set when the step's objects cannot enqueue the instance as
	// they go, so it cannot wait for their events.
	poll bool
}

// teardownSteps are woken by the delete events of the objects they wait on,
// except the VMI step: once its VirtualMachine is gone, mapVMIToInstance has
// nothing to resolve the VMI through.
func (r *SimInstanceReconciler) teardownSteps() []teardownStep {
	return []teardownStep{
		{name: teardownQuarantine, run: r.quarantineInstanceDevice},
		{name: teardownDeleteDPU, run: r.deleteInstanceDPU},
		{name: teardownDeleteDPUDevice, run: r.deleteInstanceDevice},
		{name: teardownDeleteVM, run: r.deleteInstanceVM},
		{name: teardownDeleteVMI, run: r.deleteInstanceVMI, poll: true},
		{name: teardownDeleteCredentials, run: r.deleteInstanceCredentials},
	}
}

// teardownRequeue is when a blocked step next needs a pass that no event
// brings: its force deadline, or its poll interval.
func teardownRequeue(step teardownStep, instance *scalev1alpha1.SimInstance, now time.Time) time.Duration {
	if step.poll {
		return teardownPoll
	}
	remaining := lifecycleForceTimeout - now.Sub(instance.Status.TeardownStartedAt.Time)
	return max(remaining, teardownPoll)
}

// reconcileDelete tears an instance's children down one step at a time and
// removes the finalizer once every step's objects are gone. Only objects
// proven to be this instance's are deleted; anything else is left in place.
func (r *SimInstanceReconciler) reconcileDelete(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(instance, simInstanceTeardownFinalizer) {
		return ctrl.Result{}, nil
	}
	target, ok := newTeardownTarget(fleet, instance)
	if !ok {
		logf.FromContext(ctx).Error(nil, "Could not name SimInstance children without a fleet; skipped teardown")
		return ctrl.Result{}, r.removeInstanceFinalizer(ctx, instance)
	}

	base := instance.DeepCopy()
	now := r.now()
	instance.Status.Phase = scalev1alpha1.SimInstanceTerminating
	steps := r.teardownSteps()
	current := 0
	if condition := meta.FindStatusCondition(instance.Status.Conditions, scalev1alpha1.SimInstanceTeardown); condition != nil {
		for i, step := range steps {
			if step.name == condition.Reason {
				current = i
			}
		}
	}
	if !instanceConditionTrue(instance, scalev1alpha1.SimInstanceTeardown) || instance.Status.TeardownStartedAt == nil {
		startTeardownStep(instance, steps[current].name, now)
	}
	for i := current; i < len(steps); i++ {
		if i != current {
			startTeardownStep(instance, steps[i].name, now)
		}
		done, err := steps[i].run(ctx, target, instance, now)
		if err != nil {
			if patchErr := r.patchInstanceStatus(ctx, base, instance); patchErr != nil {
				return ctrl.Result{}, fmt.Errorf("%w; %v", err, patchErr)
			}
			return ctrl.Result{}, err
		}
		if !done {
			requeue := teardownRequeue(steps[i], instance, now)
			return ctrl.Result{RequeueAfter: requeue}, r.patchInstanceStatus(ctx, base, instance)
		}
	}
	return ctrl.Result{}, r.removeInstanceFinalizer(ctx, instance)
}

func startTeardownStep(instance *scalev1alpha1.SimInstance, step string, now time.Time) {
	setInstanceCondition(instance, scalev1alpha1.SimInstanceTeardown, true, step,
		fmt.Sprintf("Teardown step %s is running", step))
	instance.Status.TeardownStartedAt = &metav1.Time{Time: now}
}

func (r *SimInstanceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *SimInstanceReconciler) removeInstanceFinalizer(ctx context.Context, instance *scalev1alpha1.SimInstance) error {
	base := instance.DeepCopy()
	controllerutil.RemoveFinalizer(instance, simInstanceTeardownFinalizer)
	if err := r.Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove SimInstance %q finalizer: %w", instance.Name, err)
	}
	logf.FromContext(ctx).Info("Tore down SimInstance")
	return nil
}

// quarantineInstanceDevice removes the selection labels so DPUSet stops
// targeting the device before its DPU is deleted. Without the fleet those
// labels are unknown and the device is deleted as it is.
func (r *SimInstanceReconciler) quarantineInstanceDevice(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	_ time.Time,
) (bool, error) {
	if target.fleet == nil {
		return true, nil
	}
	device := &provisioningv1.DPUDevice{}
	found, err := r.getChild(ctx, target.key(target.deviceName), device, "DPUDevice")
	if err != nil {
		return false, err
	}
	if !found || !ownsTeardownChild(ctx, instance, device, "DPUDevice") {
		return true, nil
	}
	base := device.DeepCopy()
	for key := range deviceSelectionLabels(target.fleet) {
		delete(device.Labels, key)
	}
	if inventoryMetadataUnchanged(base, device) {
		return true, nil
	}
	if err := r.Patch(ctx, device, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("quarantine DPUDevice %q: %w", device.Name, err)
	}
	logf.FromContext(ctx).V(1).Info("Quarantined DPUDevice", "dpuDevice", device.Name)
	return true, nil
}

// deleteInstanceDPU deletes the DPU DPF created for this instance's device,
// then its kubeadm join Secret. A DPU of the same name that is not provably
// ours keeps its join Secret too.
func (r *SimInstanceReconciler) deleteInstanceDPU(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	now time.Time,
) (bool, error) {
	dpu := &provisioningv1.DPU{}
	found, err := r.getChild(ctx, target.key(target.dpuName), dpu, "DPU")
	if err != nil {
		return false, err
	}
	if !found {
		return true, r.deleteInstanceJoinSecret(ctx, target)
	}
	ours, err := r.ownsInstanceDPU(ctx, target, instance, dpu)
	if err != nil || !ours {
		return err == nil, err
	}
	if err := r.deleteTeardownObject(ctx, instance, dpu, "DPU", now); err != nil {
		return false, err
	}
	return false, r.deleteInstanceJoinSecret(ctx, target)
}

// ownsInstanceDPU proves a DPU is ours through the device it names: that
// device is controlled by the instance, or it is gone and the DPU is the one
// whose UID the instance recorded. A DPU has no owner reference to check.
func (r *SimInstanceReconciler) ownsInstanceDPU(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	dpu *provisioningv1.DPU,
) (bool, error) {
	log := logf.FromContext(ctx)
	if dpu.Spec.DPUDeviceName != target.deviceName {
		log.Info("Left a DPU that names another DPUDevice", "dpu", dpu.Name, "dpuDevice", dpu.Spec.DPUDeviceName)
		return false, nil
	}
	device := &provisioningv1.DPUDevice{}
	found, err := r.getChild(ctx, target.key(target.deviceName), device, "DPUDevice")
	if err != nil {
		return false, err
	}
	if found {
		return ownsTeardownChild(ctx, instance, device, "DPUDevice"), nil
	}
	recorded := instance.Status.ObservedUIDs.DPU
	if recorded == "" || string(dpu.UID) != recorded {
		log.Info("Left a DPU whose UID was not recorded", "dpu", dpu.Name, "uid", dpu.UID, "recordedUID", recorded)
		return false, nil
	}
	return true, nil
}

func (r *SimInstanceReconciler) deleteInstanceJoinSecret(ctx context.Context, target teardownTarget) error {
	name := utils.KubeadmJoinSecretName(target.dpuName)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: target.namespace}}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete join Secret %q: %w", name, err)
	}
	return nil
}

func (r *SimInstanceReconciler) deleteInstanceDevice(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	now time.Time,
) (bool, error) {
	return r.deleteControlledChild(ctx, target, instance, &provisioningv1.DPUDevice{}, target.deviceName, "DPUDevice", now)
}

func (r *SimInstanceReconciler) deleteInstanceVM(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	now time.Time,
) (bool, error) {
	return r.deleteControlledChild(ctx, target, instance, &virtv1.VirtualMachine{}, target.vmName, "VirtualMachine", now)
}

// deleteInstanceVMI deletes any VMI the VirtualMachine left behind. The VMI
// is ours only through this instance's VirtualMachine, or, once that is gone,
// through the VM and VMI UIDs the instance recorded.
func (r *SimInstanceReconciler) deleteInstanceVMI(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	now time.Time,
) (bool, error) {
	vm := &virtv1.VirtualMachine{}
	found, err := r.getChild(ctx, target.key(target.vmName), vm, "VirtualMachine")
	if err != nil {
		return false, err
	}
	if !found {
		vm = nil
	}
	vmi := &virtv1.VirtualMachineInstance{}
	found, err = r.getChild(ctx, target.key(target.vmName), vmi, "VirtualMachineInstance")
	if err != nil || !found {
		return err == nil, err
	}
	if !instanceOwnsVMI(instance, vm, vmi) {
		logf.FromContext(ctx).Info("Left a VirtualMachineInstance this SimInstance does not own",
			"virtualMachineInstance", vmi.Name)
		return true, nil
	}
	return false, r.deleteTeardownObject(ctx, instance, vmi, "VirtualMachineInstance", now)
}

func instanceOwnsVMI(
	instance *scalev1alpha1.SimInstance,
	vm *virtv1.VirtualMachine,
	vmi *virtv1.VirtualMachineInstance,
) bool {
	if vm != nil {
		return guestBelongsToInstance(instance, vm, vmi)
	}
	owner := metav1.GetControllerOf(vmi)
	recorded := instance.Status.ObservedUIDs
	return owner != nil &&
		owner.APIVersion == virtv1.GroupVersion.String() &&
		owner.Kind == "VirtualMachine" &&
		recorded.VirtualMachine != "" && string(owner.UID) == recorded.VirtualMachine &&
		recorded.VirtualMachineInstance != "" && string(vmi.UID) == recorded.VirtualMachineInstance
}

func (r *SimInstanceReconciler) deleteInstanceCredentials(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	now time.Time,
) (bool, error) {
	done := true
	for _, child := range []struct {
		obj        client.Object
		name, kind string
	}{
		{&corev1.Secret{}, target.cloudInitName, "Secret"},
		{&rbacv1.RoleBinding{}, target.credentialName, "RoleBinding"},
		{&rbacv1.Role{}, target.credentialName, "Role"},
		{&corev1.ServiceAccount{}, target.credentialName, "ServiceAccount"},
	} {
		gone, err := r.deleteControlledChild(ctx, target, instance, child.obj, child.name, child.kind, now)
		if err != nil {
			return false, err
		}
		done = done && gone
	}
	return done, nil
}

// deleteControlledChild reports true once the named object is gone or is
// not controlled by the instance.
func (r *SimInstanceReconciler) deleteControlledChild(
	ctx context.Context,
	target teardownTarget,
	instance *scalev1alpha1.SimInstance,
	obj client.Object,
	name, kind string,
	now time.Time,
) (bool, error) {
	found, err := r.getChild(ctx, target.key(name), obj, kind)
	if err != nil {
		return false, err
	}
	if !found || !ownsTeardownChild(ctx, instance, obj, kind) {
		return true, nil
	}
	return false, r.deleteTeardownObject(ctx, instance, obj, kind, now)
}

func ownsTeardownChild(ctx context.Context, instance *scalev1alpha1.SimInstance, obj client.Object, kind string) bool {
	if err := validateControlledBy(obj, instance, "SimInstance", kind); err != nil {
		logf.FromContext(ctx).Info("Left a child this SimInstance does not own", "kind", kind, "name", obj.GetName(),
			"reason", err.Error())
		return false
	}
	return true
}

// deleteTeardownObject deletes obj, which the caller proved is this
// instance's. Once the current step has been blocked on it for
// lifecycleForceTimeout, its finalizers are stripped.
func (r *SimInstanceReconciler) deleteTeardownObject(
	ctx context.Context,
	instance *scalev1alpha1.SimInstance,
	obj client.Object,
	kind string,
	now time.Time,
) error {
	if obj.GetDeletionTimestamp().IsZero() {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %q: %w", kind, obj.GetName(), err)
		}
		logf.FromContext(ctx).V(1).Info("Deleted SimInstance child", "kind", kind, "name", obj.GetName())
		return nil
	}
	elapsed := now.Sub(instance.Status.TeardownStartedAt.Time)
	if elapsed < lifecycleForceTimeout {
		return nil
	}
	return stripFinalizersAndForceDelete(ctx, r.Client, r.Recorder, instance, obj, kind, instance.Spec.Ordinal, elapsed)
}
