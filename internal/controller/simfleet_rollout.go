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
	"slices"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpf"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// admitBatch is how many ordinals one admission step adds to the rollout.
const admitBatch = 100

// listInstances is this fleet's SimInstances keyed by ordinal. An instance
// carrying the fleet label but controlled by anything else, including an
// earlier fleet of the same name, is not this fleet's. The instances are the
// cache's own objects and must not be modified.
func (r *SimFleetReconciler) listInstances(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
) (map[int]*scalev1alpha1.SimInstance, error) {
	list := &scalev1alpha1.SimInstanceList{}
	if err := r.List(ctx, list,
		client.InNamespace(fleet.Namespace),
		client.MatchingLabels{naming.LabelFleetName: fleet.Name},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return nil, fmt.Errorf("list SimInstances for SimFleet %q: %w", fleet.Name, err)
	}
	instances := make(map[int]*scalev1alpha1.SimInstance, len(list.Items))
	for i := range list.Items {
		instance := &list.Items[i]
		owner := metav1.GetControllerOf(instance)
		if owner == nil || owner.UID != fleet.UID || owner.Kind != "SimFleet" {
			continue
		}
		instances[instance.Spec.Ordinal] = instance
	}
	return instances, nil
}

func desiredInstance(fleet *scalev1alpha1.SimFleet, ordinal int) *scalev1alpha1.SimInstance {
	return &scalev1alpha1.SimInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.InstanceName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal)),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(fleet, scalev1alpha1.GroupVersion.WithKind("SimFleet")),
			},
		},
		Spec: scalev1alpha1.SimInstanceSpec{
			Ordinal:   ordinal,
			HostIndex: naming.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
		},
	}
}

// nextAdmitted raises the rollout index by one batch once every admitted
// ordinal has all of its children, so a batch finishes before the next starts.
func nextAdmitted(fleet *scalev1alpha1.SimFleet, admitted int, instances map[int]*scalev1alpha1.SimInstance) int {
	if !instancesCreated(instances, 0, admitted) {
		return admitted
	}
	return max(admitted, min(admitted+admitBatch, fleet.Spec.Replicas))
}

// instancesCreated reports whether every ordinal in [start, end) has a live
// instance with all of its children. A Failed or deleting instance is about
// to lose them, so it does not count.
func instancesCreated(instances map[int]*scalev1alpha1.SimInstance, start, end int) bool {
	for ordinal := start; ordinal < end; ordinal++ {
		instance, ok := instances[ordinal]
		if !ok ||
			instance.Status.Phase == scalev1alpha1.SimInstanceFailed ||
			!instance.DeletionTimestamp.IsZero() ||
			!instanceConditionTrue(instance, scalev1alpha1.SimInstanceChildrenCreated) {
			return false
		}
	}
	return true
}

// instancesHave reports whether every ordinal in [start, end) has an instance
// with conditionType True.
func instancesHave(instances map[int]*scalev1alpha1.SimInstance, start, end int, conditionType string) bool {
	for ordinal := start; ordinal < end; ordinal++ {
		instance, ok := instances[ordinal]
		if !ok || !instanceConditionTrue(instance, conditionType) {
			return false
		}
	}
	return true
}

func sortedOrdinals(instances map[int]*scalev1alpha1.SimInstance) []int {
	return slices.Sorted(maps.Keys(instances))
}

// deleteScaledAwayInstances deletes every instance at or past spec.replicas.
// Each instance's teardown finalizer removes its children in order.
func (r *SimFleetReconciler) deleteScaledAwayInstances(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instances map[int]*scalev1alpha1.SimInstance,
) error {
	for _, ordinal := range sortedOrdinals(instances) {
		if ordinal < fleet.Spec.Replicas {
			continue
		}
		if err := r.deleteInstance(ctx, instances[ordinal], "ScaledDown"); err != nil {
			return err
		}
	}
	return nil
}

// deleteFailedInstances deletes every Failed instance so the create pass can
// replace it once its teardown has finished and the object is gone. A
// scaled-away instance was already deleted by deleteScaledAwayInstances.
func (r *SimFleetReconciler) deleteFailedInstances(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instances map[int]*scalev1alpha1.SimInstance,
) error {
	for _, ordinal := range sortedOrdinals(instances) {
		instance := instances[ordinal]
		if ordinal >= fleet.Spec.Replicas || instance.Status.Phase != scalev1alpha1.SimInstanceFailed {
			continue
		}
		if err := r.deleteInstance(ctx, instance, instance.Status.FailureReason); err != nil {
			return err
		}
	}
	return nil
}

