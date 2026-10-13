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
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

var _ = Describe("SimInstance reconciler", func() {
	var (
		fleet    *scalev1alpha1.SimFleet
		instance *scalev1alpha1.SimInstance
	)

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "siminstance-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		fleet = virtualMachineFleet("converge", namespace.Name)
		Expect(k8sClient.Create(ctx, fleet)).To(Succeed())
		instance = newEnvtestInstance(fleet)
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())
	})

	It("converges every child of its ordinal under the instance", func() {
		startManager(false)

		Eventually(func(g Gomega) {
			current := &scalev1alpha1.SimInstance{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), current)).To(Succeed())
			g.Expect(current.Finalizers).To(ContainElement(simInstanceTeardownFinalizer))
			g.Expect(instanceConditionTrue(current, scalev1alpha1.SimInstanceChildrenCreated)).To(BeTrue(),
				"conditions = %+v", current.Status.Conditions)
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		current := &scalev1alpha1.SimInstance{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), current)).To(Succeed())
		for _, child := range ordinalChildren(fleet, 0) {
			key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
			Expect(k8sClient.Get(ctx, key, child.obj)).To(Succeed(), "get %T %s", child.obj, child.name)
			Expect(controlledBy(child.obj, scalev1alpha1.GroupVersion.String(), "SimInstance", current.Name, current.UID)).
				To(BeTrue(), "%T %s owner = %+v", child.obj, child.name, child.obj.GetOwnerReferences())
		}
	})

	// envtest runs no garbage collector, so every child that disappears was
	// deleted by the teardown finalizer. The instance is Ready first: the VMI
	// outlives its VirtualMachine here, and only the UIDs a Ready instance
	// recorded let teardown delete it.
	It("tears every child down before the deleted instance goes away", func() {
		startManager(false)

		vm := &virtv1.VirtualMachine{}
		vmKey := client.ObjectKey{Name: utils.VirtualMachineName(fleet.Name, 0), Namespace: fleet.Namespace}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, vmKey, vm)).To(Succeed())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		vmi := bootGuest(vm, "10.244.0.17")
		dpu := provisionDPU(fleet, 0)
		configureKubelet(dpu)
		joinSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      utils.KubeadmJoinSecretName(dpu.Name),
			Namespace: fleet.Namespace,
		}}
		Expect(k8sClient.Create(ctx, joinSecret)).To(Succeed())
		Eventually(func(g Gomega) {
			current := &scalev1alpha1.SimInstance{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), current)).To(Succeed())
			g.Expect(current.Status.Phase).To(Equal(scalev1alpha1.SimInstanceReady),
				"conditions = %+v", current.Status.Conditions)
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		children := ordinalChildren(fleet, 0)
		for _, obj := range []client.Object{vmi, dpu, joinSecret} {
			children = append(children, ordinalChild{obj.GetName(), obj})
		}
		for _, child := range children {
			key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
			Expect(k8sClient.Get(ctx, key, child.obj)).To(Succeed(), "get %T %s", child.obj, child.name)
		}

		Expect(k8sClient.Delete(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega) {
			for _, child := range children {
				key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
				err := k8sClient.Get(ctx, key, child.obj.DeepCopyObject().(client.Object))
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s still exists: %v", child.obj, child.name, err)
			}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), &scalev1alpha1.SimInstance{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "SimInstance still exists: %v", err)
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})

var _ = Describe("SimInstance spec admission", func() {
	var instance *scalev1alpha1.SimInstance

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "siminstance-admission-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		fleet := virtualMachineFleet("admission", namespace.Name)
		Expect(k8sClient.Create(ctx, fleet)).To(Succeed())
		instance = newEnvtestInstance(fleet)
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())
	})

	DescribeTable("rejects a change to the instance identity",
		func(mutate func(*scalev1alpha1.SimInstanceSpec), message string) {
			current := &scalev1alpha1.SimInstance{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), current)).To(Succeed())
			mutate(&current.Spec)

			err := k8sClient.Update(ctx, current)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "Update() error = %v, want Invalid", err)
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("spec.ordinal", func(spec *scalev1alpha1.SimInstanceSpec) { spec.Ordinal = 1 }, "ordinal is immutable"),
		Entry("spec.hostIndex", func(spec *scalev1alpha1.SimInstanceSpec) { spec.HostIndex = 1 }, "hostIndex is immutable"),
	)
})

// newEnvtestInstance is ordinal 0 of fleet, controlled by it.
func newEnvtestInstance(fleet *scalev1alpha1.SimFleet) *scalev1alpha1.SimInstance {
	GinkgoHelper()
	instance := &scalev1alpha1.SimInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.InstanceName(fleet.Name, 0),
			Namespace: fleet.Namespace,
		},
		Spec: scalev1alpha1.SimInstanceSpec{Ordinal: 0, HostIndex: 0},
	}
	Expect(controllerutil.SetControllerReference(fleet, instance, scheme.Scheme)).To(Succeed())
	return instance
}

// configureKubelet stands in for the DPU agent: it reports the kubelet
// configured on dpu's status.
func configureKubelet(dpu *provisioningv1.DPU) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dpu), dpu)).To(Succeed())
	dpu.Status.AgentStatus = &provisioningv1.AgentStatus{}
	meta.SetStatusCondition(&dpu.Status.AgentStatus.Conditions, metav1.Condition{
		Type:    "KubeletConfigured",
		Status:  metav1.ConditionTrue,
		Reason:  "Configured",
		Message: "kubelet is configured",
	})
	Expect(k8sClient.Status().Update(ctx, dpu)).To(Succeed())
}

