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
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

func fleetControllerReference() metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion:         scalev1alpha1.GroupVersion.String(),
		Kind:               "SimFleet",
		Name:               "scale-3000",
		UID:                "fleet-uid",
		Controller:         &controller,
		BlockOwnerDeletion: &controller,
	}
}

func fleetDevice() *provisioningv1.DPUDevice {
	numberOfPFs := 1
	return &provisioningv1.DPUDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000-device-000000",
			Namespace: "default",
			Labels: map[string]string{
				labelFleetName: "scale-3000",
				labelFleetUID:  "fleet-uid",
				labelOrdinal:   "000000",
			},
			OwnerReferences: []metav1.OwnerReference{fleetControllerReference()},
			ResourceVersion: "1",
		},
		Spec: provisioningv1.DPUDeviceSpec{
			SerialNumber: "MT0000000000",
			NumberOfPFs:  &numberOfPFs,
		},
	}
}

func fleetNode() *provisioningv1.DPUNode {
	return &provisioningv1.DPUNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000-host-0000",
			Namespace: "default",
			Labels: map[string]string{
				labelFleetName: "scale-3000",
				labelFleetUID:  "fleet-uid",
				labelOrdinal:   "0000",
			},
			OwnerReferences: []metav1.OwnerReference{fleetControllerReference()},
			Finalizers:      []string{"provisioning.dpu.nvidia.com/dpunode-protection"},
			ResourceVersion: "1",
		},
		Spec: provisioningv1.DPUNodeSpec{
			DPUs:             []provisioningv1.DPURef{{Name: "scale-3000-device-000000"}},
			NodeRebootMethod: &provisioningv1.NodeRebootMethod{External: &provisioningv1.External{}},
		},
	}
}

func updatedDevice(mutate func(*provisioningv1.DPUDevice)) *provisioningv1.DPUDevice {
	next := fleetDevice()
	next.ResourceVersion = "2"
	mutate(next)
	return next
}

func updatedNode(mutate func(*provisioningv1.DPUNode)) *provisioningv1.DPUNode {
	next := fleetNode()
	next.ResourceVersion = "2"
	mutate(next)
	return next
}

func TestInventoryChildPredicateCreateDelete(t *testing.T) {
	pred := inventoryChildPredicate()
	if !pred.Create(event.CreateEvent{Object: fleetDevice()}) {
		t.Error("DPUDevice create should enqueue")
	}
	if !pred.Delete(event.DeleteEvent{Object: fleetDevice()}) {
		t.Error("DPUDevice delete should enqueue")
	}
}