// deleteInstance skips an instance already being deleted: its teardown is in
// progress and a second delete would change nothing.
func (r *SimFleetReconciler) deleteInstance(ctx context.Context, instance *scalev1alpha1.SimInstance, reason string) error {
	if !instance.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := r.Delete(ctx, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete SimInstance %q: %w", instance.Name, err)
	}
	logf.FromContext(ctx).Info("Deleted SimInstance",
		"simInstance", instance.Name,
		"ordinal", instance.Spec.Ordinal,
		"reason", reason,
	)
	return nil
}

// persistAdmitted records a raised rollout index before any instance it
// admits exists, so a restart mid-batch finishes the batch. The patch carries
// the resourceVersion, so a stale fleet gets a conflict and requeues instead
// of overwriting an index a newer pass already raised further.
func (r *SimFleetReconciler) persistAdmitted(ctx context.Context, fleet *scalev1alpha1.SimFleet, admitted int) error {
	base := fleet.DeepCopy()
	fleet.Status.AdmittedReplicas = admitted
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Status().Patch(ctx, fleet, patch); err != nil {
		return fmt.Errorf("update SimFleet %q admittedReplicas: %w", fleet.Name, err)
	}
	return nil
}

// createMissingInstances creates every admitted ordinal that has no instance.
// On AlreadyExists, a terminating earlier incarnation is waited out: its
// delete event brings the fleet back here once it is gone. A live instance of
// that name another owner controls never goes away on its own, so it is
// returned as the conflict the fleet status reports.
func (r *SimFleetReconciler) createMissingInstances(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	admitted int,
	instances map[int]*scalev1alpha1.SimInstance,
) (string, error) {
	conflict := ""
	for ordinal := range admitted {
		if _, ok := instances[ordinal]; ok {
			continue
		}
		instance := desiredInstance(fleet, ordinal)
		err := r.Create(ctx, instance)
		switch {
		case err == nil:
			logf.FromContext(ctx).V(1).Info("Created SimInstance", "simInstance", instance.Name, "ordinal", ordinal)
		case apierrors.IsAlreadyExists(err):
			held, err := r.instanceHeldByAnotherOwner(ctx, fleet, instance)
			if err != nil {
				return conflict, err
			}
			if held && conflict == "" {
				conflict = fmt.Sprintf("SimInstance %q for ordinal %d is controlled by another owner", instance.Name, ordinal)
			}
		default:
			return conflict, fmt.Errorf("create SimInstance %q: %w", instance.Name, err)
		}
	}
	return conflict, nil
}

// instanceHeldByAnotherOwner reports whether the existing object named like
// desired is live and not controlled by fleet.
func (r *SimFleetReconciler) instanceHeldByAnotherOwner(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	desired *scalev1alpha1.SimInstance,
) (bool, error) {
	existing := &scalev1alpha1.SimInstance{}
	found, err := r.getAfterAlreadyExists(ctx, client.ObjectKeyFromObject(desired), existing, "SimInstance")
	if err != nil || !found {
		return false, err
	}
	if !existing.DeletionTimestamp.IsZero() {
		logf.FromContext(ctx).V(1).Info("Skipped creating SimInstance that still exists",
			"simInstance", existing.Name,
			"ordinal", desired.Spec.Ordinal,
		)
		return false, nil
	}
	owner := metav1.GetControllerOf(existing)
	return owner == nil || owner.UID != fleet.UID, nil
}

// reconcileDPUNodes ensures the DPUNode of every admitted host whose devices
// all exist, listing the host's admitted devices, and reports how many of the
// admitted hosts have a DPUNode.
func (r *SimFleetReconciler) reconcileDPUNodes(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	admitted int,
	instances map[int]*scalev1alpha1.SimInstance,
) (int, error) {
	nodes := 0
	for hostIndex := range naming.DesiredNodeCount(admitted, fleet.Spec.DevicesPerDPUNode) {
		start, end := naming.HostOrdinalRange(hostIndex, fleet.Spec.DevicesPerDPUNode, admitted)
		if !instancesHave(instances, start, end, scalev1alpha1.SimInstanceDeviceCreated) {
			exists, err := r.nodeExists(ctx, fleet, hostIndex)
			if err != nil {
				return nodes, err
			}
			if exists {
				nodes++
			}
			continue
		}
		ready, err := r.ensureNode(ctx, fleet, dpf.DesiredHostNode(fleet, hostIndex, admitted), true)
		if err != nil {
			return nodes, err
		}
		if ready {
			nodes++
		}
	}
	return nodes, nil
}