// bootGuest stands in for KubeVirt, which envtest does not run: it creates
// the VMI the VirtualMachine would run, with a bridged address and Ready.
// The VMI CRD has no status subresource, so the status is written on create.
func bootGuest(vm *virtv1.VirtualMachine, address string) *virtv1.VirtualMachineInstance {
	GinkgoHelper()
	vmi := &virtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vm.Name,
			Namespace: vm.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: virtv1.GroupVersion.String(),
				Kind:       "VirtualMachine",
				Name:       vm.Name,
				UID:        vm.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: virtv1.VirtualMachineInstanceSpec{Domain: virtv1.DomainSpec{Devices: virtv1.Devices{}}},
		Status: virtv1.VirtualMachineInstanceStatus{
			Phase:      virtv1.Running,
			Interfaces: []virtv1.VirtualMachineInstanceNetworkInterface{{Name: bridgedNetworkName, IP: address}},
			Conditions: []virtv1.VirtualMachineInstanceCondition{{
				Type:   virtv1.VirtualMachineInstanceReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	Expect(k8sClient.Create(ctx, vmi)).To(Succeed())
	return vmi
}

// provisionDPU stands in for DPF: it creates the DPU its DPUSet would for the
// ordinal's device, with the kubelet not configured yet.
func provisionDPU(fleet *scalev1alpha1.SimFleet, ordinal int) *provisioningv1.DPU {
	GinkgoHelper()
	dpu := &provisioningv1.DPU{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, ordinal),
			Namespace: fleet.Namespace,
		},
		Spec: provisioningv1.DPUSpec{
			DPUNodeName:   utils.NodeName(fleet.Name, utils.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode)),
			DPUDeviceName: utils.DeviceName(fleet.Name, ordinal),
			BFB:           ptr.To("bfb"),
			SerialNumber:  utils.SerialNumber(fleet.Name, ordinal),
			NodeEffect:    provisioningv1.NodeEffect{Action: provisioningv1.Action{NoEffect: ptr.To(true)}},
			DPUFlavor:     "flavor",
		},
	}
	Expect(k8sClient.Create(ctx, dpu)).To(Succeed())
	return dpu
}

var _ = Describe("SimInstance readiness", func() {
	var fleet *scalev1alpha1.SimFleet

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "siminstance-ready-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		fleet = virtualMachineFleet("ready", namespace.Name)
		Expect(k8sClient.Create(ctx, fleet)).To(Succeed())
	})

	It("reaches Ready on the DPU kubelet status update and is replaced when its VM goes", func() {
		startFleetManager()

		vm := &virtv1.VirtualMachine{}
		vmKey := client.ObjectKey{Name: utils.VirtualMachineName(fleet.Name, 0), Namespace: fleet.Namespace}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, vmKey, vm)).To(Succeed())
		}, time.Minute, 100*time.Millisecond).Should(Succeed())
		vmi := bootGuest(vm, "10.244.0.17")
		dpu := provisionDPU(fleet, 0)

		Eventually(func(g Gomega) {
			instance := getFleetInstance(g, fleet, 0)
			g.Expect(instance.Status.DPUName).To(Equal(dpu.Name))
			g.Expect(instanceConditionTrue(instance, scalev1alpha1.SimInstanceSelectable)).To(BeTrue(),
				"conditions = %+v", instance.Status.Conditions)
			g.Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceProvisioning))
		}, time.Minute, 100*time.Millisecond).Should(Succeed())

		// Only a status update changes KubeletConfigured, and nothing else
		// requeues the instance, so Ready proves the DPU watch passes it.
		configureKubelet(dpu)

		var ready *scalev1alpha1.SimInstance
		Eventually(func(g Gomega) {
			ready = getFleetInstance(g, fleet, 0)
			g.Expect(ready.Status.Phase).To(Equal(scalev1alpha1.SimInstanceReady),
				"conditions = %+v", ready.Status.Conditions)
		}, time.Minute, 100*time.Millisecond).Should(Succeed())
		device := &provisioningv1.DPUDevice{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: utils.DeviceName(fleet.Name, 0), Namespace: fleet.Namespace,
		}, device)).To(Succeed())
		Expect(ready.Status.ObservedUIDs).To(Equal(scalev1alpha1.ObservedUIDs{
			DPU:                    string(dpu.UID),
			DPUDevice:              string(device.UID),
			VirtualMachine:         string(vm.UID),
			VirtualMachineInstance: string(vmi.UID),
		}))

		// The Failed phase lasts only until the fleet deletes the instance,
		// so it is caught on a watch rather than by polling.
		watcher, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		events, err := watcher.Watch(ctx, &scalev1alpha1.SimInstanceList{},
			client.InNamespace(fleet.Namespace),
			client.MatchingFields{"metadata.name": ready.Name},
		)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(events.Stop)

		// envtest runs no KubeVirt and no garbage collector, so the VMI
		// outlives its VirtualMachine here; the instance's teardown deletes
		// it through the UIDs it recorded.
		removeWithoutFinalizers(vm)

		Eventually(events.ResultChan(), time.Minute).Should(Receive(Satisfy(func(event watch.Event) bool {
			current, ok := event.Object.(*scalev1alpha1.SimInstance)
			return ok && current.UID == ready.UID &&
				current.Status.Phase == scalev1alpha1.SimInstanceFailed &&
				current.Status.FailureReason == "VirtualMachineReplaced"
		})))
		Eventually(func(g Gomega) {
			replacement := getFleetInstance(g, fleet, 0)
			g.Expect(replacement.UID).NotTo(Equal(ready.UID))
			g.Expect(instanceConditionTrue(replacement, scalev1alpha1.SimInstanceChildrenCreated)).To(BeTrue(),
				"conditions = %+v", replacement.Status.Conditions)
		}, time.Minute, 100*time.Millisecond).Should(Succeed())
	})
})
