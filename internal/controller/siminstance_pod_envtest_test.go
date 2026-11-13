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
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/ownership"
)

func podFleetFor(name, namespace string) *scalev1alpha1.SimFleet {
	fleet := virtualMachineFleet(name, namespace)
	fleet.Spec.VirtualMachineTemplate = nil
	fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{Image: "example.com/node@sha256:test"}
	return fleet
}

// podChildren are the objects a SimInstance creates for a pod fleet's ordinal.
func podChildren(fleet *scalev1alpha1.SimFleet, ordinal int) []ordinalChild {
	credentialName := dpuagent.CredentialName(fleet, ordinal)
	return []ordinalChild{
		{credentialName, &corev1.ServiceAccount{}},
		{credentialName, &rbacv1.Role{}},
		{credentialName, &rbacv1.RoleBinding{}},
		{naming.GuestSecretName(fleet.Name, ordinal), &corev1.Secret{}},
		{naming.DeviceName(fleet.Name, ordinal), &provisioningv1.DPUDevice{}},
		{naming.GuestName(fleet.Name, ordinal), &corev1.Pod{}},
	}
}

// runPod stands in for the kubelet, which envtest does not run: it reports
// the Pod's address, and its Ready condition when ready is set.
func runPod(guest *corev1.Pod, address string, ready bool) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(guest), guest)).To(Succeed())
	guest.Status.Phase = corev1.PodRunning
	guest.Status.PodIP = address
	guest.Status.PodIPs = []corev1.PodIP{{IP: address}}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	guest.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	Expect(k8sClient.Status().Update(ctx, guest)).To(Succeed())
}

var _ = Describe("SimInstance pod guest", func() {
	var fleet *scalev1alpha1.SimFleet

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "siminstance-pod-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		fleet = podFleetFor("pods", namespace.Name)
		Expect(k8sClient.Create(ctx, fleet)).To(Succeed())
	})

	It("converges a Pod, publishes its address before it is Ready, and tears it down", func() {
		startManagerWithKubeVirt(false, false)
		instance := newEnvtestInstance(fleet)
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())

		guest := &corev1.Pod{}
		guestKey := client.ObjectKey{Name: naming.GuestName(fleet.Name, 0), Namespace: fleet.Namespace}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, guestKey, guest)).To(Succeed())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		current := &scalev1alpha1.SimInstance{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), current)).To(Succeed())
		for _, child := range podChildren(fleet, 0) {
			key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
			Eventually(func() error { return k8sClient.Get(ctx, key, child.obj) }, 30*time.Second, 100*time.Millisecond).
				Should(Succeed(), "get %T %s", child.obj, child.name)
			Expect(ownership.ControlledBy(child.obj, scalev1alpha1.GroupVersion.String(), "SimInstance", current.Name, current.UID)).
				To(BeTrue(), "%T %s owner = %+v", child.obj, child.name, child.obj.GetOwnerReferences())
		}
		Expect(guest.Spec.Containers[0].SecurityContext.Privileged).To(HaveValue(BeTrue()))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: naming.GuestSecretName(fleet.Name, 0), Namespace: fleet.Namespace,
		}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey("identity.env"))
		Expect(secret.Data).To(HaveKey("mgmt.kubeconfig"))
		Expect(secret.Data).To(HaveKey("config.yaml"))

		// The address is published as soon as the Pod has one, not when it is Ready.
		runPod(guest, "10.244.0.17", false)
		Eventually(func(g Gomega) {
			device := &provisioningv1.DPUDevice{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: naming.DeviceName(fleet.Name, 0), Namespace: fleet.Namespace,
			}, device)).To(Succeed())
			g.Expect(device.Spec.BMCIP).To(HaveValue(Equal("10.244.0.17")))
			instance := getFleetInstance(g, fleet, 0)
			g.Expect(instanceConditionTrue(instance, scalev1alpha1.SimInstanceSelectable)).To(BeTrue(),
				"conditions = %+v", instance.Status.Conditions)
			g.Expect(instanceConditionTrue(instance, scalev1alpha1.SimInstanceGuestReady)).To(BeFalse())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		// Only the Ready condition changes here, so Ready proves the Pod watch passes it.
		runPod(guest, "10.244.0.17", true)
		dpu := provisionDPU(fleet, 0)
		configureKubelet(dpu)
		Eventually(func(g Gomega) {
			ready := getFleetInstance(g, fleet, 0)
			g.Expect(ready.Status.Phase).To(Equal(scalev1alpha1.SimInstanceReady), "conditions = %+v", ready.Status.Conditions)
			g.Expect(ready.Status.ObservedUIDs.Pod).To(Equal(string(guest.UID)))
			g.Expect(ready.Status.ObservedUIDs.VirtualMachine).To(BeEmpty())
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		Expect(k8sClient.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			for _, child := range podChildren(fleet, 0) {
				key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
				err := k8sClient.Get(ctx, key, child.obj.DeepCopyObject().(client.Object))
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s still exists: %v", child.obj, child.name, err)
			}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(instance), &scalev1alpha1.SimInstance{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "SimInstance still exists: %v", err)
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})

var _ = Describe("a cluster without KubeVirt", func() {
	It("converges a pod fleet and reports KubeVirtNotInstalled for a virtual machine fleet", func() {
		startManagerWithKubeVirt(true, false)

		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "no-kubevirt-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		podFleet := podFleetFor("pods", namespace.Name)
		vmFleet := virtualMachineFleet("vms", namespace.Name)
		Expect(k8sClient.Create(ctx, podFleet)).To(Succeed())
		Expect(k8sClient.Create(ctx, vmFleet)).To(Succeed())

		Eventually(func(g Gomega) {
			instance := getFleetInstance(g, podFleet, 0)
			g.Expect(instanceConditionTrue(instance, scalev1alpha1.SimInstanceChildrenCreated)).To(BeTrue(),
				"conditions = %+v", instance.Status.Conditions)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: naming.GuestName(podFleet.Name, 0), Namespace: namespace.Name,
			}, &corev1.Pod{})).To(Succeed())
		}, time.Minute, 100*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			current := &scalev1alpha1.SimFleet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vmFleet), current)).To(Succeed())
			guests := meta.FindStatusCondition(current.Status.Conditions, scalev1alpha1.ConditionGuestsReady)
			g.Expect(guests).NotTo(BeNil())
			g.Expect(guests.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(guests.Reason).To(Equal("KubeVirtNotInstalled"))
			bootstrap := meta.FindStatusCondition(current.Status.Conditions, scalev1alpha1.ConditionBootstrapReconciled)
			g.Expect(bootstrap).NotTo(BeNil())
			g.Expect(bootstrap.Reason).To(Equal("KubeVirtNotInstalled"))
			instance := getFleetInstance(g, vmFleet, 0)
			childrenCreated := meta.FindStatusCondition(instance.Status.Conditions, scalev1alpha1.SimInstanceChildrenCreated)
			g.Expect(childrenCreated).NotTo(BeNil(), "conditions = %+v", instance.Status.Conditions)
			g.Expect(childrenCreated.Reason).To(Equal("KubeVirtNotInstalled"))
		}, time.Minute, 100*time.Millisecond).Should(Succeed())

		vms := &virtv1.VirtualMachineList{}
		Expect(k8sClient.List(ctx, vms, client.InNamespace(namespace.Name))).To(Succeed())
		Expect(vms.Items).To(BeEmpty(), "a VirtualMachine was created without KubeVirt")
	})
})
