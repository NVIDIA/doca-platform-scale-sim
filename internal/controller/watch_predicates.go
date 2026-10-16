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
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

func inventoryChildPredicate() predicate.Predicate {
	return onUpdate(inventoryChildChanged)
}

// dpuWatchPredicate passes the DPU updates the instance reads: the device it
// names, kubelet readiness, and the start of deletion.
func dpuWatchPredicate() predicate.Predicate {
	return onUpdate(dpuWatchChanged)
}

// virtualMachineWatchPredicate limits owned-VM reconciles to readiness and
// ownership/lifecycle changes that can affect SimFleet status or management.
func virtualMachineWatchPredicate() predicate.Predicate {
	return onUpdate(virtualMachineWatchChanged)
}

// vmiWatchPredicate ignores high-volume VMI status noise while preserving
// phase and Ready transitions that can change SimFleet readiness.
func vmiWatchPredicate() predicate.Predicate {
	return onUpdate(vmiWatchChanged)
}

// secretWatchPredicate limits owned-Secret reconciles to metadata drift while
// ignoring create-only .data updates that SimFleet never patches.
func secretWatchPredicate() predicate.Predicate {
	return onUpdate(secretWatchChanged)
}

// simInstanceWatchPredicate passes the instance updates the fleet rollout
// reads: phase, DPU name, conditions, and the start of deletion.
func simInstanceWatchPredicate() predicate.Predicate {
	return onUpdate(simInstanceWatchChanged)
}

func simInstanceWatchChanged(oldObj, newObj client.Object) bool {
	oldInstance, ok := oldObj.(*scalev1alpha1.SimInstance)
	if !ok {
		return false
	}
	newInstance, ok := newObj.(*scalev1alpha1.SimInstance)
	if !ok {
		return false
	}
	return oldInstance.Status.Phase != newInstance.Status.Phase ||
		oldInstance.Status.DPUName != newInstance.Status.DPUName ||
		!equality.Semantic.DeepEqual(oldInstance.Status.Conditions, newInstance.Status.Conditions) ||
		deletionTimestampChanged(oldObj, newObj)
}

// onUpdate keeps create and delete (the Funcs defaults) and filters updates.
func onUpdate(changed func(oldObj, newObj client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return changed(e.ObjectOld, e.ObjectNew)
		},
	}
}

// inventoryChildChanged reports whether an update touched state the owner
// writes and therefore restores. An event that passes reconciles the owner, so
// anything DPF mutates during normal provisioning must be ignored here:
// annotations, finalizers, added labels, extra owner references, and the spec
// fields DPF fills in. Deletion still passes, through the deletion timestamp
// and through delete events.
func inventoryChildChanged(oldObj, newObj client.Object) bool {
	return managedLabelDropped(oldObj.GetLabels(), newObj.GetLabels()) ||
		controllerRefChanged(oldObj, newObj) ||
		deletionTimestampChanged(oldObj, newObj) ||
		managedSpecChanged(oldObj, newObj)
}

// inventoryManagedLabelKeys are the identity labels the controller always
// writes. Template and deviceSelectionLabels keys vary per fleet, so their
// drift is restored on the owner's next pass rather than on every DPF label
// change.
var inventoryManagedLabelKeys = []string{
	naming.LabelFleetName,
	naming.LabelFleetUID,
	naming.LabelOrdinal,
	provisioningv1.DPUNodeNameLabel,
}

// managedLabelDropped reports whether a controller-owned identity label was
// removed or given a different value. Extra labels another actor added or
// later changed are not SimFleet drift.
func managedLabelDropped(oldLabels, newLabels map[string]string) bool {
	for _, key := range inventoryManagedLabelKeys {
		oldValue, had := oldLabels[key]
		if !had {
			continue
		}
		if newValue, ok := newLabels[key]; !ok || newValue != oldValue {
			return true
		}
	}
	return false
}