func TestInventoryChildPredicateUpdate(t *testing.T) {
	pred := inventoryChildPredicate()
	psid := "MT_STATUS"
	installInterface := "hostAgent"
	bmcIP := "10.1.2.3"
	numberOfPFs := 2
	deletedAt := metav1.Now()

	tests := []struct {
		name    string
		oldObj  client.Object
		newObj  client.Object
		enqueue bool
	}{
		{
			name:   "DPUDevice status only",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Status.PSID = &psid
			}),
		},
		{
			name:   "DPUNode status only",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.Status.DPUInstallInterface = &installInterface
			}),
		},
		{
			name:   "DPUDevice finalizer added by DPF",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Finalizers = append(d.Finalizers, "provisioning.dpu.nvidia.com/dpudevice-protection")
			}),
		},
		{
			name:   "DPUNode finalizer removed by DPF",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.Finalizers = nil
			}),
		},
		{
			name:   "DPUNode annotation added by DPF",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.Annotations = map[string]string{"provisioning.dpu.nvidia.com/reboot": "required"}
			}),
		},
		{
			name:   "DPUDevice label added by DPF",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Labels["provisioning.dpu.nvidia.com/dpu-detected"] = "true"
			}),
		},
		{
			name: "DPUDevice extra DPF label later removed",
			oldObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Labels["provisioning.dpu.nvidia.com/dpu-detected"] = "true"
			}),
			newObj: fleetDevice(),
		},
		{
			name:   "DPUDevice spec field owned by DPF",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Generation = 2
				d.Spec.BMCIP = &bmcIP
			}),
		},
		{
			name:   "DPUDevice non-controller owner added",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.OwnerReferences = append(d.OwnerReferences, metav1.OwnerReference{
					APIVersion: "provisioning.dpu.nvidia.com/v1alpha1",
					Kind:       "DPUNode",
					Name:       "scale-3000-host-0000",
					UID:        "node-uid",
				})
			}),
		},
		{
			name:   "DPUDevice fleet label removed",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				delete(d.Labels, labelOrdinal)
			}),
			enqueue: true,
		},
		{
			name:   "DPUNode fleet label overwritten",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.Labels[labelFleetUID] = "other-uid"
			}),
			enqueue: true,
		},
		{
			name:   "DPUDevice controller reference removed",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.OwnerReferences = nil
			}),
			enqueue: true,
		},
		{
			name:   "DPUNode deletion timestamp set",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.DeletionTimestamp = &deletedAt
			}),
			enqueue: true,
		},
		{
			name:   "DPUDevice numberOfPFs changed",
			oldObj: fleetDevice(),
			newObj: updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Generation = 2
				d.Spec.NumberOfPFs = &numberOfPFs
			}),
		},
		{
			name:   "DPUNode spec.dpus emptied",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.Generation = 2
				n.Spec.DPUs = nil
			}),
			enqueue: true,
		},
		{
			name:   "DPUNode reboot method changed",
			oldObj: fleetNode(),
			newObj: updatedNode(func(n *provisioningv1.DPUNode) {
				n.Generation = 2
				n.Spec.NodeRebootMethod = &provisioningv1.NodeRebootMethod{HostAgent: &provisioningv1.HostAgent{}}
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{ObjectOld: test.oldObj, ObjectNew: test.newObj})
			if got != test.enqueue {
				t.Errorf("Update() = %t, want %t", got, test.enqueue)
			}
		})
	}
}

func TestDPUWatchPredicate(t *testing.T) {
	pred := dpuWatchPredicate()
	oldDPU := &provisioningv1.DPU{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "host-device",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: provisioningv1.DPUSpec{
			DPUDeviceName: "scale-3000-device-000000",
			DPUNodeName:   "scale-3000-host-0000",
		},
	}

	if !pred.Create(event.CreateEvent{Object: oldDPU}) {
		t.Fatal("DPU create should enqueue")
	}
	if !pred.Delete(event.DeleteEvent{Object: oldDPU}) {
		t.Fatal("DPU delete should enqueue")
	}

	statusDPU := oldDPU.DeepCopy()
	statusDPU.ResourceVersion = "2"
	statusDPU.Status.Phase = provisioningv1.DPUReady
	if pred.Update(event.UpdateEvent{ObjectOld: oldDPU, ObjectNew: statusDPU}) {
		t.Fatal("DPU status-only update should not enqueue")
	}

	deletingDPU := oldDPU.DeepCopy()
	deletedAt := metav1.Now()
	deletingDPU.DeletionTimestamp = &deletedAt
	deletingDPU.ResourceVersion = "2"
	if !pred.Update(event.UpdateEvent{ObjectOld: oldDPU, ObjectNew: deletingDPU}) {
		t.Fatal("DPU deletion timestamp change should enqueue")
	}

	flavorDPU := oldDPU.DeepCopy()
	flavorDPU.Spec.DPUFlavor = "other"
	flavorDPU.Generation = 2
	if pred.Update(event.UpdateEvent{ObjectOld: oldDPU, ObjectNew: flavorDPU}) {
		t.Fatal("unrelated DPU spec change should not enqueue")
	}

	renamed := oldDPU.DeepCopy()
	renamed.Spec.DPUDeviceName = "scale-3000-device-000001"
	renamed.Generation = 2
	if !pred.Update(event.UpdateEvent{ObjectOld: oldDPU, ObjectNew: renamed}) {
		t.Fatal("DPU spec.dpuDeviceName change should enqueue")
	}

	configured := oldDPU.DeepCopy()
	configured.ResourceVersion = "2"
	configured.Status.AgentStatus = &provisioningv1.AgentStatus{}
	meta.SetStatusCondition(&configured.Status.AgentStatus.Conditions, metav1.Condition{
		Type:   "KubeletConfigured",
		Status: metav1.ConditionTrue,
		Reason: "Configured",
	})
	if !pred.Update(event.UpdateEvent{ObjectOld: oldDPU, ObjectNew: configured}) {
		t.Fatal("DPU KubeletConfigured becoming True should enqueue")
	}
	if !pred.Update(event.UpdateEvent{ObjectOld: configured, ObjectNew: oldDPU}) {
		t.Fatal("DPU KubeletConfigured leaving True should enqueue")
	}
	stillConfigured := configured.DeepCopy()
	stillConfigured.ResourceVersion = "3"
	stillConfigured.Status.Phase = provisioningv1.DPUReady
	if pred.Update(event.UpdateEvent{ObjectOld: configured, ObjectNew: stillConfigured}) {
		t.Fatal("DPU status update that keeps KubeletConfigured should not enqueue")
	}
}

