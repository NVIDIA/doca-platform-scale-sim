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
	"strings"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/cloudinit"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpf"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/ownership"
)

const instanceOrdinal = 12

// assignUIDs stands in for the API server, which the fake client does not:
// the token path only runs for a ServiceAccount that has a UID.
var assignUIDs = interceptor.Funcs{
	Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID(fmt.Sprintf("uid-%T-%s", obj, obj.GetName())))
		}
		return c.Create(ctx, obj, opts...)
	},
}

type instanceHarness struct {
	client     client.Client
	reconciler *SimInstanceReconciler
	issuer     *countingServiceAccountIssuer
	fleet      *scalev1alpha1.SimFleet
	instance   *scalev1alpha1.SimInstance
}

func newInstanceHarness(fleet *scalev1alpha1.SimFleet, objects ...client.Object) *instanceHarness {
	GinkgoHelper()
	scheme := instanceTestScheme()
	instance := newInstanceFixture(fleet, instanceOrdinal)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&scalev1alpha1.SimInstance{}).
		WithIndex(&provisioningv1.DPU{}, dpuDeviceField, func(object client.Object) []string {
			return []string{object.(*provisioningv1.DPU).Spec.DPUDeviceName}
		}).
		WithInterceptorFuncs(assignUIDs).
		WithObjects(append([]client.Object{fleet, instance}, objects...)...).
		Build()
	issuer := &countingServiceAccountIssuer{}
	return &instanceHarness{
		client: c,
		reconciler: &SimInstanceReconciler{
			Client:                    c,
			APIReader:                 c,
			Scheme:                    scheme,
			ServiceAccountTokenIssuer: issuer,
			ManagementCA:              []byte("test-ca"),
		},
		issuer:   issuer,
		fleet:    fleet,
		instance: instance,
	}
}

func (h *instanceHarness) reconcile() {
	GinkgoHelper()
	result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(h.instance),
	})
	Expect(err).NotTo(HaveOccurred(), "Reconcile()")
	Expect(result).To(Equal(ctrl.Result{}), "Reconcile() result")
}

func (h *instanceHarness) get(name string, obj client.Object) bool {
	GinkgoHelper()
	err := h.client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: h.fleet.Namespace}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	Expect(err).NotTo(HaveOccurred(), "get %T %q", obj, name)
	return true
}

func (h *instanceHarness) current() *scalev1alpha1.SimInstance {
	GinkgoHelper()
	instance := &scalev1alpha1.SimInstance{}
	Expect(h.get(h.instance.Name, instance)).To(BeTrue(), "SimInstance is gone")
	return instance
}

func (h *instanceHarness) device() *provisioningv1.DPUDevice {
	GinkgoHelper()
	device := &provisioningv1.DPUDevice{}
	Expect(h.get(naming.DeviceName(h.fleet.Name, instanceOrdinal), device)).To(BeTrue(), "DPUDevice was not created")
	return device
}

func (h *instanceHarness) virtualMachine() *virtv1.VirtualMachine {
	GinkgoHelper()
	vm := &virtv1.VirtualMachine{}
	Expect(h.get(naming.VirtualMachineName(h.fleet.Name, instanceOrdinal), vm)).To(BeTrue(), "VirtualMachine was not created")
	return vm
}

func (h *instanceHarness) create(obj client.Object) {
	GinkgoHelper()
	Expect(h.client.Create(context.Background(), obj)).To(Succeed(), "create %T %q", obj, obj.GetName())
}

func (h *instanceHarness) update(obj client.Object) {
	GinkgoHelper()
	Expect(h.client.Update(context.Background(), obj)).To(Succeed(), "update %T %q", obj, obj.GetName())
}

// countWrites counts every write the reconciler makes, leaving out writes to
// the SimInstance itself unless includeInstance is set.
func (h *instanceHarness) countWrites(includeInstance bool) *int {
	writes := 0
	count := func(obj client.Object) {
		if _, ok := obj.(*scalev1alpha1.SimInstance); includeInstance || !ok {
			writes++
		}
	}
	h.reconciler.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			count(obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			count(obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			count(obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			count(obj)
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResourceName string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			count(obj)
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(
			ctx context.Context,
			c client.Client,
			subResourceName string,
			obj client.Object,
			patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			count(obj)
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	})
	return &writes
}

// seededGuest is a ready VirtualMachine owned by the fixture instance and the
// VMI KubeVirt runs for it with a bridged address, carrying recordedUIDs.
// They are seeded rather than created: the fake client's deduced field
// manager cannot convert a VMI's status.
func seededGuest(fleet *scalev1alpha1.SimFleet, address string) (*virtv1.VirtualMachine, *virtv1.VirtualMachineInstance) {
	instance := newInstanceFixture(fleet, instanceOrdinal)
	vm := kubevirt.DesiredVirtualMachine(fleet, instanceOrdinal)
	vm.UID = types.UID(recordedUIDs.VirtualMachine)
	vm.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: scalev1alpha1.GroupVersion.String(),
		Kind:       "SimInstance",
		Name:       instance.Name,
		UID:        instance.UID,
		Controller: ptr.To(true),
	}}
	vm.Status.Ready = true
	vmi := &virtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vm.Name,
			Namespace: vm.Namespace,
			UID:       types.UID(recordedUIDs.VirtualMachineInstance),
			Labels:    vm.Spec.Template.ObjectMeta.Labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: virtv1.GroupVersion.String(),
				Kind:       "VirtualMachine",
				Name:       vm.Name,
				UID:        vm.UID,
				Controller: ptr.To(true),
			}},
		},
		Status: virtv1.VirtualMachineInstanceStatus{
			Interfaces: []virtv1.VirtualMachineInstanceNetworkInterface{{
				Name: kubevirt.BridgedNetworkName,
				IP:   address,
			}},
			Conditions: []virtv1.VirtualMachineInstanceCondition{{
				Type:   virtv1.VirtualMachineInstanceReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	return vm, vmi
}

func (h *instanceHarness) createDPU(name string, kubelet bool) *provisioningv1.DPU {
	GinkgoHelper()
	dpu := &provisioningv1.DPU{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.fleet.Namespace},
		Spec: provisioningv1.DPUSpec{
			DPUDeviceName: naming.DeviceName(h.fleet.Name, instanceOrdinal),
		},
	}
	if kubelet {
		dpu.Status.AgentStatus = &provisioningv1.AgentStatus{}
		meta.SetStatusCondition(&dpu.Status.AgentStatus.Conditions, metav1.Condition{
			Type:   "KubeletConfigured",
			Status: metav1.ConditionTrue,
			Reason: "Configured",
		})
	}
	h.create(dpu)
	return dpu
}

func controlledByInstance(obj client.Object, instance *scalev1alpha1.SimInstance) bool {
	return ownership.ControlledBy(obj, scalev1alpha1.GroupVersion.String(), "SimInstance", instance.Name, instance.UID)
}

func requireCondition(instance *scalev1alpha1.SimInstance, conditionType string, status metav1.ConditionStatus, reason string) {
	GinkgoHelper()
	condition := meta.FindStatusCondition(instance.Status.Conditions, conditionType)
	Expect(condition).NotTo(BeNil(), "condition %s missing; conditions = %+v", conditionType, instance.Status.Conditions)
	Expect(condition.Status).To(Equal(status), "condition %s = %s/%s (%s)",
		conditionType, condition.Status, condition.Reason, condition.Message)
	if reason != "" {
		Expect(condition.Reason).To(Equal(reason), "condition %s = %s/%s (%s)",
			conditionType, condition.Status, condition.Reason, condition.Message)
	}
	Expect(condition.ObservedGeneration).To(Equal(instance.Generation), "condition %s observedGeneration", conditionType)
}

// cacheWithout stands in for the manager cache in front of the fake client:
// Get reports NotFound for every object hidden returns true for.
func cacheWithout(c client.Client, hidden func(client.Object) bool) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if hidden(obj) {
				return apierrors.NewNotFound(schema.GroupResource{}, key.Name)
			}
			return nil
		},
	})
}

