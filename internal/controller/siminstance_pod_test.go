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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/pod"
)

// podInstanceFleet is instanceFleet with a Pod where the VirtualMachine was.
func podInstanceFleet() *scalev1alpha1.SimFleet {
	fleet := instanceFleet()
	fleet.Spec.VirtualMachineTemplate = nil
	fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{Image: "simfleet-node:test"}
	return fleet
}

// noKubeVirtScheme is the scheme of a cluster without KubeVirt: reading a
// KubeVirt object through it fails with a not-registered error.
func noKubeVirtScheme() *runtime.Scheme {
	GinkgoHelper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		scalev1alpha1.AddToScheme,
		provisioningv1.AddToScheme,
	} {
		Expect(add(scheme)).To(Succeed(), "build scheme")
	}
	return scheme
}

func newPodInstanceHarness(fleet *scalev1alpha1.SimFleet, objects ...client.Object) *instanceHarness {
	GinkgoHelper()
	return newInstanceHarnessOn(noKubeVirtScheme(), false, fleet, objects...)
}

func (h *instanceHarness) guestPod() *corev1.Pod {
	GinkgoHelper()
	guest := &corev1.Pod{}
	Expect(h.get(naming.GuestName(h.fleet.Name, instanceOrdinal), guest)).To(BeTrue(), "Pod was not created")
	return guest
}

