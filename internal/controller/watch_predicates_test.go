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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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

func watchedDPU() *provisioningv1.DPU {
	return &provisioningv1.DPU{
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
}

func watchedVM() *virtv1.VirtualMachine {
	return &virtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "scale-3000-dpu-000000",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{fleetControllerReference()},
			ResourceVersion: "1",
		},
	}
}

func watchedVMI() *virtv1.VirtualMachineInstance {
	return &virtv1.VirtualMachineInstance{
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

// watchUpdate is an update event for predicates whose table also covers
// malformed events: when mutate is set it replaces oldObj/newObj with the
// fixture and a mutated copy of it.
type watchUpdate[T client.Object] struct {
	oldObj client.Object
	newObj client.Object
	mutate func(T)
}

var _ = Describe("Watch predicates", func() {
	Describe("Inventory child", func() {
		It("enqueues DPUDevice create and delete", func() {
			pred := inventoryChildPredicate()
			Expect(pred.Create(event.CreateEvent{Object: fleetDevice()})).To(BeTrue(), "DPUDevice create should enqueue")
			Expect(pred.Delete(event.DeleteEvent{Object: fleetDevice()})).To(BeTrue(), "DPUDevice delete should enqueue")
		})

		DescribeTable("filters updates",
			func(oldObj, newObj client.Object, enqueue bool) {
				pred := inventoryChildPredicate()
				Expect(pred.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})).To(Equal(enqueue))
			},
			Entry("DPUDevice status only", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Status.PSID = ptr.To("MT_STATUS")
			}), false),
			Entry("DPUNode status only", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.Status.DPUInstallInterface = ptr.To("hostAgent")
			}), false),
			Entry("DPUDevice finalizer added by DPF", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Finalizers = append(d.Finalizers, "provisioning.dpu.nvidia.com/dpudevice-protection")
			}), false),
			Entry("DPUNode finalizer removed by DPF", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.Finalizers = nil
			}), false),
			Entry("DPUNode annotation added by DPF", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.Annotations = map[string]string{"provisioning.dpu.nvidia.com/reboot": "required"}
			}), false),
			Entry("DPUDevice label added by DPF", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Labels["provisioning.dpu.nvidia.com/dpu-detected"] = "true"
			}), false),
			Entry("DPUDevice extra DPF label later removed", updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Labels["provisioning.dpu.nvidia.com/dpu-detected"] = "true"
			}), fleetDevice(), false),
			Entry("DPUDevice spec field owned by DPF", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Generation = 2
				d.Spec.BMCIP = ptr.To("10.1.2.3")
			}), false),
			Entry("DPUDevice non-controller owner added", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.OwnerReferences = append(d.OwnerReferences, metav1.OwnerReference{
					APIVersion: "provisioning.dpu.nvidia.com/v1alpha1",
					Kind:       "DPUNode",
					Name:       "scale-3000-host-0000",
					UID:        "node-uid",
				})
			}), false),
			Entry("DPUDevice fleet label removed", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				delete(d.Labels, labelOrdinal)
			}), true),
			Entry("DPUNode fleet label overwritten", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.Labels[labelFleetUID] = "other-uid"
			}), true),
			Entry("DPUDevice controller reference removed", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.OwnerReferences = nil
			}), true),
			Entry("DPUNode deletion timestamp set", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.DeletionTimestamp = ptr.To(metav1.Now())
			}), true),
			Entry("DPUDevice numberOfPFs changed", fleetDevice(), updatedDevice(func(d *provisioningv1.DPUDevice) {
				d.Generation = 2
				d.Spec.NumberOfPFs = ptr.To(2)
			}), false),
			Entry("DPUNode spec.dpus emptied", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.Generation = 2
				n.Spec.DPUs = nil
			}), true),
			Entry("DPUNode reboot method changed", fleetNode(), updatedNode(func(n *provisioningv1.DPUNode) {
				n.Generation = 2
				n.Spec.NodeRebootMethod = &provisioningv1.NodeRebootMethod{HostAgent: &provisioningv1.HostAgent{}}
			}), false),
		)
	})

	Describe("DPU", func() {
		var oldDPU *provisioningv1.DPU

		BeforeEach(func() {
			oldDPU = watchedDPU()
		})

		update := func(oldObj, newObj client.Object) bool {
			return dpuWatchPredicate().Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
		}

		It("enqueues DPU create and delete", func() {
			pred := dpuWatchPredicate()
			Expect(pred.Create(event.CreateEvent{Object: oldDPU})).To(BeTrue(), "DPU create should enqueue")
			Expect(pred.Delete(event.DeleteEvent{Object: oldDPU})).To(BeTrue(), "DPU delete should enqueue")
		})

		It("ignores a status-only update", func() {
			statusDPU := oldDPU.DeepCopy()
			statusDPU.ResourceVersion = "2"
			statusDPU.Status.Phase = provisioningv1.DPUReady
			Expect(update(oldDPU, statusDPU)).To(BeFalse())
		})

		It("enqueues a deletion timestamp change", func() {
			deletingDPU := oldDPU.DeepCopy()
			deletingDPU.DeletionTimestamp = ptr.To(metav1.Now())
			deletingDPU.ResourceVersion = "2"
			Expect(update(oldDPU, deletingDPU)).To(BeTrue())
		})

		It("ignores an unrelated spec change", func() {
			flavorDPU := oldDPU.DeepCopy()
			flavorDPU.Spec.DPUFlavor = "other"
			flavorDPU.Generation = 2
			Expect(update(oldDPU, flavorDPU)).To(BeFalse())
		})

		It("enqueues a spec.dpuDeviceName change", func() {
			renamed := oldDPU.DeepCopy()
			renamed.Spec.DPUDeviceName = "scale-3000-device-000001"
			renamed.Generation = 2
			Expect(update(oldDPU, renamed)).To(BeTrue())
		})

		It("enqueues KubeletConfigured transitions and ignores updates that keep it True", func() {
			configured := oldDPU.DeepCopy()
			configured.ResourceVersion = "2"
			configured.Status.AgentStatus = &provisioningv1.AgentStatus{}
			meta.SetStatusCondition(&configured.Status.AgentStatus.Conditions, metav1.Condition{
				Type:   "KubeletConfigured",
				Status: metav1.ConditionTrue,
				Reason: "Configured",
			})
			Expect(update(oldDPU, configured)).To(BeTrue(), "DPU KubeletConfigured becoming True should enqueue")
			Expect(update(configured, oldDPU)).To(BeTrue(), "DPU KubeletConfigured leaving True should enqueue")

			stillConfigured := configured.DeepCopy()
			stillConfigured.ResourceVersion = "3"
			stillConfigured.Status.Phase = provisioningv1.DPUReady
			Expect(update(configured, stillConfigured)).To(BeFalse(),
				"DPU status update that keeps KubeletConfigured should not enqueue")
		})
	})

	Describe("VirtualMachine", func() {
		It("enqueues VirtualMachine create and delete", func() {
			pred := virtualMachineWatchPredicate()
			Expect(pred.Create(event.CreateEvent{Object: watchedVM()})).To(BeTrue(), "VirtualMachine create should enqueue")
			Expect(pred.Delete(event.DeleteEvent{Object: watchedVM()})).To(BeTrue(), "VirtualMachine delete should enqueue")
		})

		DescribeTable("filters updates",
			func(test watchUpdate[*virtv1.VirtualMachine], enqueue bool) {
				oldObj, newObj := test.oldObj, test.newObj
				if test.mutate != nil {
					oldVM := watchedVM()
					newVM := oldVM.DeepCopy()
					newVM.ResourceVersion = "2"
					test.mutate(newVM)
					oldObj, newObj = oldVM, newVM
				}
				got := virtualMachineWatchPredicate().Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
				Expect(got).To(Equal(enqueue))
			},
			Entry("nil old object", watchUpdate[*virtv1.VirtualMachine]{newObj: watchedVM()}, false),
			Entry("nil new object", watchUpdate[*virtv1.VirtualMachine]{oldObj: watchedVM()}, false),
			Entry("wrong old object type",
				watchUpdate[*virtv1.VirtualMachine]{oldObj: &provisioningv1.DPU{}, newObj: &provisioningv1.DPU{}}, false),
			Entry("wrong new object type",
				watchUpdate[*virtv1.VirtualMachine]{oldObj: watchedVM(), newObj: &provisioningv1.DPU{}}, false),
			Entry("annotation only", watchUpdate[*virtv1.VirtualMachine]{mutate: func(vm *virtv1.VirtualMachine) {
				vm.Annotations = map[string]string{"example.com/noise": "value"}
			}}, false),
			Entry("printable status only", watchUpdate[*virtv1.VirtualMachine]{mutate: func(vm *virtv1.VirtualMachine) {
				vm.Status.PrintableStatus = virtv1.VirtualMachineStatusRunning
			}}, false),
			Entry("status condition only", watchUpdate[*virtv1.VirtualMachine]{mutate: func(vm *virtv1.VirtualMachine) {
				vm.Status.Conditions = append(vm.Status.Conditions, virtv1.VirtualMachineCondition{
					Type:   "Failure",
					Status: corev1.ConditionFalse,
				})
			}}, false),
			Entry("ready changed", watchUpdate[*virtv1.VirtualMachine]{mutate: func(vm *virtv1.VirtualMachine) {
				vm.Status.Ready = true
			}}, true),
			Entry("deletion timestamp changed", watchUpdate[*virtv1.VirtualMachine]{mutate: func(vm *virtv1.VirtualMachine) {
				vm.DeletionTimestamp = ptr.To(metav1.Now())
			}}, true),
			Entry("controller owner changed", watchUpdate[*virtv1.VirtualMachine]{mutate: func(vm *virtv1.VirtualMachine) {
				vm.OwnerReferences[0].Name = "other-fleet"
			}}, true),
		)
	})

	Describe("VirtualMachineInstance", func() {
		It("enqueues VMI create and delete", func() {
			pred := vmiWatchPredicate()
			Expect(pred.Create(event.CreateEvent{Object: watchedVMI()})).To(BeTrue(), "VMI create should enqueue")
			Expect(pred.Delete(event.DeleteEvent{Object: watchedVMI()})).To(BeTrue(), "VMI delete should enqueue")
		})

		DescribeTable("filters updates",
			func(mutate func(*virtv1.VirtualMachineInstance), enqueue bool) {
				oldVMI := watchedVMI()
				newVMI := oldVMI.DeepCopy()
				newVMI.ResourceVersion = "2"
				mutate(newVMI)
				got := vmiWatchPredicate().Update(event.UpdateEvent{ObjectOld: oldVMI, ObjectNew: newVMI})
				Expect(got).To(Equal(enqueue))
			},
			Entry("metadata only", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Annotations = map[string]string{"example.com/noise": "value"}
			}, false),
			Entry("unrelated status noise", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Reason = "StatusNoise"
			}, false),
			Entry("ready condition details only", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions[0].Reason = "DifferentReason"
				vmi.Status.Conditions[0].Message = "Different message"
				vmi.Status.Conditions[0].LastTransitionTime = metav1.Now()
			}, false),
			Entry("unrelated condition", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions = append(vmi.Status.Conditions, virtv1.VirtualMachineInstanceCondition{
					Type:   virtv1.VirtualMachineInstanceSynchronized,
					Status: corev1.ConditionTrue,
				})
			}, false),
			Entry("phase changed", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Phase = virtv1.Running
			}, true),
			Entry("ready status changed", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions[0].Status = corev1.ConditionTrue
			}, true),
			Entry("deletion timestamp changed", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.DeletionTimestamp = ptr.To(metav1.Now())
			}, true),
			Entry("ready condition removed", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Conditions = nil
			}, true),
			// The bridged address is what the instance publishes as spec.bmcIP,
			// and it arrives on its own status update: the guest reports it well
			// before Ready and without a phase change. Ignoring it would leave
			// the device unselectable until some unrelated event.
			Entry("default interface reported an address", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Interfaces = []virtv1.VirtualMachineInstanceNetworkInterface{{
					Name: "default", IP: "10.1.2.3",
				}}
			}, true),
			Entry("interface noise the fleet does not publish", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Interfaces[0].MAC = "02:00:00:00:00:01"
				vmi.Status.Interfaces[0].QueueCount = 4
				vmi.Status.Interfaces[0].InfoSource = "domain, guest-agent"
			}, false),
			Entry("another network reported an address", func(vmi *virtv1.VirtualMachineInstance) {
				vmi.Status.Interfaces = append(
					vmi.Status.Interfaces,
					virtv1.VirtualMachineInstanceNetworkInterface{Name: "secondary", IP: "10.9.9.9"},
				)
			}, false),
		)

		// A guest that comes back on a different address has to republish
		// spec.bmcIP, so the move itself has to reach the reconciler.
		It("enqueues when the default interface moved to a new address", func() {
			published := watchedVMI()
			published.Status.Interfaces[0].IP = "10.1.2.3"
			moved := published.DeepCopy()
			moved.ResourceVersion = "3"
			moved.Status.Interfaces[0].IP = "10.4.5.6"
			Expect(vmiWatchPredicate().Update(event.UpdateEvent{ObjectOld: published, ObjectNew: moved})).To(BeTrue(),
				"Update() = false for a guest that moved address")
		})
	})

	Describe("Secret", func() {
		It("enqueues Secret create and delete", func() {
			pred := secretWatchPredicate()
			Expect(pred.Create(event.CreateEvent{Object: fleetSecret()})).To(BeTrue(), "Secret create should enqueue")
			Expect(pred.Delete(event.DeleteEvent{Object: fleetSecret()})).To(BeTrue(), "Secret delete should enqueue")
		})

		DescribeTable("filters updates",
			func(test watchUpdate[*corev1.Secret], enqueue bool) {
				oldObj, newObj := test.oldObj, test.newObj
				if test.mutate != nil {
					oldSecret := fleetSecret()
					newSecret := oldSecret.DeepCopy()
					newSecret.ResourceVersion = "2"
					test.mutate(newSecret)
					oldObj, newObj = oldSecret, newSecret
				}
				got := secretWatchPredicate().Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
				Expect(got).To(Equal(enqueue))
			},
			Entry("nil old object", watchUpdate[*corev1.Secret]{newObj: fleetSecret()}, false),
			Entry("nil new object", watchUpdate[*corev1.Secret]{oldObj: fleetSecret()}, false),
			Entry("wrong old object type",
				watchUpdate[*corev1.Secret]{oldObj: &provisioningv1.DPU{}, newObj: &provisioningv1.DPU{}}, false),
			Entry("wrong new object type",
				watchUpdate[*corev1.Secret]{oldObj: fleetSecret(), newObj: &provisioningv1.DPU{}}, false),
			Entry("data only", watchUpdate[*corev1.Secret]{mutate: func(secret *corev1.Secret) {
				secret.Data["userdata"] = []byte("#cloud-config\nhostname: mutated\n")
			}}, false),
			Entry("annotation only", watchUpdate[*corev1.Secret]{mutate: func(secret *corev1.Secret) {
				secret.Annotations = map[string]string{"example.com/noise": "value"}
			}}, false),
			Entry("fleet label removed", watchUpdate[*corev1.Secret]{mutate: func(secret *corev1.Secret) {
				delete(secret.Labels, labelFleetUID)
			}}, true),
			Entry("controller owner changed", watchUpdate[*corev1.Secret]{mutate: func(secret *corev1.Secret) {
				secret.OwnerReferences[0].Name = "other-fleet"
			}}, true),
			Entry("deletion timestamp changed", watchUpdate[*corev1.Secret]{mutate: func(secret *corev1.Secret) {
				secret.DeletionTimestamp = ptr.To(metav1.Now())
			}}, true),
		)
	})

	Describe("SimInstance", func() {
		var oldInstance *scalev1alpha1.SimInstance

		BeforeEach(func() {
			fleet := rolloutFleet(3, 1, 3)
			oldInstance = fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated)
		})

		It("enqueues the fleet on SimInstance create and delete", func() {
			pred := simInstanceWatchPredicate()
			Expect(pred.Create(event.CreateEvent{Object: oldInstance})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: oldInstance})).To(BeTrue())
		})

		DescribeTable("filters updates",
			func(mutate func(*scalev1alpha1.SimInstance), enqueue bool) {
				newInstance := oldInstance.DeepCopy()
				mutate(newInstance)
				got := simInstanceWatchPredicate().Update(event.UpdateEvent{ObjectOld: oldInstance, ObjectNew: newInstance})
				Expect(got).To(Equal(enqueue))
			},
			Entry("no change", func(*scalev1alpha1.SimInstance) {}, false),
			Entry("annotation", func(i *scalev1alpha1.SimInstance) { i.Annotations = map[string]string{"a": "b"} }, false),
			Entry("teardown clock", func(i *scalev1alpha1.SimInstance) {
				now := metav1.Now()
				i.Status.TeardownStartedAt = &now
			}, false),
			Entry("phase", func(i *scalev1alpha1.SimInstance) { i.Status.Phase = scalev1alpha1.SimInstanceFailed }, true),
			Entry("dpu name", func(i *scalev1alpha1.SimInstance) { i.Status.DPUName = "dpu-0" }, true),
			Entry("condition", func(i *scalev1alpha1.SimInstance) {
				setInstanceCondition(i, scalev1alpha1.SimInstanceChildrenCreated, true, "Created", "")
			}, true),
			Entry("deletion", func(i *scalev1alpha1.SimInstance) {
				now := metav1.Now()
				i.DeletionTimestamp = &now
			}, true),
		)
	})
})