// controllerRefChanged compares only the controller owner reference, the one
// SimFleet sets and validates before adopting a child. Other owner references
// belong to whoever added them.
func controllerRefChanged(oldObj, newObj client.Object) bool {
	return !equality.Semantic.DeepEqual(metav1.GetControllerOf(oldObj), metav1.GetControllerOf(newObj))
}

func deletionTimestampChanged(oldObj, newObj client.Object) bool {
	return !equality.Semantic.DeepEqual(oldObj.GetDeletionTimestamp(), newObj.GetDeletionTimestamp())
}

// managedSpecChanged reports spec drift the owner can actually restore.
// DPUDevice spec and DPUNode reboot method are create-time only.
func managedSpecChanged(oldObj, newObj client.Object) bool {
	oldTyped, ok := oldObj.(*provisioningv1.DPUNode)
	if !ok {
		return false
	}
	newTyped, ok := newObj.(*provisioningv1.DPUNode)
	if !ok {
		return false
	}
	return !equality.Semantic.DeepEqual(oldTyped.Spec.DPUs, newTyped.Spec.DPUs)
}

func dpuWatchChanged(oldObj, newObj client.Object) bool {
	oldDPU, ok := oldObj.(*provisioningv1.DPU)
	if !ok {
		return false
	}
	newDPU, ok := newObj.(*provisioningv1.DPU)
	if !ok {
		return false
	}
	// DPUProvisioned follows KubeletConfigured, and converge does not requeue,
	// so this status transition is the only thing that moves the instance to
	// Ready.
	return oldDPU.Spec.DPUDeviceName != newDPU.Spec.DPUDeviceName ||
		kubeletConfigured(oldDPU) != kubeletConfigured(newDPU) ||
		deletionTimestampChanged(oldDPU, newDPU)
}

func virtualMachineWatchChanged(oldObj, newObj client.Object) bool {
	oldVM, ok := oldObj.(*virtv1.VirtualMachine)
	if !ok {
		return false
	}
	newVM, ok := newObj.(*virtv1.VirtualMachine)
	if !ok {
		return false
	}
	return oldVM.Status.Ready != newVM.Status.Ready ||
		deletionTimestampChanged(oldVM, newVM) ||
		controllerRefChanged(oldVM, newVM)
}

func vmiWatchChanged(oldObj, newObj client.Object) bool {
	oldVMI, ok := oldObj.(*virtv1.VirtualMachineInstance)
	if !ok {
		return false
	}
	newVMI, ok := newObj.(*virtv1.VirtualMachineInstance)
	if !ok {
		return false
	}
	if deletionTimestampChanged(oldVMI, newVMI) {
		return true
	}
	if oldVMI.Status.Phase != newVMI.Status.Phase {
		return true
	}
	// The bridged address becomes the device's spec.bmcIP. It appears on its
	// own status update, before Ready and without a phase change, so ignoring
	// it would leave the device unselectable until some unrelated event.
	if kubevirt.GuestBridgedIPv4(oldVMI) != kubevirt.GuestBridgedIPv4(newVMI) {
		return true
	}
	oldReady, oldFound := vmiReadyStatus(oldVMI)
	newReady, newFound := vmiReadyStatus(newVMI)
	return oldFound != newFound || oldReady != newReady
}

func vmiReadyStatus(vmi *virtv1.VirtualMachineInstance) (corev1.ConditionStatus, bool) {
	for _, condition := range vmi.Status.Conditions {
		if condition.Type == virtv1.VirtualMachineInstanceReady {
			return condition.Status, true
		}
	}
	return "", false
}

func secretWatchChanged(oldObj, newObj client.Object) bool {
	oldSecret, ok := oldObj.(*corev1.Secret)
	if !ok {
		return false
	}
	newSecret, ok := newObj.(*corev1.Secret)
	if !ok {
		return false
	}
	if deletionTimestampChanged(oldSecret, newSecret) ||
		controllerRefChanged(oldSecret, newSecret) ||
		!equality.Semantic.DeepEqual(oldSecret.Labels, newSecret.Labels) {
		return true
	}
	return false
}