// reportPod stands in for the kubelet: it sets the Pod's addresses and its
// Ready condition.
func (h *instanceHarness) reportPod(address string, ready bool) {
	GinkgoHelper()
	guest := h.guestPod()
	guest.Status.PodIPs = nil
	if address != "" {
		guest.Status.PodIPs = []corev1.PodIP{{IP: address}}
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	guest.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	Expect(h.client.Status().Update(context.Background(), guest)).To(Succeed(), "update Pod status")
}

func ownedByInstance(fleet *scalev1alpha1.SimFleet) []metav1.OwnerReference {
	instance := newInstanceFixture(fleet, instanceOrdinal)
	return []metav1.OwnerReference{{
		APIVersion:         scalev1alpha1.GroupVersion.String(),
		Kind:               "SimInstance",
		Name:               instance.Name,
		UID:                instance.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// seededPod is the Pod of a converged instance, running at address.
func seededPod(fleet *scalev1alpha1.SimFleet, address string, ready bool) *corev1.Pod {
	guest := pod.DesiredPod(fleet, instanceOrdinal)
	guest.UID = types.UID(recordedPodUID)
	guest.OwnerReferences = ownedByInstance(fleet)
	if address != "" {
		guest.Status.PodIPs = []corev1.PodIP{{IP: address}}
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	guest.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return guest
}

// newReadyPodChildren are the children a Ready pod instance holds.
func newReadyPodChildren(fleet *scalev1alpha1.SimFleet) *readyChildren {
	// The credentials, device and DPU do not depend on the guest template, so
	// they are built from the VirtualMachine form of the fleet.
	vmFleet := fleet.DeepCopy()
	vmFleet.Spec.PodTemplate = nil
	vmFleet.Spec.VirtualMachineTemplate = &scalev1alpha1.VirtualMachineTemplate{Image: "simfleet-node:test"}
	children := newReadyChildren(vmFleet)
	children.vm, children.vmi, children.cloudInit = nil, nil, nil
	children.guestSecret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:            naming.GuestSecretName(fleet.Name, instanceOrdinal),
		Namespace:       fleet.Namespace,
		Labels:          pod.GuestSecretLabels(fleet, instanceOrdinal),
		OwnerReferences: ownedByInstance(fleet),
	}}
	children.pod = seededPod(fleet, "10.244.0.17", true)
	return children
}

var _ = Describe("SimInstance reconcile of a pod fleet (fake client)", func() {
	It("creates the guest Secret and credentials first, then the Pod, all owned by the instance", func() {
		h := newPodInstanceHarness(podInstanceFleet())
		h.reconcile()

		secret := &corev1.Secret{}
		Expect(h.get(naming.GuestSecretName(h.fleet.Name, instanceOrdinal), secret)).To(BeTrue(), "guest Secret was not created")
		Expect(controlledByInstance(secret, h.instance)).To(BeTrue(), "Secret owner = %+v", secret.OwnerReferences)
		Expect(secret.Labels).To(HaveKeyWithValue(naming.LabelFleetName, h.fleet.Name))
		Expect(secret.Data).To(HaveKey("identity.env"))
		Expect(secret.Data).To(HaveKey("config.yaml"))
		Expect(secret.Data).To(HaveKey("mgmt.kubeconfig"))
		Expect(h.get(naming.CloudInitSecretName(h.fleet.Name, instanceOrdinal), &corev1.Secret{})).To(BeFalse(),
			"a pod fleet has no cloud-init Secret")
		Expect(h.issuer.n).To(Equal(1), "token issue count")
		name := dpuagent.CredentialName(h.fleet, instanceOrdinal)
		for _, obj := range []client.Object{&corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}} {
			Expect(h.get(name, obj)).To(BeTrue(), "%T %q was not created", obj, name)
		}

		guest := h.guestPod()
		Expect(controlledByInstance(guest, h.instance)).To(BeTrue(), "Pod owner = %+v", guest.OwnerReferences)
		Expect(guest.Spec.Containers[0].Image).To(Equal("simfleet-node:test"))
		requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "Ready")
		requireCondition(h.current(), scalev1alpha1.SimInstanceChildrenCreated, metav1.ConditionTrue, "Created")

		h.reconcile()
		Expect(h.issuer.n).To(Equal(1), "second Reconcile issued another token")
	})

	It("creates no Pod while the guest Secret cannot be written", func() {
		h := newPodInstanceHarness(podInstanceFleet())
		h.issuer.err = errors.New("token API unavailable")

		_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.instance)})

		Expect(err).To(HaveOccurred(), "Reconcile() error = nil, want the token failure")
		Expect(h.get(naming.GuestName(h.fleet.Name, instanceOrdinal), &corev1.Pod{})).To(BeFalse(),
			"Pod exists without its guest Secret")
		requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionFalse, "")
	})

	It("holds only the identity in the guest Secret and issues no token when the agent is disabled", func() {
		fleet := podInstanceFleet()
		fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(false)}
		h := newPodInstanceHarness(fleet)

		h.reconcile()

		secret := &corev1.Secret{}
		Expect(h.get(naming.GuestSecretName(fleet.Name, instanceOrdinal), secret)).To(BeTrue(), "guest Secret was not created")
		Expect(secret.Data).To(HaveLen(1))
		Expect(secret.Data).To(HaveKey("identity.env"))
		Expect(h.issuer.n).To(BeZero(), "a disabled agent needs no token")
		Expect(h.guestPod().Spec.Containers[0].ReadinessProbe).To(BeNil())
		requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "NotRequired")
	})

	It("waits for the Pod to be ready, reporting WaitingForPod, then reports Ready", func() {
		h := newPodInstanceHarness(podInstanceFleet())
		h.reconcile()
		requireCondition(h.current(), scalev1alpha1.SimInstanceGuestReady, metav1.ConditionFalse, "WaitingForPod")
		requireCondition(h.current(), scalev1alpha1.SimInstanceSelectable, metav1.ConditionFalse, "WaitingForGuestAddress")

		h.reportPod("10.244.0.17", false)
		h.reconcile()
		requireCondition(h.current(), scalev1alpha1.SimInstanceGuestReady, metav1.ConditionFalse, "WaitingForPod")

		h.reportPod("10.244.0.17", true)
		h.reconcile()
		requireCondition(h.current(), scalev1alpha1.SimInstanceGuestReady, metav1.ConditionTrue, "Ready")
	})

	It("publishes the Pod address as soon as it has an IPv4, before the Pod is Ready", func() {
		h := newPodInstanceHarness(podInstanceFleet())
		h.reconcile()
		Expect(h.device().Spec.BMCIP).To(BeNil(), "no address yet")

		h.reportPod("fd00::17", false)
		h.reconcile()
		Expect(h.device().Spec.BMCIP).To(BeNil(), "an IPv6 address is not published")

		h.reportPod("10.244.0.17", false)
		h.reconcile()

		device := h.device()
		Expect(device.Spec.BMCIP).To(HaveValue(Equal("10.244.0.17")))
		for key, want := range h.fleet.Spec.DeviceSelectionLabels {
			Expect(device.Labels).To(HaveKeyWithValue(key, want), "DPUDevice selection label %s", key)
		}
		requireCondition(h.current(), scalev1alpha1.SimInstanceSelectable, metav1.ConditionTrue, "Published")
		requireCondition(h.current(), scalev1alpha1.SimInstanceGuestReady, metav1.ConditionFalse, "WaitingForPod")
	})

	It("replaces the published address when the Pod comes back with another", func() {
		h := newPodInstanceHarness(podInstanceFleet())
		h.reconcile()
		h.reportPod("10.244.0.17", true)
		h.reconcile()
		Expect(h.device().Spec.BMCIP).To(HaveValue(Equal("10.244.0.17")))

		h.reportPod("10.244.0.99", true)
		h.reconcile()

		Expect(h.device().Spec.BMCIP).To(HaveValue(Equal("10.244.0.99")))
	})

	It("does not publish the address of a Pod this instance does not control", func() {
		fleet := podInstanceFleet()
		foreign := seededPod(fleet, "10.244.0.66", true)
		foreign.OwnerReferences = foreignOwner()
		h := newPodInstanceHarness(fleet, foreign)

		h.reconcile()

		Expect(h.current().Status.Phase).To(Equal(scalev1alpha1.SimInstanceFailed))
		Expect(h.current().Status.FailureReason).To(Equal("OwnershipConflict"))
		Expect(h.device().Spec.BMCIP).To(BeNil(), "the foreign Pod's address was published")
		Expect(h.guestPod().OwnerReferences).To(Equal(foreignOwner()), "the foreign Pod was modified")
	})

	It("does not rewrite the spec of an existing Pod but repairs its identity labels", func() {
		fleet := podInstanceFleet()
		existing := seededPod(fleet, "10.244.0.17", true)
		delete(existing.Labels, naming.LabelOrdinal)
		existing.Spec.Containers[0].Image = "someone-edited:v2"
		h := newPodInstanceHarness(fleet, existing)

		h.reconcile()

		guest := h.guestPod()
		Expect(guest.Labels).To(HaveKeyWithValue(naming.LabelOrdinal, "000012"))
		Expect(guest.Spec.Containers[0].Image).To(Equal("someone-edited:v2"))
	})

	It("reaches Ready and records the Pod UID, with no VirtualMachine UIDs", func() {
		fleet := podInstanceFleet()
		guest := seededPod(fleet, "10.244.0.17", true)
		h := newPodInstanceHarness(fleet, guest)
		dpu := h.createDPU(dpuagent.CredentialName(fleet, instanceOrdinal), true)

		h.reconcile()

		instance := h.current()
		Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceReady), "conditions = %+v", instance.Status.Conditions)
		Expect(instance.Status.ObservedUIDs).To(Equal(scalev1alpha1.ObservedUIDs{
			DPU:       string(dpu.UID),
			DPUDevice: string(h.device().UID),
			Pod:       recordedPodUID,
		}))
	})
})