func TestVirtualMachineWatchPredicate(t *testing.T) {
	pred := virtualMachineWatchPredicate()
	oldVM := &virtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "scale-3000-dpu-000000",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{fleetControllerReference()},
			ResourceVersion: "1",
		},
	}

	if !pred.Create(event.CreateEvent{Object: oldVM}) {
		t.Fatal("VirtualMachine create should enqueue")
	}
	if !pred.Delete(event.DeleteEvent{Object: oldVM}) {
		t.Fatal("VirtualMachine delete should enqueue")
	}

	deletedAt := metav1.Now()
	tests := []struct {
		name    string
		oldObj  client.Object
		newObj  client.Object
		mutate  func(*virtv1.VirtualMachine)
		enqueue bool
	}{
		{
			name:   "nil old object",
			newObj: oldVM,
		},
		{
			name:   "nil new object",
			oldObj: oldVM,
		},
		{
			name:   "wrong old object type",
			oldObj: &provisioningv1.DPU{},
			newObj: &provisioningv1.DPU{},
		},
		{
			name:   "wrong new object type",
			oldObj: oldVM,
			newObj: &provisioningv1.DPU{},
		},
		{
			name: "annotation only",
			mutate: func(vm *virtv1.VirtualMachine) {
				vm.Annotations = map[string]string{"example.com/noise": "value"}
			},
		},
		{
			name: "printable status only",
			mutate: func(vm *virtv1.VirtualMachine) {
				vm.Status.PrintableStatus = virtv1.VirtualMachineStatusRunning
			},
		},
		{
			name: "status condition only",
			mutate: func(vm *virtv1.VirtualMachine) {
				vm.Status.Conditions = append(vm.Status.Conditions, virtv1.VirtualMachineCondition{
					Type:   "Failure",
					Status: corev1.ConditionFalse,
				})
			},
		},
		{
			name: "ready changed",
			mutate: func(vm *virtv1.VirtualMachine) {
				vm.Status.Ready = true
			},
			enqueue: true,
		},
		{
			name: "deletion timestamp changed",
			mutate: func(vm *virtv1.VirtualMachine) {
				vm.DeletionTimestamp = &deletedAt
			},
			enqueue: true,
		},
		{
			name: "controller owner changed",
			mutate: func(vm *virtv1.VirtualMachine) {
				vm.OwnerReferences[0].Name = "other-fleet"
			},
			enqueue: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldObj, newObj := test.oldObj, test.newObj
			if test.mutate != nil {
				oldObj = oldVM
				newVM := oldVM.DeepCopy()
				newVM.ResourceVersion = "2"
				test.mutate(newVM)
				newObj = newVM
			}
			got := pred.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
			if got != test.enqueue {
				t.Errorf("Update() = %t, want %t", got, test.enqueue)
			}
		})
	}
}