// labelScoped is what ManagerCacheOptions leaves out of the cache.
func labelScoped(obj client.Object) bool {
	_, labelled := obj.GetLabels()[naming.LabelFleetName]
	return labelScopedCacheKind(obj) && !labelled
}

var teardownClock = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

var recordedUIDs = scalev1alpha1.ObservedUIDs{
	DPU:                    "dpu-uid",
	DPUDevice:              "device-uid",
	VirtualMachine:         "vm-uid",
	VirtualMachineInstance: "vmi-uid",
}

// readyChildren are the children a Ready instance holds, plus the DPU and
// kubeadm join Secret DPF creates for its device. A nil field is absent.
type readyChildren struct {
	serviceAccount *corev1.ServiceAccount
	role           *rbacv1.Role
	roleBinding    *rbacv1.RoleBinding
	cloudInit      *corev1.Secret
	device         *provisioningv1.DPUDevice
	vm             *virtv1.VirtualMachine
	vmi            *virtv1.VirtualMachineInstance
	dpu            *provisioningv1.DPU
	joinSecret     *corev1.Secret
}

func newReadyChildren(fleet *scalev1alpha1.SimFleet) *readyChildren {
	instance := newInstanceFixture(fleet, instanceOrdinal)
	owned := []metav1.OwnerReference{{
		APIVersion:         scalev1alpha1.GroupVersion.String(),
		Kind:               "SimInstance",
		Name:               instance.Name,
		UID:                instance.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
	serviceAccount := dpuagent.DesiredServiceAccount(fleet, instanceOrdinal)
	serviceAccount.OwnerReferences = owned
	role := dpuagent.DesiredRole(fleet, instanceOrdinal)
	role.OwnerReferences = owned
	roleBinding := dpuagent.DesiredRoleBinding(fleet, instanceOrdinal)
	roleBinding.OwnerReferences = owned
	cloudInit := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:            naming.CloudInitSecretName(fleet.Name, instanceOrdinal),
		Namespace:       fleet.Namespace,
		Labels:          cloudinit.SecretLabels(fleet, instanceOrdinal),
		OwnerReferences: owned,
	}}
	device := dpf.DesiredDevice(fleet, instanceOrdinal)
	device.UID = types.UID(recordedUIDs.DPUDevice)
	device.OwnerReferences = owned
	device.Spec.BMCIP = ptr.To("10.244.0.17")
	mergeLabels(device, dpf.DeviceSelectionLabels(fleet))
	vm, vmi := seededGuest(fleet, "10.244.0.17")
	dpuName := naming.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, instanceOrdinal)
	return &readyChildren{
		serviceAccount: serviceAccount,
		role:           role,
		roleBinding:    roleBinding,
		cloudInit:      cloudInit,
		device:         device,
		vm:             vm,
		vmi:            vmi,
		dpu: &provisioningv1.DPU{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dpuName,
				Namespace: fleet.Namespace,
				UID:       types.UID(recordedUIDs.DPU),
			},
			Spec: provisioningv1.DPUSpec{DPUDeviceName: device.Name},
		},
		joinSecret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      naming.KubeadmJoinSecretName(dpuName),
			Namespace: fleet.Namespace,
		}},
	}
}

func (c *readyChildren) objects() []client.Object {
	var objects []client.Object
	add := func(obj client.Object, present bool) {
		if present {
			objects = append(objects, obj)
		}
	}
	add(c.serviceAccount, c.serviceAccount != nil)
	add(c.role, c.role != nil)
	add(c.roleBinding, c.roleBinding != nil)
	add(c.cloudInit, c.cloudInit != nil)
	add(c.device, c.device != nil)
	add(c.vm, c.vm != nil)
	add(c.vmi, c.vmi != nil)
	add(c.dpu, c.dpu != nil)
	add(c.joinSecret, c.joinSecret != nil)
	return objects
}

type teardownHarness struct {
	*instanceHarness
	now      time.Time
	recorder *events.FakeRecorder
}

// newReadyInstanceHarness is an instance that converged: it holds the
// teardown finalizer, the fleet-name label, and the recorded child UIDs.
func newReadyInstanceHarness(fleet *scalev1alpha1.SimFleet, children *readyChildren) *teardownHarness {
	GinkgoHelper()
	th := &teardownHarness{
		instanceHarness: newInstanceHarness(fleet, children.objects()...),
		now:             teardownClock,
		recorder:        events.NewFakeRecorder(100),
	}
	th.reconciler.Now = func() time.Time { return th.now }
	th.reconciler.Recorder = th.recorder

	instance := th.current()
	controllerutil.AddFinalizer(instance, scalev1alpha1.SimInstanceFinalizer)
	instance.Labels = map[string]string{naming.LabelFleetName: fleet.Name}
	th.update(instance)
	instance.Status.Phase = scalev1alpha1.SimInstanceReady
	instance.Status.ObservedUIDs = recordedUIDs
	Expect(th.client.Status().Update(context.Background(), instance)).To(Succeed(), "update SimInstance status")
	return th
}

func (th *teardownHarness) deleteInstance() {
	GinkgoHelper()
	Expect(th.client.Delete(context.Background(), th.current())).To(Succeed(), "delete SimInstance")
}

