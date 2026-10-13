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
	"strings"
	"testing"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

const instanceOrdinal = 12

type countingServiceAccountIssuer struct {
	n     int
	token []byte
	err   error
}

func (c *countingServiceAccountIssuer) Issue(
	_ context.Context,
	_, _ string,
) ([]byte, error) {
	c.n++
	if c.err != nil {
		return nil, c.err
	}
	if len(c.token) == 0 {
		return []byte("management-service-account-token"), nil
	}
	return c.token, nil
}

func instanceFleet() *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: scalev1alpha1.GroupVersion.String(),
			Kind:       "SimFleet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000",
			Namespace: "ns",
			UID:       types.UID("fleet-uid"),
		},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              20,
			DevicesPerDPUNode:     10,
			DeviceSelectionLabels: map[string]string{"scale.dpu.nvidia.com/pool": "scale-3000"},
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
				Image: "simfleet-node:test",
			},
		},
	}
}

func newInstanceFixture(fleet *scalev1alpha1.SimFleet, ordinal int) *scalev1alpha1.SimInstance {
	return &scalev1alpha1.SimInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.InstanceName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			UID:       types.UID(fmt.Sprintf("instance-uid-%d", ordinal)),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         scalev1alpha1.GroupVersion.String(),
				Kind:               "SimFleet",
				Name:               fleet.Name,
				UID:                fleet.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: scalev1alpha1.SimInstanceSpec{
			Ordinal:   ordinal,
			HostIndex: utils.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
		},
	}
}

func instanceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		scalev1alpha1.AddToScheme,
		provisioningv1.AddToScheme,
		virtv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return scheme
}

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
	t          *testing.T
	client     client.Client
	reconciler *SimInstanceReconciler
	issuer     *countingServiceAccountIssuer
	fleet      *scalev1alpha1.SimFleet
	instance   *scalev1alpha1.SimInstance
}

func newInstanceHarness(t *testing.T, fleet *scalev1alpha1.SimFleet, objects ...client.Object) *instanceHarness {
	t.Helper()
	scheme := instanceTestScheme(t)
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
		t:      t,
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
	h.t.Helper()
	result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(h.instance),
	})
	if err != nil {
		h.t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{}) {
		h.t.Fatalf("Reconcile() result = %+v, want empty", result)
	}
}

func (h *instanceHarness) get(name string, obj client.Object) bool {
	h.t.Helper()
	err := h.client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: h.fleet.Namespace}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		h.t.Fatalf("get %T %q: %v", obj, name, err)
	}
	return true
}

func (h *instanceHarness) current() *scalev1alpha1.SimInstance {
	h.t.Helper()
	instance := &scalev1alpha1.SimInstance{}
	if !h.get(h.instance.Name, instance) {
		h.t.Fatal("SimInstance is gone")
	}
	return instance
}

func (h *instanceHarness) device() *provisioningv1.DPUDevice {
	h.t.Helper()
	device := &provisioningv1.DPUDevice{}
	if !h.get(utils.DeviceName(h.fleet.Name, instanceOrdinal), device) {
		h.t.Fatal("DPUDevice was not created")
	}
	return device
}

func (h *instanceHarness) virtualMachine() *virtv1.VirtualMachine {
	h.t.Helper()
	vm := &virtv1.VirtualMachine{}
	if !h.get(utils.VirtualMachineName(h.fleet.Name, instanceOrdinal), vm) {
		h.t.Fatal("VirtualMachine was not created")
	}
	return vm
}

func (h *instanceHarness) create(obj client.Object) {
	h.t.Helper()
	if err := h.client.Create(context.Background(), obj); err != nil {
		h.t.Fatalf("create %T %q: %v", obj, obj.GetName(), err)
	}
}