var _ = Describe("SimInstance reconcile without KubeVirt (fake client)", func() {
	It("reports KubeVirtNotInstalled for a VirtualMachine fleet and creates no VirtualMachine", func() {
		h := newInstanceHarnessOn(noKubeVirtScheme(), false, instanceFleet())

		_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.instance)})

		Expect(err).To(MatchError(kubevirt.ErrNotInstalled))
		requireCondition(h.current(), scalev1alpha1.SimInstanceChildrenCreated, metav1.ConditionFalse, "KubeVirtNotInstalled")
		Expect(h.get(naming.CloudInitSecretName(h.fleet.Name, instanceOrdinal), &corev1.Secret{})).To(BeFalse(),
			"no credential is issued for a guest that cannot be created")
		Expect(h.issuer.n).To(BeZero())
	})

	It("does not look for recorded VirtualMachines when it checks a Ready instance for drift", func() {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.vm, children.vmi = nil, nil
		th := newReadyInstanceHarnessOn(noKubeVirtScheme(), false, fleet, children, recordedUIDs)

		// The converge that follows fails on the missing KubeVirt, which is
		// not what this test is about.
		_, _ = th.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(th.instance)})

		Expect(th.current().Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed))
		Expect(th.current().Status.FailureReason).To(BeEmpty())
	})
})

var _ = Describe("SimInstance teardown of a pod fleet (fake client)", func() {
	It("deletes the Pod where a VirtualMachine fleet deletes the VirtualMachine, without polling", func() {
		fleet := podInstanceFleet()
		children := newReadyPodChildren(fleet)
		th := newReadyPodInstanceHarness(fleet, children)
		var round []string
		th.reconciler.Client = interceptor.NewClient(th.client.(client.WithWatch), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				round = append(round, deleteRecord(obj))
				return nil
			},
		})
		th.deleteInstance()

		want := []struct {
			deleted []string
			requeue ctrl.Result
		}{
			{[]string{deleteRecord(children.dpu), deleteRecord(children.joinSecret)}, ctrl.Result{RequeueAfter: lifecycleForceTimeout}},
			{[]string{deleteRecord(children.device)}, ctrl.Result{RequeueAfter: lifecycleForceTimeout}},
			{[]string{deleteRecord(children.pod)}, ctrl.Result{RequeueAfter: lifecycleForceTimeout}},
			{[]string{
				deleteRecord(children.guestSecret), deleteRecord(children.roleBinding),
				deleteRecord(children.role), deleteRecord(children.serviceAccount),
			}, ctrl.Result{RequeueAfter: lifecycleForceTimeout}},
			{nil, ctrl.Result{}},
		}
		for i, wantRound := range want {
			round = nil
			result := th.reconcileResult()
			Expect(round).To(Equal(wantRound.deleted), "Reconcile %d deleted", i+1)
			Expect(result).To(Equal(wantRound.requeue), "Reconcile %d result", i+1)
		}
		Expect(th.instanceGone()).To(BeTrue())
	})

	It("leaves a Pod this instance does not control", func() {
		fleet := podInstanceFleet()
		children := newReadyPodChildren(fleet)
		children.pod.OwnerReferences = foreignOwner()
		th := newReadyPodInstanceHarness(fleet, children)
		th.deleteInstance()

		th.runTeardown()

		Expect(th.exists(children.pod)).To(BeTrue(), "a foreign Pod was deleted")
	})

	It("deletes by name whichever guest kind a deleted fleet left behind", func() {
		fleet := podInstanceFleet()
		children := newReadyPodChildren(fleet)
		th := newReadyPodInstanceHarness(fleet, children)
		th.deleteInstance()
		// The fleet is gone: only the instance's label and spec name its children.
		Expect(th.client.Delete(context.Background(), fleet)).To(Succeed())

		th.runTeardown()

		for _, obj := range children.objects() {
			Expect(th.exists(obj)).To(BeFalse(), "%T %s still exists", obj, obj.GetName())
		}
	})

	It("tears a VirtualMachine fleet down without a KubeVirt step when KubeVirt is absent", func() {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.vm, children.vmi = nil, nil
		th := newReadyInstanceHarnessOn(noKubeVirtScheme(), false, fleet, children, recordedUIDs)
		th.deleteInstance()

		th.runTeardown()

		Expect(th.instanceGone()).To(BeTrue())
		Expect(th.exists(children.device)).To(BeFalse())
	})
})