func TestVMIWatchPredicate(t *testing.T) {
	pred := vmiWatchPredicate()
	oldVMI := &virtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "scale-3000-dpu-000000",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Status: virtv1.VirtualMachineInstanceStatus{
			Phase: virtv1.Pending,
			Conditions: []virtv1.VirtualMachineInstanceCondition{{
				Type:   virtv1.VirtualMachineInstanceReady,
				Status: corev1.ConditionFalse,
				Reason: "GuestNotRunning",
			}},
			Interfaces: []virtv1.VirtualMachineInstanceNetworkInterface{{
				Name: "default", InterfaceName: "eth0",
			}},
		},
	}

	if !pred.Create(event.CreateEvent{Object: oldVMI}) {
		t.Fatal("VMI create should enqueue")
	}
	if !pred.Delete(event.DeleteEvent{Object: oldVMI}) {
		t.Fatal("VMI delete should enqueue")
	}

	deletedAt := metav1.Now()
	tests := []struct {
		name    string
		mutate  func(*virtv1.VirtualMachineInstance)
		enqueue bool
	}{
		{
			name: "metadata only",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Annotations = map[string]string{"example.com/noise": "value"}
			},
		},
		{
			name: "unrelated status noise",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Reason = "StatusNoise"
			},
		},
		{
			name: "ready condition details only",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions[0].Reason = "DifferentReason"
				vmi.Status.Conditions[0].Message = "Different message"
				vmi.Status.Conditions[0].LastTransitionTime = metav1.Now()
			},
		},
		{
			name: "unrelated condition",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions = append(vmi.Status.Conditions, virtv1.VirtualMachineInstanceCondition{
					Type:   virtv1.VirtualMachineInstanceSynchronized,
					Status: corev1.ConditionTrue,
				})
			},
		},
		{
			name: "phase changed",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Phase = virtv1.Running
			},
			enqueue: true,
		},
		{
			name: "ready status changed",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions[0].Status = corev1.ConditionTrue
			},
			enqueue: true,
		},
		{
			name: "deletion timestamp changed",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.DeletionTimestamp = &deletedAt
			},
			enqueue: true,
		},
		{
			name: "ready condition removed",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions = nil
			},
			enqueue: true,
		},
		// The bridged address is what the instance publishes as spec.bmcIP,
		// and it arrives on its own status update: the guest reports it well
		// before Ready and without a phase change. Ignoring it would leave
		// the device unselectable until some unrelated event.
		{
			name: "default interface reported an address",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Interfaces = []virtv1.VirtualMachineInstanceNetworkInterface{{
					Name: "default", IP: "10.1.2.3",
				}}
			},
			enqueue: true,
		},
		{
			name: "interface noise the fleet does not publish",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Interfaces[0].MAC = "02:00:00:00:00:01"
				vmi.Status.Interfaces[0].QueueCount = 4
				vmi.Status.Interfaces[0].InfoSource = "domain, guest-agent"
			},
		},
		{
			name: "another network reported an address",
			mutate: func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Interfaces = append(
					vmi.Status.Interfaces,
					virtv1.VirtualMachineInstanceNetworkInterface{Name: "secondary", IP: "10.9.9.9"},
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newVMI := oldVMI.DeepCopy()
			newVMI.ResourceVersion = "2"
			test.mutate(newVMI)
			got := pred.Update(event.UpdateEvent{ObjectOld: oldVMI, ObjectNew: newVMI})
			if got != test.enqueue {
				t.Errorf("Update() = %t, want %t", got, test.enqueue)
			}
		})
	}

	// A guest that comes back on a different address has to republish
	// spec.bmcIP, so the move itself has to reach the reconciler.
	t.Run("default interface moved to a new address", func(t *testing.T) {
		published := oldVMI.DeepCopy()
		published.Status.Interfaces[0].IP = "10.1.2.3"
		moved := published.DeepCopy()
		moved.ResourceVersion = "3"
		moved.Status.Interfaces[0].IP = "10.4.5.6"
		if !pred.Update(event.UpdateEvent{ObjectOld: published, ObjectNew: moved}) {
			t.Error("Update() = false for a guest that moved address")
		}
	})
}

func fleetSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000-dpu-000000-cloud-init",
			Namespace: "default",
			Labels: map[string]string{
				labelFleetName: "scale-3000",
				labelFleetUID:  "fleet-uid",
				labelOrdinal:   "000000",
			},
			OwnerReferences: []metav1.OwnerReference{fleetControllerReference()},
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"userdata": []byte("#cloud-config\nhostname: scale-3000-dpu-000000\n"),
		},
	}
}