func (h *instanceHarness) update(obj client.Object) {
	h.t.Helper()
	if err := h.client.Update(context.Background(), obj); err != nil {
		h.t.Fatalf("update %T %q: %v", obj, obj.GetName(), err)
	}
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
	vm := desiredVirtualMachine(fleet, instanceOrdinal)
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
			Labels:    virtualMachineLabels(fleet, instanceOrdinal),
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
				Name: bridgedNetworkName,
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
	h.t.Helper()
	dpu := &provisioningv1.DPU{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.fleet.Namespace},
		Spec: provisioningv1.DPUSpec{
			DPUDeviceName: utils.DeviceName(h.fleet.Name, instanceOrdinal),
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
	return controlledBy(obj, scalev1alpha1.GroupVersion.String(), "SimInstance", instance.Name, instance.UID)
}

func requireCondition(t *testing.T, instance *scalev1alpha1.SimInstance, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(instance.Status.Conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %s missing; conditions = %+v", conditionType, instance.Status.Conditions)
	}
	if condition.Status != status || (reason != "" && condition.Reason != reason) {
		t.Fatalf("condition %s = %s/%s (%s), want %s/%s",
			conditionType, condition.Status, condition.Reason, condition.Message, status, reason)
	}
	if condition.ObservedGeneration != instance.Generation {
		t.Fatalf("condition %s observedGeneration = %d, want %d",
			conditionType, condition.ObservedGeneration, instance.Generation)
	}
}

func TestSimInstanceReconcile(t *testing.T) {
	t.Run("finalizer is added before any child exists", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		var childSeenWithoutFinalizer bool
		creates := 0
		h.reconciler.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				creates++
				instance := &scalev1alpha1.SimInstance{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(h.instance), instance); err != nil {
					return err
				}
				if !controllerutil.ContainsFinalizer(instance, simInstanceTeardownFinalizer) {
					childSeenWithoutFinalizer = true
				}
				return c.Create(ctx, obj, opts...)
			},
		})

		h.reconcile()

		if creates == 0 {
			t.Fatal("Reconcile created no child, so the finalizer ordering was not exercised")
		}
		if childSeenWithoutFinalizer {
			t.Fatal("a child was created before the teardown finalizer was persisted")
		}
		if !controllerutil.ContainsFinalizer(h.current(), simInstanceTeardownFinalizer) {
			t.Fatalf("finalizers = %v, want %s", h.current().Finalizers, simInstanceTeardownFinalizer)
		}
	})

	t.Run("an instance of a deleting fleet converges nothing", func(t *testing.T) {
		fleet := instanceFleet()
		fleet.Finalizers = []string{simFleetTeardownFinalizer}
		now := metav1.Now()
		fleet.DeletionTimestamp = &now
		h := newInstanceHarness(t, fleet)
		writes := h.countWrites(true)

		h.reconcile()

		if *writes != 0 {
			t.Fatalf("reconciler made %d writes for an instance its fleet is about to delete", *writes)
		}
	})

	t.Run("credentials are owned by the instance and the cloud-init Secret is issued once", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		h.reconcile()

		name := dpuAgentCredentialName(h.fleet, instanceOrdinal)
		for _, obj := range []client.Object{&corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}} {
			if !h.get(name, obj) {
				t.Fatalf("%T %q was not created", obj, name)
			}
			if !controlledByInstance(obj, h.instance) {
				t.Fatalf("%T owner = %+v, want SimInstance %s", obj, obj.GetOwnerReferences(), h.instance.Name)
			}
		}
		secret := &corev1.Secret{}
		if !h.get(utils.CloudInitSecretName(h.fleet.Name, instanceOrdinal), secret) {
			t.Fatal("cloud-init Secret was not created")
		}
		if !controlledByInstance(secret, h.instance) {
			t.Fatalf("Secret owner = %+v, want the SimInstance", secret.OwnerReferences)
		}
		if h.issuer.n != 1 {
			t.Fatalf("token issued %d times, want 1", h.issuer.n)
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "")

		h.reconcile()
		if h.issuer.n != 1 {
			t.Fatalf("second Reconcile issued another token: %d calls", h.issuer.n)
		}
	})

	t.Run("DPUDevice carries the identity labels and its spec is never rewritten", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		h.reconcile()

		device := h.device()
		if !controlledByInstance(device, h.instance) {
			t.Fatalf("DPUDevice owner = %+v, want the SimInstance", device.OwnerReferences)
		}
		for key, want := range deviceLabels(h.fleet, instanceOrdinal) {
			if device.Labels[key] != want {
				t.Fatalf("DPUDevice label %s = %q, want %q", key, device.Labels[key], want)
			}
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceDeviceCreated, metav1.ConditionTrue, "")

		device.Spec.SerialNumber = "rewritten-by-dpf"
		delete(device.Labels, labelOrdinal)
		h.update(device)
		h.reconcile()

		device = h.device()
		if device.Spec.SerialNumber != "rewritten-by-dpf" {
			t.Fatalf("DPUDevice serialNumber = %q, the reconciler rewrote spec", device.Spec.SerialNumber)
		}
		if device.Labels[labelOrdinal] != "000012" {
			t.Fatalf("DPUDevice ordinal label = %q, want it restored", device.Labels[labelOrdinal])
		}
	})

	t.Run("VirtualMachine is created only after the cloud-init Secret exists", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		h.issuer.err = fmt.Errorf("token API unavailable")
		_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(h.instance),
		})
		if err == nil {
			t.Fatal("Reconcile() error = nil, want the token failure")
		}
		vmName := utils.VirtualMachineName(h.fleet.Name, instanceOrdinal)
		if h.get(vmName, &virtv1.VirtualMachine{}) {
			t.Fatal("VirtualMachine exists without its cloud-init Secret")
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionFalse, "")

		h.issuer.err = nil
		h.reconcile()
		vm := h.virtualMachine()
		if !controlledByInstance(vm, h.instance) {
			t.Fatalf("VirtualMachine owner = %+v, want the SimInstance", vm.OwnerReferences)
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceChildrenCreated, metav1.ConditionTrue, "")
	})

	t.Run("a device without a booted guest is not selectable", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())

		h.reconcile()

		requireCondition(t, h.current(), scalev1alpha1.SimInstanceSelectable, metav1.ConditionFalse, "")
	})

	t.Run("a bridged guest address makes the device selectable", func(t *testing.T) {
		vm, vmi := seededGuest(instanceFleet(), "10.244.0.17")
		h := newInstanceHarness(t, instanceFleet(), vm, vmi)
		h.reconcile()

		device := h.device()
		if device.Spec.BMCIP == nil || *device.Spec.BMCIP != "10.244.0.17" {
			t.Fatalf("DPUDevice spec.bmcIP = %v, want 10.244.0.17", device.Spec.BMCIP)
		}
		for key, want := range h.fleet.Spec.DeviceSelectionLabels {
			if device.Labels[key] != want {
				t.Fatalf("DPUDevice selection label %s = %q, want %q", key, device.Labels[key], want)
			}
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceSelectable, metav1.ConditionTrue, "")
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceVMReady, metav1.ConditionTrue, "")
	})

	t.Run("a DPU with kubelet configured is provisioned", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		dpuName := dpuAgentCredentialName(h.fleet, instanceOrdinal)
		h.createDPU(dpuName, true)

		h.reconcile()

		instance := h.current()
		requireCondition(t, instance, scalev1alpha1.SimInstanceDPUProvisioned, metav1.ConditionTrue, "")
		if instance.Status.DPUName != dpuName {
			t.Fatalf("status.dpuName = %q, want %q", instance.Status.DPUName, dpuName)
		}
	})

	t.Run("two DPUs naming the device are reported as duplicates", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		h.createDPU("dpu-a", true)
		h.createDPU("dpu-b", true)

		h.reconcile()

		requireCondition(t, h.current(), scalev1alpha1.SimInstanceDPUProvisioned, metav1.ConditionFalse, "DuplicateDPU")
	})

	t.Run("status.dpuName is cleared unless exactly one DPU names the device", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		first := h.createDPU("dpu-a", true)
		h.reconcile()
		if got := h.current().Status.DPUName; got != "dpu-a" {
			t.Fatalf("status.dpuName = %q, want dpu-a", got)
		}

		second := h.createDPU("dpu-b", true)
		h.reconcile()
		if got := h.current().Status.DPUName; got != "" {
			t.Fatalf("status.dpuName = %q with duplicate DPUs, want it cleared", got)
		}

		for _, dpu := range []*provisioningv1.DPU{first, second} {
			if err := h.client.Delete(context.Background(), dpu); err != nil {
				t.Fatalf("delete DPU %s: %v", dpu.Name, err)
			}
		}
		h.createDPU("dpu-a", true)
		h.reconcile()
		if got := h.current().Status.DPUName; got != "dpu-a" {
			t.Fatalf("status.dpuName = %q, want dpu-a", got)
		}
		if err := h.client.Delete(context.Background(), &provisioningv1.DPU{
			ObjectMeta: metav1.ObjectMeta{Name: "dpu-a", Namespace: h.fleet.Namespace},
		}); err != nil {
			t.Fatalf("delete DPU dpu-a: %v", err)
		}
		h.reconcile()
		if got := h.current().Status.DPUName; got != "" {
			t.Fatalf("status.dpuName = %q with no DPU, want it cleared", got)
		}
	})

	t.Run("no DPU is reported as missing", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())

		h.reconcile()

		requireCondition(t, h.current(), scalev1alpha1.SimInstanceDPUProvisioned, metav1.ConditionFalse, "DPUMissing")
		if phase := h.current().Status.Phase; phase != scalev1alpha1.SimInstanceProvisioning {
			t.Fatalf("phase = %q, want Provisioning", phase)
		}
	})

	t.Run("all conditions true records every child UID and reaches Ready", func(t *testing.T) {
		vm, vmi := seededGuest(instanceFleet(), "10.244.0.17")
		h := newInstanceHarness(t, instanceFleet(), vm, vmi)
		dpu := h.createDPU(dpuAgentCredentialName(h.fleet, instanceOrdinal), true)

		h.reconcile()

		instance := h.current()
		if instance.Status.Phase != scalev1alpha1.SimInstanceReady {
			t.Fatalf("phase = %q, want Ready; conditions = %+v", instance.Status.Phase, instance.Status.Conditions)
		}
		want := scalev1alpha1.ObservedUIDs{
			DPU:                    string(dpu.UID),
			DPUDevice:              string(h.device().UID),
			VirtualMachine:         string(h.virtualMachine().UID),
			VirtualMachineInstance: string(vmi.UID),
		}
		if want.DPU == "" || want.DPUDevice == "" || want.VirtualMachine == "" || want.VirtualMachineInstance == "" {
			t.Fatalf("fixture UIDs incomplete: %+v", want)
		}
		if !equality.Semantic.DeepEqual(instance.Status.ObservedUIDs, want) {
			t.Fatalf("observedUIDs = %+v, want %+v", instance.Status.ObservedUIDs, want)
		}
		if instance.Status.ObservedGeneration != instance.Generation {
			t.Fatalf("observedGeneration = %d, want %d", instance.Status.ObservedGeneration, instance.Generation)
		}
	})

	t.Run("a fleet without a VM template is Ready without guest UIDs", func(t *testing.T) {
		fleet := instanceFleet()
		fleet.Spec.VirtualMachineTemplate = nil
		h := newInstanceHarness(t, fleet)
		dpu := h.createDPU(dpuAgentCredentialName(fleet, instanceOrdinal), true)

		h.reconcile()

		instance := h.current()
		if instance.Status.Phase != scalev1alpha1.SimInstanceReady {
			t.Fatalf("phase = %q, want Ready; conditions = %+v", instance.Status.Phase, instance.Status.Conditions)
		}
		requireCondition(t, instance, scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "NotRequired")
		want := scalev1alpha1.ObservedUIDs{DPU: string(dpu.UID), DPUDevice: string(h.device().UID)}
		if !equality.Semantic.DeepEqual(instance.Status.ObservedUIDs, want) {
			t.Fatalf("observedUIDs = %+v, want %+v", instance.Status.ObservedUIDs, want)
		}
		if h.get(dpuAgentCredentialName(fleet, instanceOrdinal), &corev1.ServiceAccount{}) {
			t.Fatal("ServiceAccount created for a fleet that boots no guest")
		}
		if h.issuer.n != 0 {
			t.Fatalf("token issued %d times for a fleet that boots no guest", h.issuer.n)
		}
	})

	t.Run("a DPUDevice controlled by another owner fails the instance and is left alone", func(t *testing.T) {
		fleet := instanceFleet()
		foreign := desiredDevice(fleet, instanceOrdinal)
		foreign.Labels = map[string]string{"owner": "someone-else"}
		foreign.OwnerReferences = foreignOwner()
		h := newInstanceHarness(t, fleet, foreign)
		before := h.device()

		h.reconcile()

		instance := h.current()
		if instance.Status.Phase != scalev1alpha1.SimInstanceFailed || instance.Status.FailureReason != "OwnershipConflict" {
			t.Fatalf("phase/failureReason = %q/%q, want Failed/OwnershipConflict",
				instance.Status.Phase, instance.Status.FailureReason)
		}
		after := h.device()
		if after.ResourceVersion != before.ResourceVersion ||
			!equality.Semantic.DeepEqual(after.Labels, before.Labels) ||
			!equality.Semantic.DeepEqual(after.OwnerReferences, before.OwnerReferences) {
			t.Fatalf("foreign DPUDevice was modified: before %+v, after %+v", before.ObjectMeta, after.ObjectMeta)
		}
		if h.get(utils.VirtualMachineName(fleet.Name, instanceOrdinal), &virtv1.VirtualMachine{}) {
			t.Fatal("converge continued past the ownership conflict")
		}
	})

	t.Run("a missing fleet is not an error and creates nothing", func(t *testing.T) {
		h := newInstanceHarness(t, instanceFleet())
		if err := h.client.Delete(context.Background(), h.fleet); err != nil {
			t.Fatalf("delete fleet: %v", err)
		}

		h.reconcile()

		if h.get(utils.DeviceName(h.fleet.Name, instanceOrdinal), &provisioningv1.DPUDevice{}) {
			t.Fatal("DPUDevice created without an owning fleet")
		}
		if h.get(dpuAgentCredentialName(h.fleet, instanceOrdinal), &corev1.ServiceAccount{}) {
			t.Fatal("ServiceAccount created without an owning fleet")
		}
		if len(h.current().Finalizers) != 0 {
			t.Fatalf("finalizers = %v, want none without a fleet", h.current().Finalizers)
		}
	})
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
	_, labelled := obj.GetLabels()[labelFleetName]
	return labelScopedCacheKind(obj) && !labelled
}