func (th *teardownHarness) reconcileResult() ctrl.Result {
	GinkgoHelper()
	result, err := th.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(th.instance),
	})
	Expect(err).NotTo(HaveOccurred(), "Reconcile()")
	return result
}

func (th *teardownHarness) instanceGone() bool {
	GinkgoHelper()
	return !th.get(th.instance.Name, &scalev1alpha1.SimInstance{})
}

// runTeardown reconciles until the finalizer is removed and the instance is
// gone.
func (th *teardownHarness) runTeardown() {
	GinkgoHelper()
	for range 12 {
		if th.instanceGone() {
			return
		}
		th.reconcileResult()
	}
	Fail(fmt.Sprintf("teardown did not finish; instance = %+v", th.current().Status))
}

func (th *teardownHarness) exists(obj client.Object) bool {
	GinkgoHelper()
	return th.get(obj.GetName(), obj.DeepCopyObject().(client.Object))
}

func (th *teardownHarness) events() []string {
	var got []string
	for {
		select {
		case event := <-th.recorder.Events:
			got = append(got, event)
		default:
			return got
		}
	}
}

func hasEvent(events []string, prefix string) bool {
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			return true
		}
	}
	return false
}

func deleteRecord(obj client.Object) string {
	return fmt.Sprintf("%T %s", obj, obj.GetName())
}