func (r *SimFleetReconciler) nodeExists(ctx context.Context, fleet *scalev1alpha1.SimFleet, hostIndex int) (bool, error) {
	name := naming.NodeName(fleet.Name, hostIndex)
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: fleet.Namespace}, &provisioningv1.DPUNode{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get DPUNode %q: %w", name, err)
	}
	return true, nil
}

func (r *SimFleetReconciler) ensureNode(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	desired *provisioningv1.DPUNode,
	devicesReady bool,
) (bool, error) {
	name := desired.Name
	node := &provisioningv1.DPUNode{}
	key := client.ObjectKey{Name: name, Namespace: fleet.Namespace}
	if err := r.Get(ctx, key, node); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get DPUNode %q: %w", name, err)
		}
		if !devicesReady {
			return false, nil
		}
		node = desired
		if err := controllerutil.SetControllerReference(fleet, node, r.Scheme); err != nil {
			return false, fmt.Errorf("set DPUNode %q owner: %w", name, err)
		}
		if err := r.Create(ctx, node); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return r.getAndUpdateNode(ctx, fleet, key, desired, devicesReady)
			}
			return false, fmt.Errorf("create DPUNode %q: %w", name, err)
		}
		logf.FromContext(ctx).V(1).Info("Created DPUNode", "dpuNode", name)
		return true, nil
	}
	return r.updateNodeIfNeeded(ctx, fleet, node, desired, devicesReady)
}

func (r *SimFleetReconciler) getAndUpdateNode(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	key client.ObjectKey,
	desired *provisioningv1.DPUNode,
	devicesReady bool,
) (bool, error) {
	node := &provisioningv1.DPUNode{}
	found, err := r.getAfterAlreadyExists(ctx, key, node, "DPUNode")
	if !found {
		return false, err
	}
	return r.updateNodeIfNeeded(ctx, fleet, node, desired, devicesReady)
}

func (r *SimFleetReconciler) getAfterAlreadyExists(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	kind string,
) (bool, error) {
	if err := r.Get(ctx, key, obj); err != nil {
		// The object exists but this cache has not seen it yet. Its create
		// event brings the fleet back, which is cheaper than failing the pass.
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get %s %q after AlreadyExists: %w", kind, key.Name, err)
	}
	return true, nil
}

func (r *SimFleetReconciler) updateNodeIfNeeded(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	node *provisioningv1.DPUNode,
	desired *provisioningv1.DPUNode,
	devicesReady bool,
) (bool, error) {
	if err := validateCanAdopt(node, fleet, "DPUNode"); err != nil {
		return false, err
	}
	base := node.DeepCopy()
	mergeLabels(node, desired.Labels)
	if err := controllerutil.SetControllerReference(fleet, node, r.Scheme); err != nil {
		return false, fmt.Errorf("set DPUNode %q owner: %w", node.Name, err)
	}
	if devicesReady {
		node.Spec.DPUs = dpf.AppendMissingDPUs(node.Spec.DPUs, desired.Spec.DPUs)
	}
	if inventoryMetadataUnchanged(base, node) &&
		equality.Semantic.DeepEqual(base.Spec.DPUs, node.Spec.DPUs) {
		return true, nil
	}
	if err := r.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("patch DPUNode %q: %w", node.Name, err)
	}
	return true, nil
}

func validateCanAdopt(obj client.Object, fleet *scalev1alpha1.SimFleet, kind string) error {
	owner := metav1.GetControllerOf(obj)
	if owner == nil {
		return fmt.Errorf("%s %q is missing owner reference", kind, obj.GetName())
	}
	if owner.UID != fleet.UID || owner.Kind != "SimFleet" {
		return fmt.Errorf("%s %q is controlled by another owner", kind, obj.GetName())
	}
	return nil
}