func TestLabelScopedCacheKindMatchesManagerCacheOptions(t *testing.T) {
	byObject := ManagerCacheOptions().ByObject
	for obj := range byObject {
		if !labelScopedCacheKind(obj) {
			t.Errorf("labelScopedCacheKind(%T) = false, but ManagerCacheOptions scopes it by label", obj)
		}
	}
	if got := len(byObject); got != 4 {
		t.Errorf("ManagerCacheOptions scopes %d kinds, want the 4 labelScopedCacheKind knows", got)
	}
	if labelScopedCacheKind(&provisioningv1.DPUDevice{}) {
		t.Error("labelScopedCacheKind(DPUDevice) = true, want false")
	}
}

func TestSimInstanceCredentialsReadTheAPIServerPastTheCache(t *testing.T) {
	credentials := func(fleet *scalev1alpha1.SimFleet) *readyChildren {
		children := newReadyChildren(fleet)
		children.serviceAccount.UID = "sa-uid"
		return children
	}

	t.Run("a cloud-init Secret the cache has not seen yet mints no second token", func(t *testing.T) {
		fleet := instanceFleet()
		children := credentials(fleet)
		h := newInstanceHarness(t, fleet,
			children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
		h.reconciler.Client = cacheWithout(h.client, func(obj client.Object) bool {
			return obj.GetName() == children.cloudInit.Name
		})

		h.reconcile()

		if h.issuer.n != 0 {
			t.Fatalf("token issued %d times for an existing cloud-init Secret", h.issuer.n)
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "Ready")
	})

	t.Run("an owned cloud-init Secret that lost its labels is repaired, not reissued", func(t *testing.T) {
		fleet := instanceFleet()
		children := credentials(fleet)
		children.cloudInit.Labels = nil
		h := newInstanceHarness(t, fleet,
			children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
		h.reconciler.Client = cacheWithout(h.client, labelScoped)

		h.reconcile()

		if h.issuer.n != 0 {
			t.Fatalf("token issued %d times for an existing cloud-init Secret", h.issuer.n)
		}
		secret := &corev1.Secret{}
		if !h.get(children.cloudInit.Name, secret) {
			t.Fatal("cloud-init Secret is gone")
		}
		for key, want := range cloudInitSecretLabels(fleet, instanceOrdinal) {
			if secret.Labels[key] != want {
				t.Fatalf("Secret label %s = %q, want %q restored", key, secret.Labels[key], want)
			}
		}
	})

	t.Run("an owned ServiceAccount that lost its labels is repaired on AlreadyExists", func(t *testing.T) {
		fleet := instanceFleet()
		children := credentials(fleet)
		children.serviceAccount.Labels = nil
		h := newInstanceHarness(t, fleet,
			children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
		h.reconciler.Client = cacheWithout(h.client, labelScoped)

		h.reconcile()

		account := &corev1.ServiceAccount{}
		if !h.get(children.serviceAccount.Name, account) {
			t.Fatal("ServiceAccount is gone")
		}
		for key, want := range dpuAgentCredentialLabels(fleet, instanceOrdinal) {
			if account.Labels[key] != want {
				t.Fatalf("ServiceAccount label %s = %q, want %q restored", key, account.Labels[key], want)
			}
		}
		requireCondition(t, h.current(), scalev1alpha1.SimInstanceCredentialsReady, metav1.ConditionTrue, "Ready")
	})

	t.Run("a label-stripped Secret held by another owner fails the instance", func(t *testing.T) {
		fleet := instanceFleet()
		children := credentials(fleet)
		children.cloudInit.Labels = nil
		children.cloudInit.OwnerReferences = foreignOwner()
		h := newInstanceHarness(t, fleet,
			children.serviceAccount, children.role, children.roleBinding, children.cloudInit)
		h.reconciler.Client = cacheWithout(h.client, labelScoped)

		h.reconcile()

		if h.issuer.n != 0 {
			t.Fatalf("token issued %d times for a Secret name another owner holds", h.issuer.n)
		}
		if instance := h.current(); instance.Status.FailureReason != "OwnershipConflict" {
			t.Fatalf("failureReason = %q, want OwnershipConflict", instance.Status.FailureReason)
		}
		secret := &corev1.Secret{}
		if !h.get(children.cloudInit.Name, secret) || len(secret.Labels) != 0 {
			t.Fatalf("foreign Secret was modified: %+v", secret.ObjectMeta)
		}
	})
}

func TestMapFleetToInstances(t *testing.T) {
	fleet := instanceFleet()
	labelled := func(ordinal int) *scalev1alpha1.SimInstance {
		instance := newInstanceFixture(fleet, ordinal)
		instance.Labels = map[string]string{labelFleetName: fleet.Name}
		return instance
	}
	owned := labelled(0)
	earlierFleet := labelled(1)
	earlierFleet.OwnerReferences[0].UID = "earlier-fleet-uid"
	unlabelled := newInstanceFixture(fleet, 2)
	otherNamespace := labelled(3)
	otherNamespace.Namespace = "other"
	h := newInstanceHarness(t, fleet, owned, earlierFleet, unlabelled, otherNamespace)

	got := h.reconciler.mapFleetToInstances(context.Background(), fleet)

	want := []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(owned)}}
	if !equality.Semantic.DeepEqual(got, want) {
		t.Fatalf("mapFleetToInstances() = %v, want %v", got, want)
	}
	if got := h.reconciler.mapFleetToInstances(context.Background(), &corev1.Secret{}); got != nil {
		t.Fatalf("mapFleetToInstances(Secret) = %v, want nil", got)
	}
}

func TestKubeletConfigured(t *testing.T) {
	if kubeletConfigured(nil) {
		t.Fatal("kubeletConfigured(nil) = true")
	}
	dpu := &provisioningv1.DPU{}
	if kubeletConfigured(dpu) {
		t.Fatal("kubeletConfigured() = true without agent status")
	}

	dpu.Status.AgentStatus = &provisioningv1.AgentStatus{}
	meta.SetStatusCondition(&dpu.Status.AgentStatus.Conditions, metav1.Condition{
		Type:   "KubeletConfigured",
		Status: metav1.ConditionFalse,
	})
	if kubeletConfigured(dpu) {
		t.Fatal("kubeletConfigured() = true for false condition")
	}
	meta.SetStatusCondition(&dpu.Status.AgentStatus.Conditions, metav1.Condition{
		Type:   "KubeletConfigured",
		Status: metav1.ConditionTrue,
	})
	if !kubeletConfigured(dpu) {
		t.Fatal("kubeletConfigured() = false for true condition")
	}
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
	serviceAccount := desiredDPUAgentServiceAccount(fleet, instanceOrdinal)
	serviceAccount.OwnerReferences = owned
	role := desiredDPUAgentRole(fleet, instanceOrdinal)
	role.OwnerReferences = owned
	roleBinding := desiredDPUAgentRoleBinding(fleet, instanceOrdinal)
	roleBinding.OwnerReferences = owned
	cloudInit := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:            utils.CloudInitSecretName(fleet.Name, instanceOrdinal),
		Namespace:       fleet.Namespace,
		Labels:          cloudInitSecretLabels(fleet, instanceOrdinal),
		OwnerReferences: owned,
	}}
	device := desiredDevice(fleet, instanceOrdinal)
	device.UID = types.UID(recordedUIDs.DPUDevice)
	device.OwnerReferences = owned
	device.Spec.BMCIP = ptr.To("10.244.0.17")
	mergeLabels(device, deviceSelectionLabels(fleet))
	vm, vmi := seededGuest(fleet, "10.244.0.17")
	dpuName := utils.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, instanceOrdinal)
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
			Name:      utils.KubeadmJoinSecretName(dpuName),
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