var _ = Describe("SimInstance reconcile (fake client)", func() {
	It("finalizer is added before any child exists", func() {
		h := newInstanceHarness(instanceFleet())
		var childSeenWithoutFinalizer bool
		creates := 0
		h.reconciler.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				creates++
				instance := &scalev1alpha1.SimInstance{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(h.instance), instance); err != nil {
					return err
				}
				if !controllerutil.ContainsFinalizer(instance, scalev1alpha1.SimInstanceFinalizer) {
					childSeenWithoutFinalizer = true
				}
				return c.Create(ctx, obj, opts...)
			},
		})

		h.reconcile()

		Expect(creates).NotTo(BeZero(), "Reconcile created no child, so the finalizer ordering was not exercised")
		Expect(childSeenWithoutFinalizer).To(BeFalse(), "a child was created before the teardown finalizer was persisted")
		Expect(h.current().Finalizers).To(ContainElement(scalev1alpha1.SimInstanceFinalizer))
	})

	It("an instance of a deleting fleet converges nothing", func() {
		fleet := instanceFleet()
		fleet.Finalizers = []string{scalev1alpha1.SimFleetFinalizer}
		now := metav1.Now()
		fleet.DeletionTimestamp = &now
		h := newInstanceHarness(fleet)
		writes := h.countWrites(true)

		h.reconcile()

		Expect(*writes).To(BeZero(), "reconciler wrote for an instance its fleet is about to delete")
	})

	It("credentials are owned by the instance and the cloud-init Secret is issued once", func() {
		h := newInstanceHarness(instanceFleet())
		h.reconcile()

		name := dpuagent.CredentialName(h.fleet, instanceOrdinal)
		for _, obj := range []client.Object{&corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}} {
			Expect(h.get(name, obj)).To(BeTrue(), "%T %q was not created", obj, name)
			Expect(controlledByInstance(obj, h.instance)).To(BeTrue(),
				"%T owner = %+v, want SimInstance %s", obj, obj.GetOwnerReferences(), h.instance.Name)
		}
		secret := &corev1.Secret{}
		Expect(h.get(naming.CloudInitSecretName(h.fleet.Name, instanceOrdinal), secret)).To(BeTrue(),
			"cloud-init Secret was not created")
		Expect(controlledByInstance(secret, h.instance)).To(BeTrue(),
			"Secret owner = %+v, want the SimInstance", secret.OwnerReferences)
		Expect(h.issuer.n).To(Equal(1), "token issue count")
		requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "")

		h.reconcile()
		Expect(h.issuer.n).To(Equal(1), "second Reconcile issued another token")
	})

	It("DPUDevice carries the identity labels and its spec is never rewritten", func() {
		h := newInstanceHarness(instanceFleet())
		h.reconcile()

		device := h.device()
		Expect(controlledByInstance(device, h.instance)).To(BeTrue(),
			"DPUDevice owner = %+v, want the SimInstance", device.OwnerReferences)
		for key, want := range dpf.DeviceLabels(h.fleet, instanceOrdinal) {
			Expect(device.Labels).To(HaveKeyWithValue(key, want), "DPUDevice label %s", key)
		}
		requireCondition(h.current(), scalev1alpha1.SimInstanceDeviceCreated, metav1.ConditionTrue, "")

		device.Spec.SerialNumber = "rewritten-by-dpf"
		delete(device.Labels, naming.LabelOrdinal)
		h.update(device)
		h.reconcile()

		device = h.device()
		Expect(device.Spec.SerialNumber).To(Equal("rewritten-by-dpf"), "the reconciler rewrote DPUDevice spec")
		Expect(device.Labels).To(HaveKeyWithValue(naming.LabelOrdinal, "000012"), "DPUDevice ordinal label not restored")
	})

	It("VirtualMachine is created only after the cloud-init Secret exists", func() {
		h := newInstanceHarness(instanceFleet())
		h.issuer.err = errors.New("token API unavailable")
		_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(h.instance),
		})
		Expect(err).To(HaveOccurred(), "Reconcile() error = nil, want the token failure")
		vmName := naming.VirtualMachineName(h.fleet.Name, instanceOrdinal)
		Expect(h.get(vmName, &virtv1.VirtualMachine{})).To(BeFalse(), "VirtualMachine exists without its cloud-init Secret")
		requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionFalse, "")

		h.issuer.err = nil
		h.reconcile()
		vm := h.virtualMachine()
		Expect(controlledByInstance(vm, h.instance)).To(BeTrue(),
			"VirtualMachine owner = %+v, want the SimInstance", vm.OwnerReferences)
		requireCondition(h.current(), scalev1alpha1.SimInstanceChildrenCreated, metav1.ConditionTrue, "")
	})

	It("a device without a booted guest is not selectable", func() {
		h := newInstanceHarness(instanceFleet())

		h.reconcile()

		requireCondition(h.current(), scalev1alpha1.SimInstanceSelectable, metav1.ConditionFalse, "")
	})

	It("a bridged guest address makes the device selectable", func() {
		vm, vmi := seededGuest(instanceFleet(), "10.244.0.17")
		h := newInstanceHarness(instanceFleet(), vm, vmi)
		h.reconcile()

		device := h.device()
		Expect(device.Spec.BMCIP).To(HaveValue(Equal("10.244.0.17")), "DPUDevice spec.bmcIP")
		for key, want := range h.fleet.Spec.DeviceSelectionLabels {
			Expect(device.Labels).To(HaveKeyWithValue(key, want), "DPUDevice selection label %s", key)
		}
		requireCondition(h.current(), scalev1alpha1.SimInstanceSelectable, metav1.ConditionTrue, "")
		requireCondition(h.current(), scalev1alpha1.SimInstanceGuestReady, metav1.ConditionTrue, "")
	})

	It("a DPU with kubelet configured is provisioned", func() {
		h := newInstanceHarness(instanceFleet())
		dpuName := dpuagent.CredentialName(h.fleet, instanceOrdinal)
		h.createDPU(dpuName, true)

		h.reconcile()

		instance := h.current()
		requireCondition(instance, scalev1alpha1.SimInstanceDPUProvisioned, metav1.ConditionTrue, "")
		Expect(instance.Status.DPUName).To(Equal(dpuName), "status.dpuName")
	})

	It("two DPUs naming the device are reported as duplicates", func() {
		h := newInstanceHarness(instanceFleet())
		h.createDPU("dpu-a", true)
		h.createDPU("dpu-b", true)

		h.reconcile()

		requireCondition(h.current(), scalev1alpha1.SimInstanceDPUProvisioned, metav1.ConditionFalse, "DuplicateDPU")
	})

	It("status.dpuName is cleared unless exactly one DPU names the device", func() {
		h := newInstanceHarness(instanceFleet())
		first := h.createDPU("dpu-a", true)
		h.reconcile()
		Expect(h.current().Status.DPUName).To(Equal("dpu-a"), "status.dpuName")

		second := h.createDPU("dpu-b", true)
		h.reconcile()
		Expect(h.current().Status.DPUName).To(BeEmpty(), "status.dpuName with duplicate DPUs")

		for _, dpu := range []*provisioningv1.DPU{first, second} {
			Expect(h.client.Delete(context.Background(), dpu)).To(Succeed(), "delete DPU %s", dpu.Name)
		}
		h.createDPU("dpu-a", true)
		h.reconcile()
		Expect(h.current().Status.DPUName).To(Equal("dpu-a"), "status.dpuName")
		Expect(h.client.Delete(context.Background(), &provisioningv1.DPU{
			ObjectMeta: metav1.ObjectMeta{Name: "dpu-a", Namespace: h.fleet.Namespace},
		})).To(Succeed(), "delete DPU dpu-a")
		h.reconcile()
		Expect(h.current().Status.DPUName).To(BeEmpty(), "status.dpuName with no DPU")
	})

	It("no DPU is reported as missing", func() {
		h := newInstanceHarness(instanceFleet())

		h.reconcile()

		requireCondition(h.current(), scalev1alpha1.SimInstanceDPUProvisioned, metav1.ConditionFalse, "DPUMissing")
		Expect(h.current().Status.Phase).To(Equal(scalev1alpha1.SimInstanceProvisioning))
	})

	It("all conditions true records every child UID and reaches Ready", func() {
		vm, vmi := seededGuest(instanceFleet(), "10.244.0.17")
		h := newInstanceHarness(instanceFleet(), vm, vmi)
		dpu := h.createDPU(dpuagent.CredentialName(h.fleet, instanceOrdinal), true)

		h.reconcile()

		instance := h.current()
		Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceReady),
			"conditions = %+v", instance.Status.Conditions)
		want := scalev1alpha1.ObservedUIDs{
			DPU:                    string(dpu.UID),
			DPUDevice:              string(h.device().UID),
			VirtualMachine:         string(h.virtualMachine().UID),
			VirtualMachineInstance: string(vmi.UID),
		}
		Expect([]string{want.DPU, want.DPUDevice, want.VirtualMachine, want.VirtualMachineInstance}).
			NotTo(ContainElement(BeEmpty()), "fixture UIDs incomplete: %+v", want)
		Expect(instance.Status.ObservedUIDs).To(Equal(want))
		Expect(instance.Status.ObservedGeneration).To(Equal(instance.Generation))
	})

	It("a fleet without a VM template is Ready without guest UIDs", func() {
		fleet := instanceFleet()
		fleet.Spec.VirtualMachineTemplate = nil
		h := newInstanceHarness(fleet)
		dpu := h.createDPU(dpuagent.CredentialName(fleet, instanceOrdinal), true)

		h.reconcile()

		instance := h.current()
		Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceReady),
			"conditions = %+v", instance.Status.Conditions)
		requireCondition(instance, scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "NotRequired")
		want := scalev1alpha1.ObservedUIDs{DPU: string(dpu.UID), DPUDevice: string(h.device().UID)}
		Expect(instance.Status.ObservedUIDs).To(Equal(want))
		Expect(h.get(dpuagent.CredentialName(fleet, instanceOrdinal), &corev1.ServiceAccount{})).To(BeFalse(),
			"ServiceAccount created for a fleet that boots no guest")
		Expect(h.issuer.n).To(BeZero(), "token issued for a fleet that boots no guest")
	})

	It("a DPUDevice controlled by another owner fails the instance and is left alone", func() {
		fleet := instanceFleet()
		foreign := dpf.DesiredDevice(fleet, instanceOrdinal)
		foreign.Labels = map[string]string{"owner": "someone-else"}
		foreign.OwnerReferences = foreignOwner()
		h := newInstanceHarness(fleet, foreign)
		before := h.device()

		h.reconcile()

		instance := h.current()
		Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceFailed))
		Expect(instance.Status.FailureReason).To(Equal("OwnershipConflict"))
		after := h.device()
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "foreign DPUDevice was modified")
		Expect(after.Labels).To(Equal(before.Labels), "foreign DPUDevice was modified")
		Expect(after.OwnerReferences).To(Equal(before.OwnerReferences), "foreign DPUDevice was modified")
		Expect(h.get(naming.VirtualMachineName(fleet.Name, instanceOrdinal), &virtv1.VirtualMachine{})).To(BeFalse(),
			"converge continued past the ownership conflict")
	})

	It("a missing fleet is not an error and creates nothing", func() {
		h := newInstanceHarness(instanceFleet())
		Expect(h.client.Delete(context.Background(), h.fleet)).To(Succeed(), "delete fleet")

		h.reconcile()

		Expect(h.get(naming.DeviceName(h.fleet.Name, instanceOrdinal), &provisioningv1.DPUDevice{})).To(BeFalse(),
			"DPUDevice created without an owning fleet")
		Expect(h.get(dpuagent.CredentialName(h.fleet, instanceOrdinal), &corev1.ServiceAccount{})).To(BeFalse(),
			"ServiceAccount created without an owning fleet")
		Expect(h.current().Finalizers).To(BeEmpty(), "finalizers without a fleet")
	})

	Describe("credentials", func() {
		credentials := func(fleet *scalev1alpha1.SimFleet) *readyChildren {
			children := newReadyChildren(fleet)
			children.serviceAccount.UID = "sa-uid"
			return children
		}

		Describe("read the API server past the cache", func() {
			It("a cloud-init Secret the cache has not seen yet mints no second token", func() {
				fleet := instanceFleet()
				children := credentials(fleet)
				h := newInstanceHarness(fleet,
					children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
				h.reconciler.Client = cacheWithout(h.client, func(obj client.Object) bool {
					return obj.GetName() == children.cloudInit.Name
				})

				h.reconcile()

				Expect(h.issuer.n).To(BeZero(), "token issued for an existing cloud-init Secret")
				requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "Ready")
			})

			It("an owned cloud-init Secret that lost its labels is repaired, not reissued", func() {
				fleet := instanceFleet()
				children := credentials(fleet)
				children.cloudInit.Labels = nil
				h := newInstanceHarness(fleet,
					children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
				h.reconciler.Client = cacheWithout(h.client, labelScoped)

				h.reconcile()

				Expect(h.issuer.n).To(BeZero(), "token issued for an existing cloud-init Secret")
				secret := &corev1.Secret{}
				Expect(h.get(children.cloudInit.Name, secret)).To(BeTrue(), "cloud-init Secret is gone")
				for key, want := range cloudinit.SecretLabels(fleet, instanceOrdinal) {
					Expect(secret.Labels).To(HaveKeyWithValue(key, want), "Secret label %s not restored", key)
				}
			})

			It("an owned ServiceAccount that lost its labels is repaired on AlreadyExists", func() {
				fleet := instanceFleet()
				children := credentials(fleet)
				children.serviceAccount.Labels = nil
				h := newInstanceHarness(fleet,
					children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
				h.reconciler.Client = cacheWithout(h.client, labelScoped)

				h.reconcile()

				account := &corev1.ServiceAccount{}
				Expect(h.get(children.serviceAccount.Name, account)).To(BeTrue(), "ServiceAccount is gone")
				for key, want := range dpuagent.DesiredServiceAccount(fleet, instanceOrdinal).Labels {
					Expect(account.Labels).To(HaveKeyWithValue(key, want), "ServiceAccount label %s not restored", key)
				}
				requireCondition(h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "Ready")
			})

			It("a label-stripped Secret held by another owner fails the instance", func() {
				fleet := instanceFleet()
				children := credentials(fleet)
				children.cloudInit.Labels = nil
				children.cloudInit.OwnerReferences = foreignOwner()
				h := newInstanceHarness(fleet,
					children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
				h.reconciler.Client = cacheWithout(h.client, labelScoped)

				h.reconcile()

				Expect(h.issuer.n).To(BeZero(), "token issued for a Secret name another owner holds")
				Expect(h.current().Status.FailureReason).To(Equal("OwnershipConflict"))
				secret := &corev1.Secret{}
				Expect(h.get(children.cloudInit.Name, secret)).To(BeTrue(), "foreign Secret is gone")
				Expect(secret.Labels).To(BeEmpty(), "foreign Secret was modified: %+v", secret.ObjectMeta)
			})
		})
	})

	Describe("drift", func() {
		DescribeTable("a drifted child",
			func(mutate func(*readyChildren), reason string) {
				fleet := instanceFleet()
				children := newReadyChildren(fleet)
				mutate(children)
				th := newReadyInstanceHarness(fleet, children)
				writes := th.countWrites(false)

				th.reconcile()

				instance := th.current()
				Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceFailed))
				Expect(instance.Status.FailureReason).To(Equal(reason))
				got := th.events()
				Expect(hasEvent(got, "Normal SimInstanceFailed")).To(BeTrue(),
					"Events = %q, want a Normal SimInstanceFailed Event", got)

				th.reconcile()
				Expect(*writes).To(BeZero(), "reconciler wrote children of a drifted instance")
				Expect(th.current().Status.Phase).To(Equal(scalev1alpha1.SimInstanceFailed), "phase after another Reconcile")
			},
			Entry("DPU UID changed fails the instance and converges nothing",
				func(c *readyChildren) { c.dpu.UID = "new-dpu-uid" }, "DPUReplaced"),
			Entry("DPUDevice UID changed fails the instance and converges nothing",
				func(c *readyChildren) { c.device.UID = "new-device-uid" }, "DPUDeviceReplaced"),
			Entry("VirtualMachine UID changed fails the instance and converges nothing",
				func(c *readyChildren) { c.vm.UID = "new-vm-uid" }, "VirtualMachineReplaced"),
			Entry("VirtualMachineInstance UID changed fails the instance and converges nothing",
				func(c *readyChildren) { c.vmi.UID = "new-vmi-uid" }, "VirtualMachineInstanceReplaced"),
			Entry("DPU missing fails the instance and converges nothing",
				func(c *readyChildren) { c.dpu = nil }, "DPUReplaced"),
			Entry("DPUDevice missing fails the instance and converges nothing",
				func(c *readyChildren) { c.device = nil }, "DPUDeviceReplaced"),
			Entry("VirtualMachine missing fails the instance and converges nothing",
				func(c *readyChildren) { c.vm = nil }, "VirtualMachineReplaced"),
			Entry("VirtualMachineInstance missing fails the instance and converges nothing",
				func(c *readyChildren) { c.vmi = nil }, "VirtualMachineInstanceReplaced"),
		)

		It("recorded UIDs that still match are not drift", func() {
			fleet := instanceFleet()
			th := newReadyInstanceHarness(fleet, newReadyChildren(fleet))

			th.reconcile()

			instance := th.current()
			Expect(instance.Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed))
			Expect(instance.Status.FailureReason).To(BeEmpty())
		})

		It("the recorded DPU under another name is not drift", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.dpu.Name = "dpf-chose-another-name"
			th := newReadyInstanceHarness(fleet, children)

			th.reconcile()

			instance := th.current()
			Expect(instance.Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed))
			Expect(instance.Status.FailureReason).To(BeEmpty())
		})

		It("a DPU of the recorded name that names another device is drift", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.dpu.Spec.DPUDeviceName = "someone-else-device"
			th := newReadyInstanceHarness(fleet, children)

			th.reconcile()

			Expect(th.current().Status.FailureReason).To(Equal("DPUReplaced"))
		})

		It("a fleet without a VM template has no guest to drift", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			fleet.Spec.VirtualMachineTemplate = nil
			children.vm, children.vmi = nil, nil
			children.serviceAccount, children.role, children.roleBinding, children.cloudInit = nil, nil, nil, nil
			th := newReadyInstanceHarness(fleet, children)
			instance := th.current()
			instance.Status.ObservedUIDs = scalev1alpha1.ObservedUIDs{
				DPU:       recordedUIDs.DPU,
				DPUDevice: recordedUIDs.DPUDevice,
			}
			Expect(th.client.Status().Update(context.Background(), instance)).To(Succeed(), "update SimInstance status")

			th.reconcile()

			instance = th.current()
			Expect(instance.Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed))
			Expect(instance.Status.FailureReason).To(BeEmpty())
		})

		It("no recorded UIDs is not drift", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.dpu.UID = "any-dpu-uid"
			h := newInstanceHarness(fleet, children.dpu)

			h.reconcile()

			instance := h.current()
			Expect(instance.Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed))
			Expect(instance.Status.FailureReason).To(BeEmpty())
		})
	})

	Describe("teardown", func() {
		It("children are deleted in order and the finalizer is removed last", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			th := newReadyInstanceHarness(fleet, children)
			var round []string
			quarantinedFirst := false
			th.reconciler.Client = interceptor.NewClient(th.client.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*provisioningv1.DPU); ok {
						device := &provisioningv1.DPUDevice{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(children.device), device); err != nil {
							return err
						}
						quarantinedFirst = true
						for key := range dpf.DeviceSelectionLabels(fleet) {
							if _, ok := device.Labels[key]; ok {
								quarantinedFirst = false
							}
						}
					}
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}
					round = append(round, deleteRecord(obj))
					return nil
				},
			})
			th.deleteInstance()

			// Every step but the VMI's is woken by its object's delete event, so
			// it requeues only for its force deadline. A VMI whose VM is gone
			// maps to no instance, so that step polls.
			want := []struct {
				deleted []string
				requeue time.Duration
			}{
				{[]string{deleteRecord(children.dpu), deleteRecord(children.joinSecret)}, lifecycleForceTimeout},
				{[]string{deleteRecord(children.device)}, lifecycleForceTimeout},
				{[]string{deleteRecord(children.vm)}, lifecycleForceTimeout},
				{[]string{deleteRecord(children.vmi)}, teardownPoll},
				{[]string{
					deleteRecord(children.cloudInit), deleteRecord(children.roleBinding),
					deleteRecord(children.role), deleteRecord(children.serviceAccount),
				}, lifecycleForceTimeout},
				{nil, 0},
			}
			for i, wantRound := range want {
				round = nil
				result := th.reconcileResult()
				Expect(round).To(Equal(wantRound.deleted), "Reconcile %d deleted", i+1)
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: wantRound.requeue}), "Reconcile %d result", i+1)
				if i == 0 {
					instance := th.current()
					Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceTerminating))
					Expect(instance.Status.TeardownStartedAt).NotTo(BeNil(), "status.teardownStartedAt is not set")
					requireCondition(instance, scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteDPU")
				}
			}
			Expect(quarantinedFirst).To(BeTrue(), "DPU was deleted before the DPUDevice lost its selection labels")
			Expect(th.instanceGone()).To(BeTrue(), func() string {
				return fmt.Sprintf("SimInstance still exists with finalizers %v", th.current().Finalizers)
			})
		})

		It("a DPU naming a different device is left and teardown continues", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.dpu.Spec.DPUDeviceName = "someone-else-device"
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.runTeardown()

			Expect(th.exists(children.dpu)).To(BeTrue(), "DPU naming another device was deleted")
			Expect(th.exists(children.joinSecret)).To(BeTrue(), "join Secret of a DPU that is not ours was deleted")
			Expect(th.exists(children.device)).To(BeFalse(), "DPUDevice was not deleted")
		})

		DescribeTable("a DPU whose device is gone is deleted only when its UID was recorded",
			func(uid types.UID, deleted bool) {
				fleet := instanceFleet()
				children := newReadyChildren(fleet)
				children.device = nil
				children.dpu.UID = uid
				th := newReadyInstanceHarness(fleet, children)
				th.deleteInstance()

				th.runTeardown()

				Expect(!th.exists(children.dpu)).To(Equal(deleted), "DPU UID %s deleted", uid)
			},
			Entry("recorded UID", types.UID(recordedUIDs.DPU), true),
			Entry("unrecorded UID", types.UID("other-dpu-uid"), false),
		)

		It("a VMI owned by another VM is left", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.vm = nil
			children.vmi.OwnerReferences[0].UID = "other-vm-uid"
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.runTeardown()

			Expect(th.exists(children.vmi)).To(BeTrue(), "VMI owned by another VirtualMachine was deleted")
		})

		It("a VMI whose VM is gone is deleted when both UIDs were recorded", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.vm = nil
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.runTeardown()

			Expect(th.exists(children.vmi)).To(BeFalse(), "VMI of the recorded VirtualMachine was not deleted")
		})

		It("a step blocked by a finalizer is forced only after lifecycleForceTimeout", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.device.Finalizers = []string{"provisioning.dpu.nvidia.com/dpudevice-protection"}
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.reconcileResult() // quarantine and delete the DPU
			th.reconcileResult() // delete the DPUDevice, which its finalizer holds
			device := &provisioningv1.DPUDevice{}
			Expect(th.get(children.device.Name, device)).To(BeTrue(), "DPUDevice is gone")
			Expect(device.DeletionTimestamp.IsZero()).To(BeFalse(), "DPUDevice was not marked for deletion")

			th.now = teardownClock.Add(lifecycleForceTimeout - time.Second)
			th.reconcileResult()
			Expect(th.get(children.device.Name, device)).To(BeTrue(), "DPUDevice deleted before lifecycleForceTimeout")
			Expect(device.Finalizers).NotTo(BeEmpty(),
				"DPUDevice finalizers stripped before lifecycleForceTimeout: %+v", device.ObjectMeta)
			got := th.events()
			Expect(hasEvent(got, "Warning LifecycleForceDelete")).To(BeFalse(),
				"Events = %q, want no LifecycleForceDelete before the timeout", got)
			requireCondition(th.current(), scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteDPUDevice")

			th.now = teardownClock.Add(lifecycleForceTimeout)
			th.reconcileResult()
			Expect(th.exists(children.device)).To(BeFalse(), "DPUDevice still exists after lifecycleForceTimeout")
			got = th.events()
			Expect(hasEvent(got, "Warning LifecycleForceDelete")).To(BeTrue(),
				"Events = %q, want a LifecycleForceDelete Warning", got)

			th.runTeardown()
		})

		It("the next step's deadline starts when the previous step finishes", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.vm.Finalizers = []string{"kubevirt.io/virtualMachineControllerFinalize"}
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.reconcileResult() // quarantine and delete the DPU
			th.now = teardownClock.Add(lifecycleForceTimeout)
			th.reconcileResult() // delete the DPUDevice
			th.reconcileResult() // delete the VirtualMachine, which its finalizer holds
			th.reconcileResult() // the VirtualMachine is still blocked, within its own deadline
			vm := &virtv1.VirtualMachine{}
			Expect(th.get(children.vm.Name, vm)).To(BeTrue(), "VirtualMachine was forced on the deadline of an earlier step")
			Expect(vm.Finalizers).NotTo(BeEmpty(), "VirtualMachine was forced on the deadline of an earlier step")
			requireCondition(th.current(), scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteVM")
		})

		It("a blocked step requeues for what is left of its force deadline", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.device.Finalizers = []string{"provisioning.dpu.nvidia.com/dpudevice-protection"}
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.reconcileResult() // quarantine and delete the DPU
			th.reconcileResult() // delete the DPUDevice, which its finalizer holds
			th.now = teardownClock.Add(time.Minute)
			Expect(th.reconcileResult().RequeueAfter).To(Equal(lifecycleForceTimeout - time.Minute))
			th.now = teardownClock.Add(lifecycleForceTimeout - time.Millisecond)
			Expect(th.reconcileResult().RequeueAfter).To(Equal(teardownPoll), "want the teardownPoll minimum")
		})

		It("the VMI gets its own force deadline once its VirtualMachine is gone", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.vm.Finalizers = []string{"kubevirt.io/virtualMachineControllerFinalize"}
			children.vmi.Finalizers = []string{"kubevirt.io/foregroundDeleteVirtualMachine"}
			th := newReadyInstanceHarness(fleet, children)
			th.deleteInstance()

			th.reconcileResult() // quarantine and delete the DPU
			th.reconcileResult() // delete the DPUDevice
			th.reconcileResult() // delete the VirtualMachine, which its finalizer holds
			th.now = teardownClock.Add(lifecycleForceTimeout - time.Second)
			th.reconcileResult()
			vm := &virtv1.VirtualMachine{}
			Expect(th.get(children.vm.Name, vm)).To(BeTrue(), "VirtualMachine was forced before its deadline")
			vm.Finalizers = nil
			th.update(vm) // KubeVirt lets go just before the deadline

			th.now = teardownClock.Add(lifecycleForceTimeout)
			Expect(th.reconcileResult().RequeueAfter).To(Equal(teardownPoll), "want the VMI step to poll every teardownPoll")
			requireCondition(th.current(), scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteVMI")
			th.now = th.now.Add(time.Second)
			th.reconcileResult()
			vmi := &virtv1.VirtualMachineInstance{}
			Expect(th.get(children.vmi.Name, vmi)).To(BeTrue(), "VMI was forced on its VirtualMachine's deadline")
			Expect(vmi.Finalizers).NotTo(BeEmpty(), "VMI was forced on its VirtualMachine's deadline")
			got := th.events()
			Expect(hasEvent(got, "Warning LifecycleForceDelete")).To(BeFalse(),
				"Events = %q, want no LifecycleForceDelete yet", got)

			th.now = teardownClock.Add(2 * lifecycleForceTimeout)
			th.reconcileResult()
			Expect(th.exists(children.vmi)).To(BeFalse(), "VMI still exists after its own lifecycleForceTimeout")
			th.runTeardown()
		})

		It("a deleting fleet still names the selection labels quarantine strips", func() {
			fleet := instanceFleet()
			fleet.Finalizers = []string{scalev1alpha1.SimFleetFinalizer}
			now := metav1.Now()
			fleet.DeletionTimestamp = &now
			children := newReadyChildren(fleet)
			th := newReadyInstanceHarness(fleet, children)
			var (
				dpuDeleted        bool
				labelsAtDPUDelete map[string]string
			)
			th.reconciler.Client = interceptor.NewClient(th.client.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*provisioningv1.DPU); ok {
						device := &provisioningv1.DPUDevice{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(children.device), device); err != nil {
							return err
						}
						dpuDeleted = true
						labelsAtDPUDelete = device.Labels
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			th.deleteInstance()

			th.runTeardown()

			Expect(dpuDeleted).To(BeTrue(), "DPU was not deleted")
			for key := range dpf.DeviceSelectionLabels(fleet) {
				Expect(labelsAtDPUDelete).NotTo(HaveKey(key), "DPUDevice labels at DPU delete should have %s stripped", key)
			}
		})

		DescribeTable("teardown completes with the fleet gone",
			func(orphan func(*scalev1alpha1.SimInstance)) {
				fleet := instanceFleet()
				children := newReadyChildren(fleet)
				th := newReadyInstanceHarness(fleet, children)
				instance := th.current()
				orphan(instance)
				th.update(instance)
				Expect(th.client.Delete(context.Background(), fleet)).To(Succeed(), "delete fleet")
				th.deleteInstance()

				th.runTeardown()

				for _, obj := range children.objects() {
					Expect(th.exists(obj)).To(BeFalse(), "%T %s survived teardown without its fleet", obj, obj.GetName())
				}
			},
			Entry("named by its label", func(instance *scalev1alpha1.SimInstance) { instance.OwnerReferences = nil }),
			Entry("named by its owner reference", func(instance *scalev1alpha1.SimInstance) { instance.Labels = nil }),
		)

		It("an instance that names no fleet is released without touching anything", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			th := newReadyInstanceHarness(fleet, children)
			instance := th.current()
			instance.Labels = nil
			instance.OwnerReferences = nil
			th.update(instance)
			writes := th.countWrites(false)
			th.deleteInstance()

			th.runTeardown()

			Expect(*writes).To(BeZero(), "reconciler wrote children it cannot name")
			for _, obj := range children.objects() {
				Expect(th.exists(obj)).To(BeTrue(), "%T %s was deleted by an instance that names no fleet", obj, obj.GetName())
			}
		})

		It("a failing step persists its progress and returns the error", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			th := newReadyInstanceHarness(fleet, children)
			th.reconciler.Client = interceptor.NewClient(th.client.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*provisioningv1.DPU); ok {
						return errors.New("DPU API unavailable")
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			th.deleteInstance()

			_, err := th.reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(th.instance),
			})

			Expect(err).To(MatchError(ContainSubstring("DPU API unavailable")), "want the DPU delete failure")
			instance := th.current()
			Expect(instance.Status.Phase).To(Equal(scalev1alpha1.SimInstanceTerminating))
			Expect(instance.Status.TeardownStartedAt).NotTo(BeNil(), "status.teardownStartedAt is not set")
			requireCondition(instance, scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, teardownDeleteDPU)
			Expect(instance.Finalizers).To(ContainElement(scalev1alpha1.SimInstanceFinalizer), "finalizer removed after a failed step")
		})

		It("children controlled by another owner are left untouched", func() {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			children.device.OwnerReferences = foreignOwner()
			children.vm.OwnerReferences = foreignOwner()
			children.role.OwnerReferences = foreignOwner()
			th := newReadyInstanceHarness(fleet, children)
			before := map[string]client.Object{}
			for _, obj := range []client.Object{children.device, children.vm, children.vmi, children.role, children.dpu} {
				live := obj.DeepCopyObject().(client.Object)
				Expect(th.get(obj.GetName(), live)).To(BeTrue(), "%T %s was not seeded", obj, obj.GetName())
				before[deleteRecord(obj)] = live
			}
			th.deleteInstance()

			th.runTeardown()

			for key, want := range before {
				got := want.DeepCopyObject().(client.Object)
				Expect(th.get(want.GetName(), got)).To(BeTrue(), "%s was deleted", key)
				Expect(got.GetResourceVersion()).To(Equal(want.GetResourceVersion()), "%s was modified", key)
				Expect(got.GetDeletionTimestamp().IsZero()).To(BeTrue(), "%s was marked for deletion", key)
			}
			Expect(th.exists(children.joinSecret)).To(BeTrue(), "join Secret of a DPU that is not ours was deleted")
			Expect(th.exists(children.serviceAccount)).To(BeFalse(), "ServiceAccount the instance owns was not deleted")
		})
	})
})