func TestSecretWatchPredicate(t *testing.T) {
	pred := secretWatchPredicate()
	oldSecret := fleetSecret()

	if !pred.Create(event.CreateEvent{Object: oldSecret}) {
		t.Fatal("Secret create should enqueue")
	}
	if !pred.Delete(event.DeleteEvent{Object: oldSecret}) {
		t.Fatal("Secret delete should enqueue")
	}

	deletedAt := metav1.Now()
	tests := []struct {
		name    string
		oldObj  client.Object
		newObj  client.Object
		mutate  func(*corev1.Secret)
		enqueue bool
	}{
		{
			name:   "nil old object",
			newObj: oldSecret,
		},
		{
			name:   "nil new object",
			oldObj: oldSecret,
		},
		{
			name:   "wrong old object type",
			oldObj: &provisioningv1.DPU{},
			newObj: &provisioningv1.DPU{},
		},
		{
			name:   "wrong new object type",
			oldObj: oldSecret,
			newObj: &provisioningv1.DPU{},
		},
		{
			name: "data only",
			mutate: func(secret *corev1.Secret) {
				secret.Data["userdata"] = []byte("#cloud-config\nhostname: mutated\n")
			},
		},
		{
			name: "annotation only",
			mutate: func(secret *corev1.Secret) {
				secret.Annotations = map[string]string{"example.com/noise": "value"}
			},
		},
		{
			name: "fleet label removed",
			mutate: func(secret *corev1.Secret) {
				delete(secret.Labels, labelFleetUID)
			},
			enqueue: true,
		},
		{
			name: "controller owner changed",
			mutate: func(secret *corev1.Secret) {
				secret.OwnerReferences[0].Name = "other-fleet"
			},
			enqueue: true,
		},
		{
			name: "deletion timestamp changed",
			mutate: func(secret *corev1.Secret) {
				secret.DeletionTimestamp = &deletedAt
			},
			enqueue: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldObj, newObj := test.oldObj, test.newObj
			if test.mutate != nil {
				oldObj = oldSecret
				newSecret := oldSecret.DeepCopy()
				newSecret.ResourceVersion = "2"
				test.mutate(newSecret)
				newObj = newSecret
			}
			got := pred.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
			if got != test.enqueue {
				t.Errorf("Update() = %t, want %t", got, test.enqueue)
			}
		})
	}
}

func TestSimInstanceWatchPredicate(t *testing.T) {
	fleet := rolloutFleet(3, 1, 3)
	oldInstance := fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated)
	pred := simInstanceWatchPredicate()
	if !pred.Create(event.CreateEvent{Object: oldInstance}) || !pred.Delete(event.DeleteEvent{Object: oldInstance}) {
		t.Fatal("SimInstance create and delete should enqueue the fleet")
	}

	for _, test := range []struct {
		name    string
		mutate  func(*scalev1alpha1.SimInstance)
		enqueue bool
	}{
		{"no change", func(*scalev1alpha1.SimInstance) {}, false},
		{"annotation", func(i *scalev1alpha1.SimInstance) { i.Annotations = map[string]string{"a": "b"} }, false},
		{"teardown clock", func(i *scalev1alpha1.SimInstance) {
			now := metav1.Now()
			i.Status.TeardownStartedAt = &now
		}, false},
		{"phase", func(i *scalev1alpha1.SimInstance) { i.Status.Phase = scalev1alpha1.SimInstanceFailed }, true},
		{"dpu name", func(i *scalev1alpha1.SimInstance) { i.Status.DPUName = "dpu-0" }, true},
		{"condition", func(i *scalev1alpha1.SimInstance) {
			setInstanceCondition(i, scalev1alpha1.SimInstanceChildrenCreated, true, "Created", "")
		}, true},
		{"deletion", func(i *scalev1alpha1.SimInstance) {
			now := metav1.Now()
			i.DeletionTimestamp = &now
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			newInstance := oldInstance.DeepCopy()
			test.mutate(newInstance)
			if got := pred.Update(event.UpdateEvent{ObjectOld: oldInstance, ObjectNew: newInstance}); got != test.enqueue {
				t.Fatalf("Update() = %t, want %t", got, test.enqueue)
			}
		})
	}
}