func foreignOwner() []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: "example.com/v1",
		Kind:       "Other",
		Name:       "other",
		UID:        types.UID("other-uid"),
		Controller: ptr.To(true),
	}}
}

type teardownHarness struct {
	*instanceHarness
	now      time.Time
	recorder *events.FakeRecorder
}

// newReadyInstanceHarness is an instance that converged: it holds the
// teardown finalizer, the fleet-name label, and the recorded child UIDs.
func newReadyInstanceHarness(t *testing.T, fleet *scalev1alpha1.SimFleet, children *readyChildren) *teardownHarness {
	t.Helper()
	th := &teardownHarness{
		instanceHarness: newInstanceHarness(t, fleet, children.objects()...),
		now:             teardownClock,
		recorder:        events.NewFakeRecorder(100),
	}
	th.reconciler.Now = func() time.Time { return th.now }
	th.reconciler.Recorder = th.recorder

	instance := th.current()
	controllerutil.AddFinalizer(instance, simInstanceTeardownFinalizer)
	instance.Labels = map[string]string{labelFleetName: fleet.Name}
	th.update(instance)
	instance.Status.Phase = scalev1alpha1.SimInstanceReady
	instance.Status.ObservedUIDs = recordedUIDs
	if err := th.client.Status().Update(context.Background(), instance); err != nil {
		t.Fatalf("update SimInstance status: %v", err)
	}
	return th
}