var _ = Describe("SimInstance helpers", func() {
	Describe("labelScopedCacheKind", func() {
		It("matches ManagerCacheOptions", func() {
			byObject := ManagerCacheOptions().ByObject
			for obj := range byObject {
				Expect(labelScopedCacheKind(obj)).To(BeTrue(),
					"labelScopedCacheKind(%T) = false, but ManagerCacheOptions scopes it by label", obj)
			}
			Expect(byObject).To(HaveLen(4), "ManagerCacheOptions should scope the 4 kinds labelScopedCacheKind knows")
			Expect(labelScopedCacheKind(&provisioningv1.DPUDevice{})).To(BeFalse(), "labelScopedCacheKind(DPUDevice)")
		})
	})

	Describe("mapFleetToInstances", func() {
		It("requests only the labelled instances the fleet owns in its namespace", func() {
			fleet := instanceFleet()
			labelled := func(ordinal int) *scalev1alpha1.SimInstance {
				instance := newInstanceFixture(fleet, ordinal)
				instance.Labels = map[string]string{naming.LabelFleetName: fleet.Name}
				return instance
			}
			owned := labelled(0)
			earlierFleet := labelled(1)
			earlierFleet.OwnerReferences[0].UID = "earlier-fleet-uid"
			unlabelled := newInstanceFixture(fleet, 2)
			otherNamespace := labelled(3)
			otherNamespace.Namespace = "other"
			h := newInstanceHarness(fleet, owned, earlierFleet, unlabelled, otherNamespace)

			got := h.reconciler.mapFleetToInstances(context.Background(), fleet)

			Expect(got).To(Equal([]ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(owned)}}))
			Expect(h.reconciler.mapFleetToInstances(context.Background(), &corev1.Secret{})).To(BeNil(),
				"mapFleetToInstances(Secret)")
		})
	})

	Describe("kubeletConfigured", func() {
		It("is true only for a true KubeletConfigured condition", func() {
			Expect(kubeletConfigured(nil)).To(BeFalse(), "kubeletConfigured(nil)")
			dpu := &provisioningv1.DPU{}
			Expect(kubeletConfigured(dpu)).To(BeFalse(), "kubeletConfigured() without agent status")

			dpu.Status.AgentStatus = &provisioningv1.AgentStatus{}
			meta.SetStatusCondition(&dpu.Status.AgentStatus.Conditions, metav1.Condition{
				Type:   "KubeletConfigured",
				Status: metav1.ConditionFalse,
			})
			Expect(kubeletConfigured(dpu)).To(BeFalse(), "kubeletConfigured() for false condition")
			meta.SetStatusCondition(&dpu.Status.AgentStatus.Conditions, metav1.Condition{
				Type:   "KubeletConfigured",
				Status: metav1.ConditionTrue,
			})
			Expect(kubeletConfigured(dpu)).To(BeTrue(), "kubeletConfigured() for true condition")
		})
	})
})