var _ = Describe("SimInstance drift of a pod fleet (fake client)", func() {
	DescribeTable("fails the instance when a recorded Pod was replaced",
		func(mutate func(*readyChildren), reason string) {
			fleet := podInstanceFleet()
			children := newReadyPodChildren(fleet)
			mutate(children)
			th := newReadyPodInstanceHarness(fleet, children)

			th.reconcile()

			Expect(th.current().Status.Phase).To(Equal(scalev1alpha1.SimInstanceFailed))
			Expect(th.current().Status.FailureReason).To(Equal(reason))
		},
		Entry("a Pod with another UID", func(c *readyChildren) { c.pod.UID = types.UID("new-pod-uid") }, "PodReplaced"),
		Entry("a missing Pod", func(c *readyChildren) { c.pod = nil }, "PodReplaced"),
	)

	It("recorded UIDs that still match are not drift", func() {
		fleet := podInstanceFleet()
		th := newReadyPodInstanceHarness(fleet, newReadyPodChildren(fleet))

		th.reconcile()

		Expect(th.current().Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed))
		Expect(th.current().Status.FailureReason).To(BeEmpty())
	})

	// The manager's cache holds a Pod only while it carries the fleet-name
	// label. These cases model a Pod the cache cannot see: the cached client
	// reports it NotFound while the API reader still has it.
	Describe("a Pod missing from the label-scoped cache", func() {
		cacheMissDrift := func(mutate func(*readyChildren)) (string, error) {
			fleet := podInstanceFleet()
			children := newReadyPodChildren(fleet)
			mutate(children)
			th := newReadyPodInstanceHarness(fleet, children)
			th.reconciler.Client = interceptor.NewClient(th.client.(client.WithWatch), interceptor.Funcs{
				Get: func(
					ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
				) error {
					if _, isPod := obj.(*corev1.Pod); isPod {
						return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			return th.reconciler.detectDrift(context.Background(), fleet, th.current())
		}

		It("is not drift when the live Pod has the recorded UID but lost the fleet-name label", func() {
			reason, err := cacheMissDrift(func(c *readyChildren) { delete(c.pod.Labels, naming.LabelFleetName) })

			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(BeEmpty())
		})

		It("stays PodReplaced when the live Pod has another UID", func() {
			reason, err := cacheMissDrift(func(c *readyChildren) {
				delete(c.pod.Labels, naming.LabelFleetName)
				c.pod.UID = types.UID("new-pod-uid")
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal("PodReplaced"))
		})

		It("stays PodReplaced when the Pod is gone from the API server", func() {
			reason, err := cacheMissDrift(func(c *readyChildren) { c.pod = nil })

			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal("PodReplaced"))
		})
	})
})

// newReadyPodInstanceHarness is newReadyInstanceHarness for a pod fleet on a
// cluster without KubeVirt.
func newReadyPodInstanceHarness(fleet *scalev1alpha1.SimFleet, children *readyChildren) *teardownHarness {
	GinkgoHelper()
	return newReadyInstanceHarnessOn(noKubeVirtScheme(), false, fleet, children, scalev1alpha1.ObservedUIDs{
		DPU:       recordedUIDs.DPU,
		DPUDevice: recordedUIDs.DPUDevice,
		Pod:       recordedPodUID,
	})
}