func (th *teardownHarness) deleteInstance() {
	th.t.Helper()
	if err := th.client.Delete(context.Background(), th.current()); err != nil {
		th.t.Fatalf("delete SimInstance: %v", err)
	}
}

func (th *teardownHarness) reconcileResult() ctrl.Result {
	th.t.Helper()
	result, err := th.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(th.instance),
	})
	if err != nil {
		th.t.Fatalf("Reconcile() error = %v", err)
	}
	return result
}

func (th *teardownHarness) instanceGone() bool {
	th.t.Helper()
	return !th.get(th.instance.Name, &scalev1alpha1.SimInstance{})
}

// runTeardown reconciles until the finalizer is removed and the instance is
// gone.
func (th *teardownHarness) runTeardown() {
	th.t.Helper()
	for range 12 {
		if th.instanceGone() {
			return
		}
		th.reconcileResult()
	}
	th.t.Fatalf("teardown did not finish; instance = %+v", th.current().Status)
}

func (th *teardownHarness) exists(obj client.Object) bool {
	th.t.Helper()
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

func TestSimInstanceDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*readyChildren)
		reason string
	}{
		{"DPU UID changed", func(c *readyChildren) { c.dpu.UID = "new-dpu-uid" }, "DPUReplaced"},
		{"DPUDevice UID changed", func(c *readyChildren) { c.device.UID = "new-device-uid" }, "DPUDeviceReplaced"},
		{"VirtualMachine UID changed", func(c *readyChildren) { c.vm.UID = "new-vm-uid" }, "VirtualMachineReplaced"},
		{"VirtualMachineInstance UID changed", func(c *readyChildren) { c.vmi.UID = "new-vmi-uid" }, "VirtualMachineInstanceReplaced"},
		{"DPU missing", func(c *readyChildren) { c.dpu = nil }, "DPUReplaced"},
		{"DPUDevice missing", func(c *readyChildren) { c.device = nil }, "DPUDeviceReplaced"},
		{"VirtualMachine missing", func(c *readyChildren) { c.vm = nil }, "VirtualMachineReplaced"},
		{"VirtualMachineInstance missing", func(c *readyChildren) { c.vmi = nil }, "VirtualMachineInstanceReplaced"},
	} {
		t.Run(tc.name+" fails the instance and converges nothing", func(t *testing.T) {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			tc.mutate(children)
			th := newReadyInstanceHarness(t, fleet, children)
			writes := th.countWrites(false)

			th.reconcile()

			instance := th.current()
			if instance.Status.Phase != scalev1alpha1.SimInstanceFailed || instance.Status.FailureReason != tc.reason {
				t.Fatalf("phase/failureReason = %q/%q, want Failed/%s",
					instance.Status.Phase, instance.Status.FailureReason, tc.reason)
			}
			if got := th.events(); !hasEvent(got, "Normal SimInstanceFailed") {
				t.Fatalf("Events = %q, want a Normal SimInstanceFailed Event", got)
			}

			th.reconcile()
			if *writes != 0 {
				t.Fatalf("reconciler wrote %d children of a drifted instance", *writes)
			}
			if phase := th.current().Status.Phase; phase != scalev1alpha1.SimInstanceFailed {
				t.Fatalf("phase = %q after another Reconcile, want Failed", phase)
			}
		})
	}

	t.Run("recorded UIDs that still match are not drift", func(t *testing.T) {
		fleet := instanceFleet()
		th := newReadyInstanceHarness(t, fleet, newReadyChildren(fleet))

		th.reconcile()

		instance := th.current()
		if instance.Status.Phase == scalev1alpha1.SimInstanceFailed || instance.Status.FailureReason != "" {
			t.Fatalf("phase/failureReason = %q/%q, want no drift", instance.Status.Phase, instance.Status.FailureReason)
		}
	})

	t.Run("the recorded DPU under another name is not drift", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.dpu.Name = "dpf-chose-another-name"
		th := newReadyInstanceHarness(t, fleet, children)

		th.reconcile()

		instance := th.current()
		if instance.Status.Phase == scalev1alpha1.SimInstanceFailed || instance.Status.FailureReason != "" {
			t.Fatalf("phase/failureReason = %q/%q, want no drift", instance.Status.Phase, instance.Status.FailureReason)
		}
	})

	t.Run("a DPU of the recorded name that names another device is drift", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.dpu.Spec.DPUDeviceName = "someone-else-device"
		th := newReadyInstanceHarness(t, fleet, children)

		th.reconcile()

		if reason := th.current().Status.FailureReason; reason != "DPUReplaced" {
			t.Fatalf("failureReason = %q, want DPUReplaced", reason)
		}
	})

	t.Run("a fleet without a VM template has no guest to drift", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		fleet.Spec.VirtualMachineTemplate = nil
		children.vm, children.vmi = nil, nil
		children.serviceAccount, children.role, children.roleBinding, children.cloudInit = nil, nil, nil, nil
		th := newReadyInstanceHarness(t, fleet, children)
		instance := th.current()
		instance.Status.ObservedUIDs = scalev1alpha1.ObservedUIDs{
			DPU:       recordedUIDs.DPU,
			DPUDevice: recordedUIDs.DPUDevice,
		}
		if err := th.client.Status().Update(context.Background(), instance); err != nil {
			t.Fatalf("update SimInstance status: %v", err)
		}

		th.reconcile()

		instance = th.current()
		if instance.Status.Phase == scalev1alpha1.SimInstanceFailed || instance.Status.FailureReason != "" {
			t.Fatalf("phase/failureReason = %q/%q, want no drift", instance.Status.Phase, instance.Status.FailureReason)
		}
	})

	t.Run("no recorded UIDs is not drift", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.dpu.UID = "any-dpu-uid"
		h := newInstanceHarness(t, fleet, children.dpu)

		h.reconcile()

		instance := h.current()
		if instance.Status.Phase == scalev1alpha1.SimInstanceFailed || instance.Status.FailureReason != "" {
			t.Fatalf("phase/failureReason = %q/%q, want no drift", instance.Status.Phase, instance.Status.FailureReason)
		}
	})
}