var _ = Describe("stripFinalizersAndForceDelete", func() {
	var instance *scalev1alpha1.SimInstance

	finalizedSecret := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:       "child",
			Namespace:  "fleet-ns",
			Finalizers: []string{"x/y"},
		}}
	}

	BeforeEach(func() {
		instance = &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{Name: "scale-3000-000000", Namespace: "fleet-ns"}}
	})

	It("removes the object and records an Event", func(ctx SpecContext) {
		secret := finalizedSecret()
		c := fake.NewClientBuilder().WithScheme(instanceTestScheme()).WithObjects(secret).Build()
		recorder := events.NewFakeRecorder(10)

		Expect(stripFinalizersAndForceDelete(
			ctx, c, recorder, instance, secret.DeepCopy(), "Secret", 0, lifecycleForceTimeout+time.Second,
		)).To(Succeed())
		err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Secret GET error = %v, want NotFound", err)
		want := fmt.Sprintf(
			"Warning LifecycleForceDelete stripped finalizers from Secret %s ordinal %d after %s",
			secret.Name, 0, lifecycleForceTimeout,
		)
		Expect(recorder.Events).To(Receive(Equal(want)))
		Expect(recorder.Events).NotTo(Receive(), "unexpected second Event")
	})

	It("tolerates an already-gone object", func(ctx SpecContext) {
		c := fake.NewClientBuilder().WithScheme(instanceTestScheme()).Build()
		recorder := events.NewFakeRecorder(10)

		Expect(stripFinalizersAndForceDelete(
			ctx, c, recorder, instance, finalizedSecret(), "Secret", 0, lifecycleForceTimeout,
		)).To(Succeed())
		Expect(recorder.Events).NotTo(Receive(), "unexpected Event for an already-gone object")
	})

	It("works without a recorder", func(ctx SpecContext) {
		secret := finalizedSecret()
		c := fake.NewClientBuilder().WithScheme(instanceTestScheme()).WithObjects(secret).Build()

		Expect(stripFinalizersAndForceDelete(
			ctx, c, nil, instance, secret.DeepCopy(), "Secret", 0, lifecycleForceTimeout,
		)).To(Succeed())
		err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Secret GET error = %v, want NotFound", err)
	})
})