func deleteRecord(obj client.Object) string {
	return fmt.Sprintf("%T %s", obj, obj.GetName())
}

func TestSimInstanceTeardown(t *testing.T) {
	t.Run("children are deleted in order and the finalizer is removed last", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		th := newReadyInstanceHarness(t, fleet, children)
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
					for key := range deviceSelectionLabels(fleet) {
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
			if !equality.Semantic.DeepEqual(round, wantRound.deleted) {
				t.Fatalf("Reconcile %d deleted %q, want %q", i+1, round, wantRound.deleted)
			}
			if result != (ctrl.Result{RequeueAfter: wantRound.requeue}) {
				t.Fatalf("Reconcile %d result = %+v, want RequeueAfter %s", i+1, result, wantRound.requeue)
			}
			if i == 0 {
				instance := th.current()
				if instance.Status.Phase != scalev1alpha1.SimInstanceTerminating {
					t.Fatalf("phase = %q, want Terminating", instance.Status.Phase)
				}
				if instance.Status.TeardownStartedAt == nil {
					t.Fatal("status.teardownStartedAt is not set")
				}
				requireCondition(t, instance, scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteDPU")
			}
		}
		if !quarantinedFirst {
			t.Fatal("DPU was deleted before the DPUDevice lost its selection labels")
		}
		if !th.instanceGone() {
			t.Fatalf("SimInstance still exists with finalizers %v", th.current().Finalizers)
		}
	})

	t.Run("a DPU naming a different device is left and teardown continues", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.dpu.Spec.DPUDeviceName = "someone-else-device"
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.runTeardown()

		if !th.exists(children.dpu) {
			t.Fatal("DPU naming another device was deleted")
		}
		if !th.exists(children.joinSecret) {
			t.Fatal("join Secret of a DPU that is not ours was deleted")
		}
		if th.exists(children.device) {
			t.Fatal("DPUDevice was not deleted")
		}
	})

	t.Run("a DPU whose device is gone is deleted only when its UID was recorded", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			uid     types.UID
			deleted bool
		}{
			{"recorded UID", types.UID(recordedUIDs.DPU), true},
			{"unrecorded UID", "other-dpu-uid", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fleet := instanceFleet()
				children := newReadyChildren(fleet)
				children.device = nil
				children.dpu.UID = tc.uid
				th := newReadyInstanceHarness(t, fleet, children)
				th.deleteInstance()

				th.runTeardown()

				if deleted := !th.exists(children.dpu); deleted != tc.deleted {
					t.Fatalf("DPU UID %s deleted = %t, want %t", tc.uid, deleted, tc.deleted)
				}
			})
		}
	})

	t.Run("a VMI owned by another VM is left", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.vm = nil
		children.vmi.OwnerReferences[0].UID = "other-vm-uid"
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.runTeardown()

		if !th.exists(children.vmi) {
			t.Fatal("VMI owned by another VirtualMachine was deleted")
		}
	})

	t.Run("a VMI whose VM is gone is deleted when both UIDs were recorded", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.vm = nil
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.runTeardown()

		if th.exists(children.vmi) {
			t.Fatal("VMI of the recorded VirtualMachine was not deleted")
		}
	})

	t.Run("a step blocked by a finalizer is forced only after lifecycleForceTimeout", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.device.Finalizers = []string{"provisioning.dpu.nvidia.com/dpudevice-protection"}
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.reconcileResult() // quarantine and delete the DPU
		th.reconcileResult() // delete the DPUDevice, which its finalizer holds
		device := &provisioningv1.DPUDevice{}
		if !th.get(children.device.Name, device) || device.DeletionTimestamp.IsZero() {
			t.Fatal("DPUDevice was not marked for deletion")
		}

		th.now = teardownClock.Add(lifecycleForceTimeout - time.Second)
		th.reconcileResult()
		if !th.get(children.device.Name, device) || len(device.Finalizers) == 0 {
			t.Fatalf("DPUDevice finalizers stripped before lifecycleForceTimeout: %+v", device.ObjectMeta)
		}
		if got := th.events(); hasEvent(got, "Warning LifecycleForceDelete") {
			t.Fatalf("Events = %q, want no LifecycleForceDelete before the timeout", got)
		}
		requireCondition(t, th.current(), scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteDPUDevice")

		th.now = teardownClock.Add(lifecycleForceTimeout)
		th.reconcileResult()
		if th.exists(children.device) {
			t.Fatal("DPUDevice still exists after lifecycleForceTimeout")
		}
		if got := th.events(); !hasEvent(got, "Warning LifecycleForceDelete") {
			t.Fatalf("Events = %q, want a LifecycleForceDelete Warning", got)
		}

		th.runTeardown()
	})

	t.Run("the next step's deadline starts when the previous step finishes", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.vm.Finalizers = []string{"kubevirt.io/virtualMachineControllerFinalize"}
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.reconcileResult() // quarantine and delete the DPU
		th.now = teardownClock.Add(lifecycleForceTimeout)
		th.reconcileResult() // delete the DPUDevice
		th.reconcileResult() // delete the VirtualMachine, which its finalizer holds
		th.reconcileResult() // the VirtualMachine is still blocked, within its own deadline
		vm := &virtv1.VirtualMachine{}
		if !th.get(children.vm.Name, vm) || len(vm.Finalizers) == 0 {
			t.Fatal("VirtualMachine was forced on the deadline of an earlier step")
		}
		requireCondition(t, th.current(), scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteVM")
	})

	t.Run("a blocked step requeues for what is left of its force deadline", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.device.Finalizers = []string{"provisioning.dpu.nvidia.com/dpudevice-protection"}
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.reconcileResult() // quarantine and delete the DPU
		th.reconcileResult() // delete the DPUDevice, which its finalizer holds
		th.now = teardownClock.Add(time.Minute)
		if got := th.reconcileResult(); got.RequeueAfter != lifecycleForceTimeout-time.Minute {
			t.Fatalf("result = %+v, want RequeueAfter %s", got, lifecycleForceTimeout-time.Minute)
		}
		th.now = teardownClock.Add(lifecycleForceTimeout - time.Millisecond)
		if got := th.reconcileResult(); got.RequeueAfter != teardownPoll {
			t.Fatalf("result = %+v, want the %s minimum", got, teardownPoll)
		}
	})

	t.Run("the VMI gets its own force deadline once its VirtualMachine is gone", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.vm.Finalizers = []string{"kubevirt.io/virtualMachineControllerFinalize"}
		children.vmi.Finalizers = []string{"kubevirt.io/foregroundDeleteVirtualMachine"}
		th := newReadyInstanceHarness(t, fleet, children)
		th.deleteInstance()

		th.reconcileResult() // quarantine and delete the DPU
		th.reconcileResult() // delete the DPUDevice
		th.reconcileResult() // delete the VirtualMachine, which its finalizer holds
		th.now = teardownClock.Add(lifecycleForceTimeout - time.Second)
		th.reconcileResult()
		vm := &virtv1.VirtualMachine{}
		if !th.get(children.vm.Name, vm) {
			t.Fatal("VirtualMachine was forced before its deadline")
		}
		vm.Finalizers = nil
		th.update(vm) // KubeVirt lets go just before the deadline

		th.now = teardownClock.Add(lifecycleForceTimeout)
		if got := th.reconcileResult(); got.RequeueAfter != teardownPoll {
			t.Fatalf("result = %+v, want the VMI step to poll every %s", got, teardownPoll)
		}
		requireCondition(t, th.current(), scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, "DeleteVMI")
		th.now = th.now.Add(time.Second)
		th.reconcileResult()
		vmi := &virtv1.VirtualMachineInstance{}
		if !th.get(children.vmi.Name, vmi) || len(vmi.Finalizers) == 0 {
			t.Fatal("VMI was forced on its VirtualMachine's deadline")
		}
		if got := th.events(); hasEvent(got, "Warning LifecycleForceDelete") {
			t.Fatalf("Events = %q, want no LifecycleForceDelete yet", got)
		}

		th.now = teardownClock.Add(2 * lifecycleForceTimeout)
		th.reconcileResult()
		if th.exists(children.vmi) {
			t.Fatal("VMI still exists after its own lifecycleForceTimeout")
		}
		th.runTeardown()
	})

	t.Run("a deleting fleet still names the selection labels quarantine strips", func(t *testing.T) {
		fleet := instanceFleet()
		fleet.Finalizers = []string{simFleetTeardownFinalizer}
		now := metav1.Now()
		fleet.DeletionTimestamp = &now
		children := newReadyChildren(fleet)
		th := newReadyInstanceHarness(t, fleet, children)
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

		if !dpuDeleted {
			t.Fatal("DPU was not deleted")
		}
		for key := range deviceSelectionLabels(fleet) {
			if _, ok := labelsAtDPUDelete[key]; ok {
				t.Fatalf("DPUDevice labels at DPU delete = %v, want %s stripped", labelsAtDPUDelete, key)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		orphan func(*scalev1alpha1.SimInstance)
	}{
		{"its label", func(instance *scalev1alpha1.SimInstance) { instance.OwnerReferences = nil }},
		{"its owner reference", func(instance *scalev1alpha1.SimInstance) { instance.Labels = nil }},
	} {
		t.Run("teardown completes with the fleet gone, named by "+tc.name, func(t *testing.T) {
			fleet := instanceFleet()
			children := newReadyChildren(fleet)
			th := newReadyInstanceHarness(t, fleet, children)
			instance := th.current()
			tc.orphan(instance)
			th.update(instance)
			if err := th.client.Delete(context.Background(), fleet); err != nil {
				t.Fatalf("delete fleet: %v", err)
			}
			th.deleteInstance()

			th.runTeardown()

			for _, obj := range children.objects() {
				if th.exists(obj) {
					t.Fatalf("%T %s survived teardown without its fleet", obj, obj.GetName())
				}
			}
		})
	}

	t.Run("an instance that names no fleet is released without touching anything", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		th := newReadyInstanceHarness(t, fleet, children)
		instance := th.current()
		instance.Labels = nil
		instance.OwnerReferences = nil
		th.update(instance)
		writes := th.countWrites(false)
		th.deleteInstance()

		th.runTeardown()

		if *writes != 0 {
			t.Fatalf("reconciler wrote %d children it cannot name", *writes)
		}
		for _, obj := range children.objects() {
			if !th.exists(obj) {
				t.Fatalf("%T %s was deleted by an instance that names no fleet", obj, obj.GetName())
			}
		}
	})

	t.Run("a failing step persists its progress and returns the error", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		th := newReadyInstanceHarness(t, fleet, children)
		th.reconciler.Client = interceptor.NewClient(th.client.(client.WithWatch), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*provisioningv1.DPU); ok {
					return fmt.Errorf("DPU API unavailable")
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
		th.deleteInstance()

		_, err := th.reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(th.instance),
		})

		if err == nil || !strings.Contains(err.Error(), "DPU API unavailable") {
			t.Fatalf("Reconcile() error = %v, want the DPU delete failure", err)
		}
		instance := th.current()
		if instance.Status.Phase != scalev1alpha1.SimInstanceTerminating || instance.Status.TeardownStartedAt == nil {
			t.Fatalf("phase/teardownStartedAt = %q/%v, want Terminating and set",
				instance.Status.Phase, instance.Status.TeardownStartedAt)
		}
		requireCondition(t, instance, scalev1alpha1.SimInstanceTeardown, metav1.ConditionTrue, teardownDeleteDPU)
		if !controllerutil.ContainsFinalizer(instance, simInstanceTeardownFinalizer) {
			t.Fatal("finalizer removed after a failed step")
		}
	})

	t.Run("children controlled by another owner are left untouched", func(t *testing.T) {
		fleet := instanceFleet()
		children := newReadyChildren(fleet)
		children.device.OwnerReferences = foreignOwner()
		children.vm.OwnerReferences = foreignOwner()
		children.role.OwnerReferences = foreignOwner()
		th := newReadyInstanceHarness(t, fleet, children)
		before := map[string]client.Object{}
		for _, obj := range []client.Object{children.device, children.vm, children.vmi, children.role, children.dpu} {
			live := obj.DeepCopyObject().(client.Object)
			if !th.get(obj.GetName(), live) {
				t.Fatalf("%T %s was not seeded", obj, obj.GetName())
			}
			before[deleteRecord(obj)] = live
		}
		th.deleteInstance()

		th.runTeardown()

		for key, want := range before {
			got := want.DeepCopyObject().(client.Object)
			if !th.get(want.GetName(), got) {
				t.Fatalf("%s was deleted", key)
			}
			if got.GetResourceVersion() != want.GetResourceVersion() || !got.GetDeletionTimestamp().IsZero() {
				t.Fatalf("%s was modified: before %+v, after %+v", key, want, got)
			}
		}
		if !th.exists(children.joinSecret) {
			t.Fatal("join Secret of a DPU that is not ours was deleted")
		}
		if th.exists(children.serviceAccount) {
			t.Fatal("ServiceAccount the instance owns was not deleted")
		}
	})
}
