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
	"testing"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

func virtualMachineFleet(name, namespace string) *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              1,
			DevicesPerDPUNode:     1,
			DeviceSelectionLabels: map[string]string{"scale.dpu.nvidia.com/pool": "scale-3000"},
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
				Image: "example.com/node@sha256:test",
			},
		},
	}
}

// startFleetManager runs both reconcilers the way cmd/main.go wires them and
// returns a function that stops the manager and waits for it to exit.
func startFleetManager() func() {
	GinkgoHelper()
	return startManager(true)
}

// startManager runs the SimInstance reconciler, and the SimFleet reconciler
// when withFleet is set, and returns a function that stops the manager and
// waits for it to exit.
func startManager(withFleet bool) func() {
	GinkgoHelper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Cache:                  ManagerCacheOptions(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(SetupIndexes(ctx, mgr)).To(Succeed())
	issuer := &countingServiceAccountIssuer{}
	if withFleet {
		Expect((&SimFleetReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr)).To(Succeed())
	}
	Expect((&SimInstanceReconciler{
		Client:                    mgr.GetClient(),
		APIReader:                 mgr.GetAPIReader(),
		Scheme:                    mgr.GetScheme(),
		ServiceAccountTokenIssuer: issuer,
		ManagementCA:              []byte("test-ca"),
	}).SetupWithManager(mgr)).To(Succeed())

	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	stop := func() {
		cancel()
		Eventually(done, time.Minute).Should(BeClosed())
	}
	DeferCleanup(stop)
	return stop
}

type ordinalChild struct {
	name string
	obj  client.Object
}

// ordinalChildren are the objects a SimInstance creates for its ordinal.
func ordinalChildren(fleet *scalev1alpha1.SimFleet, ordinal int) []ordinalChild {
	credentialName := dpuAgentCredentialName(fleet, ordinal)
	return []ordinalChild{
		{credentialName, &corev1.ServiceAccount{}},
		{credentialName, &rbacv1.Role{}},
		{credentialName, &rbacv1.RoleBinding{}},
		{utils.CloudInitSecretName(fleet.Name, ordinal), &corev1.Secret{}},
		{utils.DeviceName(fleet.Name, ordinal), &provisioningv1.DPUDevice{}},
		{utils.VirtualMachineName(fleet.Name, ordinal), &virtv1.VirtualMachine{}},
	}
}

func getFleetInstance(g Gomega, fleet *scalev1alpha1.SimFleet, ordinal int) *scalev1alpha1.SimInstance {
	instance := &scalev1alpha1.SimInstance{}
	key := client.ObjectKey{Name: utils.InstanceName(fleet.Name, ordinal), Namespace: fleet.Namespace}
	g.Expect(k8sClient.Get(ctx, key, instance)).To(Succeed())
	return instance
}

// removeWithoutFinalizers deletes obj so that it is gone at once, as if
// whatever holds it had already let go.
func removeWithoutFinalizers(obj client.Object) {
	GinkgoHelper()
	base := obj.DeepCopyObject().(client.Object)
	obj.SetFinalizers(nil)
	Expect(k8sClient.Patch(ctx, obj, client.MergeFrom(base))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
	Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object)))
	}, 30*time.Second, 100*time.Millisecond).Should(BeTrue(), "%T %s still exists", obj, obj.GetName())
}

var _ = Describe("SimFleet reconciler", func() {
	var fleet *scalev1alpha1.SimFleet

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "simfleet-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		fleet = virtualMachineFleet("rollout", namespace.Name)
		fleet.Spec.Replicas = 3
		fleet.Spec.DevicesPerDPUNode = 2
		Expect(k8sClient.Create(ctx, fleet)).To(Succeed())
	})

	// expectConverged waits for one SimInstance per ordinal with every child
	// controlled by it, and for one DPUNode per host.
	expectConverged := func() {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			for ordinal := range fleet.Spec.Replicas {
				instance := getFleetInstance(g, fleet, ordinal)
				g.Expect(controlledBy(instance, scalev1alpha1.GroupVersion.String(), "SimFleet", fleet.Name, fleet.UID)).
					To(BeTrue(), "SimInstance %d owner = %+v", ordinal, instance.OwnerReferences)
				g.Expect(instanceConditionTrue(instance, scalev1alpha1.SimInstanceChildrenCreated)).To(BeTrue(),
					"SimInstance %d conditions = %+v", ordinal, instance.Status.Conditions)
				for _, child := range []ordinalChild{
					{utils.DeviceName(fleet.Name, ordinal), &provisioningv1.DPUDevice{}},
					{utils.VirtualMachineName(fleet.Name, ordinal), &virtv1.VirtualMachine{}},
				} {
					key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
					g.Expect(k8sClient.Get(ctx, key, child.obj)).To(Succeed(), "get %T %s", child.obj, child.name)
					g.Expect(controlledBy(child.obj, scalev1alpha1.GroupVersion.String(), "SimInstance",
						instance.Name, instance.UID)).
						To(BeTrue(), "%T %s owner = %+v", child.obj, child.name, child.obj.GetOwnerReferences())
				}
			}
			for hostIndex := range utils.DesiredNodeCount(fleet.Spec.Replicas, fleet.Spec.DevicesPerDPUNode) {
				node := &provisioningv1.DPUNode{}
				key := client.ObjectKey{Name: utils.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
				g.Expect(k8sClient.Get(ctx, key, node)).To(Succeed(), "get DPUNode host %d", hostIndex)
				g.Expect(controlledBy(node, scalev1alpha1.GroupVersion.String(), "SimFleet", fleet.Name, fleet.UID)).
					To(BeTrue(), "DPUNode host %d owner = %+v", hostIndex, node.OwnerReferences)
			}
			current := &scalev1alpha1.SimFleet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), current)).To(Succeed())
			g.Expect(current.Status.AdmittedReplicas).To(Equal(fleet.Spec.Replicas))
			g.Expect(current.Status.CreatedInstances).To(Equal(fleet.Spec.Replicas))
			g.Expect(current.Status.DPUNodes).To(Equal(2))
		}, time.Minute, 100*time.Millisecond).Should(Succeed())
	}

	It("rolls a fleet out through one SimInstance per ordinal", func() {
		startFleetManager()

		expectConverged()

		instances := &scalev1alpha1.SimInstanceList{}
		Expect(k8sClient.List(ctx, instances, client.InNamespace(fleet.Namespace))).To(Succeed())
		Expect(instances.Items).To(HaveLen(3))
		devices := &provisioningv1.DPUDeviceList{}
		Expect(k8sClient.List(ctx, devices, client.InNamespace(fleet.Namespace))).To(Succeed())
		Expect(devices.Items).To(HaveLen(3))
		machines := &virtv1.VirtualMachineList{}
		Expect(k8sClient.List(ctx, machines, client.InNamespace(fleet.Namespace))).To(Succeed())
		Expect(machines.Items).To(HaveLen(3))
	})

	It("recreates objects deleted while no manager was running", func() {
		stop := startFleetManager()
		expectConverged()
		stop()

		device := &provisioningv1.DPUDevice{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: utils.DeviceName(fleet.Name, 0), Namespace: fleet.Namespace,
		}, device)).To(Succeed())
		oldDeviceUID := device.UID
		removeWithoutFinalizers(device)

		instance := &scalev1alpha1.SimInstance{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: utils.InstanceName(fleet.Name, 1), Namespace: fleet.Namespace,
		}, instance)).To(Succeed())
		oldInstanceUID := instance.UID
		removeWithoutFinalizers(instance)
		// envtest runs no garbage collector; remove what it would have
		// collected once the instance was gone.
		for _, child := range ordinalChildren(fleet, 1) {
			key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
			if err := k8sClient.Get(ctx, key, child.obj); err == nil {
				removeWithoutFinalizers(child.obj)
			}
		}

		startFleetManager()

		Eventually(func(g Gomega) {
			recreated := &provisioningv1.DPUDevice{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: utils.DeviceName(fleet.Name, 0), Namespace: fleet.Namespace,
			}, recreated)).To(Succeed())
			g.Expect(recreated.UID).NotTo(Equal(oldDeviceUID))
			g.Expect(getFleetInstance(g, fleet, 1).UID).NotTo(Equal(oldInstanceUID))
		}, time.Minute, 100*time.Millisecond).Should(Succeed())
		expectConverged()
	})

	// envtest runs no garbage collector, so the fleet deletion is what
	// kubectl's background propagation leaves to the controller: the fleet
	// only gets a deletionTimestamp.
	It("holds a deleted fleet until its instances are torn down with the fleet readable", func() {
		startFleetManager()
		expectConverged()

		for ordinal := range fleet.Spec.Replicas {
			vm := &virtv1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: utils.VirtualMachineName(fleet.Name, ordinal), Namespace: fleet.Namespace,
			}, vm)).To(Succeed())
			bootGuest(vm, fmt.Sprintf("10.244.0.%d", 10+ordinal))
		}
		Eventually(func(g Gomega) {
			for ordinal := range fleet.Spec.Replicas {
				device := &provisioningv1.DPUDevice{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{
					Name: utils.DeviceName(fleet.Name, ordinal), Namespace: fleet.Namespace,
				}, device)).To(Succeed())
				for key, value := range fleet.Spec.DeviceSelectionLabels {
					g.Expect(device.Labels).To(HaveKeyWithValue(key, value), "DPUDevice %d", ordinal)
				}
			}
		}, time.Minute, 100*time.Millisecond).Should(Succeed())

		watcher, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		deviceEvents, err := watcher.Watch(ctx, &provisioningv1.DPUDeviceList{}, client.InNamespace(fleet.Namespace))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(deviceEvents.Stop)
		// deletedQuarantined records, per device, whether the object its
		// delete event carried had already lost every selection label.
		deletedQuarantined := map[string]bool{}
		recordDeviceEvents := func() {
			for {
				select {
				case event, open := <-deviceEvents.ResultChan():
					if !open {
						return
					}
					device, ok := event.Object.(*provisioningv1.DPUDevice)
					if !ok || event.Type != watch.Deleted {
						continue
					}
					selected := false
					for key := range fleet.Spec.DeviceSelectionLabels {
						if _, ok := device.Labels[key]; ok {
							selected = true
						}
					}
					deletedQuarantined[device.Name] = !selected
				default:
					return
				}
			}
		}

		Expect(k8sClient.Delete(ctx, fleet)).To(Succeed())

		Eventually(func(g Gomega) {
			recordDeviceEvents()
			// The fleet is read first, so instances listed after it was
			// found gone really did outlive it.
			current := &scalev1alpha1.SimFleet{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), current)
			instances := &scalev1alpha1.SimInstanceList{}
			g.Expect(k8sClient.List(ctx, instances, client.InNamespace(fleet.Namespace))).To(Succeed())
			if len(instances.Items) > 0 {
				if err != nil {
					StopTrying(fmt.Sprintf("SimFleet is gone while %d SimInstances remain", len(instances.Items))).
						Wrap(err).Now()
				}
				g.Expect(current.DeletionTimestamp.IsZero()).To(BeFalse())
				g.Expect(current.Finalizers).To(ContainElement(simFleetTeardownFinalizer))
			}
			g.Expect(instances.Items).To(BeEmpty())
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "SimFleet still exists: %v", err)
		}, time.Minute, 100*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			recordDeviceEvents()
			for ordinal := range fleet.Spec.Replicas {
				name := utils.DeviceName(fleet.Name, ordinal)
				wasQuarantined, deleted := deletedQuarantined[name]
				g.Expect(deleted).To(BeTrue(), "no delete event for DPUDevice %s", name)
				if !wasQuarantined {
					StopTrying("DPUDevice " + name + " was deleted with its selection labels").Now()
				}
			}
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		// The DPUNodes are left to garbage collection, which envtest does not run.
		for hostIndex := range utils.DesiredNodeCount(fleet.Spec.Replicas, fleet.Spec.DevicesPerDPUNode) {
			node := &provisioningv1.DPUNode{}
			key := client.ObjectKey{Name: utils.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
			Expect(k8sClient.Get(ctx, key, node)).To(Succeed(), "DPUNode host %d", hostIndex)
			Expect(node.DeletionTimestamp.IsZero()).To(BeTrue(), "DPUNode host %d was deleted", hostIndex)
		}
	})

	It("tears down every scaled-away ordinal", func() {
		startFleetManager()
		expectConverged()

		current := &scalev1alpha1.SimFleet{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), current)).To(Succeed())
		current.Spec.Replicas = 1
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		// envtest runs no garbage collector, so every child that disappears
		// was deleted by its instance's teardown.
		Eventually(func(g Gomega) {
			for _, ordinal := range []int{1, 2} {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: utils.InstanceName(fleet.Name, ordinal), Namespace: fleet.Namespace,
				}, &scalev1alpha1.SimInstance{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "SimInstance %d still exists: %v", ordinal, err)
				for _, child := range ordinalChildren(fleet, ordinal) {
					key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
					err := k8sClient.Get(ctx, key, child.obj)
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s still exists: %v", child.obj, child.name, err)
				}
			}
			getFleetInstance(g, fleet, 0)
			scaled := &scalev1alpha1.SimFleet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), scaled)).To(Succeed())
			g.Expect(scaled.Status.AdmittedReplicas).To(Equal(1))
		}, time.Minute, 100*time.Millisecond).Should(Succeed())
	})
})

// deletingFleet is a fleet whose delete was accepted while it held the
// teardown finalizer.
func deletingFleet(replicas, devicesPerDPUNode, admitted int) *scalev1alpha1.SimFleet {
	fleet := rolloutFleet(replicas, devicesPerDPUNode, admitted)
	fleet.Finalizers = []string{simFleetTeardownFinalizer}
	now := metav1.Now()
	fleet.DeletionTimestamp = &now
	return fleet
}

// tearingDownInstance is an instance its own finalizer still holds.
func tearingDownInstance(fleet *scalev1alpha1.SimFleet, ordinal int, deleting bool) *scalev1alpha1.SimInstance {
	instance := fleetInstance(fleet, ordinal, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated)
	instance.Finalizers = []string{simInstanceTeardownFinalizer}
	if deleting {
		now := metav1.Now()
		instance.DeletionTimestamp = &now
		instance.Status.Phase = scalev1alpha1.SimInstanceTerminating
	}
	return instance
}

func ownedHostNode(fleet *scalev1alpha1.SimFleet, hostIndex int) *provisioningv1.DPUNode {
	node := desiredHostNode(fleet, hostIndex, fleet.Spec.Replicas)
	node.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(fleet, scalev1alpha1.GroupVersion.WithKind("SimFleet")),
	}
	return node
}

func TestSimFleetReconcileAddsTheFinalizerBeforeCreatingAnyInstance(t *testing.T) {
	h := newFleetHarness(t, rolloutFleet(3, 1, 0))

	h.reconcile()

	if !controllerutil.ContainsFinalizer(h.fleet(), simFleetTeardownFinalizer) {
		t.Fatalf("finalizers = %v, want %s", h.fleet().Finalizers, simFleetTeardownFinalizer)
	}
	want := fmt.Sprintf("finalizers [%s]", simFleetTeardownFinalizer)
	if len(h.calls) == 0 || h.calls[0] != want {
		t.Fatalf("calls = %v, want %q before any other write", h.calls, want)
	}
	if creates := h.callsWithPrefix("create "); len(creates) != 3 {
		t.Fatalf("SimInstance creates = %v, want 3", creates)
	}

	h.calls = nil
	h.reconcile()
	if patches := h.callsWithPrefix("finalizers "); len(patches) != 0 {
		t.Fatalf("finalizer patches = %v, want none once it is present", patches)
	}
}

func TestSimFleetDeletion(t *testing.T) {
	t.Run("deletes every instance and holds the fleet while any remains", func(t *testing.T) {
		fleet := deletingFleet(3, 2, 3)
		terminating := tearingDownInstance(fleet, 1, true)
		node := ownedHostNode(fleet, 0)
		h := newFleetHarness(t, fleet,
			tearingDownInstance(fleet, 0, false),
			terminating,
			tearingDownInstance(fleet, 2, false),
			node,
		)

		h.reconcile()

		want := []string{
			"delete " + utils.InstanceName(fleet.Name, 0),
			"delete " + utils.InstanceName(fleet.Name, 2),
		}
		if got := h.calls; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("calls = %v, want only %v", got, want)
		}
		instances := h.instances()
		if len(instances) != 3 {
			t.Fatalf("SimInstances = %d, want all 3 held by their teardown", len(instances))
		}
		for ordinal, instance := range instances {
			if instance.DeletionTimestamp.IsZero() {
				t.Fatalf("SimInstance %d was not deleted", ordinal)
			}
		}
		if !controllerutil.ContainsFinalizer(h.fleet(), simFleetTeardownFinalizer) {
			t.Fatal("finalizer was removed while SimInstances remain")
		}

		h.calls = nil
		h.reconcile()
		if len(h.calls) != 0 {
			t.Fatalf("calls = %v, want none while every instance is already terminating", h.calls)
		}
		if !controllerutil.ContainsFinalizer(h.fleet(), simFleetTeardownFinalizer) {
			t.Fatal("finalizer was removed while terminating SimInstances remain")
		}
		current := &provisioningv1.DPUNode{}
		if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(node), current); err != nil {
			t.Fatalf("get DPUNode: %v", err)
		}
		if !current.DeletionTimestamp.IsZero() {
			t.Fatal("DPUNode was deleted by the fleet reconciler")
		}
	})

	t.Run("releases the fleet once no instance remains", func(t *testing.T) {
		fleet := deletingFleet(2, 1, 2)
		h := newFleetHarness(t, fleet,
			tearingDownInstance(fleet, 0, true),
			tearingDownInstance(fleet, 1, true),
		)
		h.reconcile()
		if h.fleetGone() {
			t.Fatal("fleet was released while SimInstances remain")
		}

		h.releaseInstances()
		h.reconcile()

		if !h.fleetGone() {
			t.Fatalf("fleet still exists with finalizers %v", h.fleet().Finalizers)
		}
		if writes := h.callsWithPrefix("status ", "create ", "new dpunode ", "patch dpunode ", "drop dpunode "); len(writes) != 0 {
			t.Fatalf("writes = %v, want only the finalizer removal", writes)
		}
	})

	t.Run("a deleting fleet creates nothing, leaves its status alone, and keeps other finalizers", func(t *testing.T) {
		fleet := deletingFleet(4, 2, 2)
		fleet.Finalizers = append(fleet.Finalizers, "example.com/hold")
		h := newFleetHarness(t, fleet,
			fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
			fleetInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
		)
		before := h.fleet()

		h.reconcile()
		h.reconcile()

		if writes := h.callsWithPrefix("status ", "create ", "new dpunode ", "patch dpunode "); len(writes) != 0 {
			t.Fatalf("writes = %v, want no status patch and no create while deleting", writes)
		}
		after := h.fleet()
		if fmt.Sprint(after.Status) != fmt.Sprint(before.Status) {
			t.Fatalf("status = %+v, want it unchanged from %+v", after.Status, before.Status)
		}
		if controllerutil.ContainsFinalizer(after, simFleetTeardownFinalizer) {
			t.Fatalf("finalizers = %v, want %s removed once no instance remains", after.Finalizers, simFleetTeardownFinalizer)
		}
		if !controllerutil.ContainsFinalizer(after, "example.com/hold") {
			t.Fatalf("finalizers = %v, want the other finalizer kept", after.Finalizers)
		}
	})

	t.Run("a deleting fleet without the finalizer is left alone", func(t *testing.T) {
		fleet := deletingFleet(2, 1, 2)
		fleet.Finalizers = []string{"example.com/hold"}
		h := newFleetHarness(t, fleet, tearingDownInstance(fleet, 0, false))

		h.reconcile()

		if len(h.calls) != 0 {
			t.Fatalf("calls = %v, want none", h.calls)
		}
	})
}

func TestSimFleetReconcileErrorIsReportedOnInventoryReady(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	fleet.Finalizers = []string{simFleetTeardownFinalizer}
	foreign := desiredHostNode(fleet, 0, fleet.Spec.Replicas)
	foreign.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: scalev1alpha1.GroupVersion.String(),
		Kind:       "SimFleet",
		Name:       "other",
		UID:        "other-uid",
		Controller: ptr.To(true),
	}}
	h := newFleetHarness(t, fleet,
		append(fleetInstances(fleet, 0, 2,
			scalev1alpha1.SimInstanceDeviceCreated,
			scalev1alpha1.SimInstanceChildrenCreated,
		), foreign)...,
	)
	reconcile := func() {
		t.Helper()
		_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
		if err == nil {
			t.Fatal("Reconcile() error = nil, want the DPUNode ownership error")
		}
	}

	reconcile()

	expectCondition(t, h.fleet().Status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "ReconcileError")
	if patches := h.callsWithPrefix("status "); len(patches) != 1 {
		t.Fatalf("status patches = %v, want one", patches)
	}

	h.calls = nil
	reconcile()
	if patches := h.callsWithPrefix("status "); len(patches) != 0 {
		t.Fatalf("status patches = %v, want none when the condition is unchanged", patches)
	}
}

var readyInstanceConditions = []string{
	scalev1alpha1.SimInstanceCredentialsReady,
	scalev1alpha1.SimInstanceDeviceCreated,
	scalev1alpha1.SimInstanceChildrenCreated,
	scalev1alpha1.SimInstanceVMReady,
	scalev1alpha1.SimInstanceSelectable,
	scalev1alpha1.SimInstanceDPUProvisioned,
}

func observedInstance(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	phase scalev1alpha1.SimInstancePhase,
	dpuName string,
	trueConditions ...string,
) *scalev1alpha1.SimInstance {
	instance := fleetInstance(fleet, ordinal, phase, trueConditions...)
	instance.Status.DPUName = dpuName
	return instance
}

func expectCondition(
	t *testing.T,
	status scalev1alpha1.SimFleetStatus,
	conditionType string,
	want metav1.ConditionStatus,
	reason string,
) {
	t.Helper()
	condition := meta.FindStatusCondition(status.Conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %s missing from %+v", conditionType, status.Conditions)
	}
	if condition.Status != want || condition.Reason != reason {
		t.Fatalf("condition %s = %s/%s (%s), want %s/%s",
			conditionType, condition.Status, condition.Reason, condition.Message, want, reason)
	}
}

func TestAggregateStatusCountsInstances(t *testing.T) {
	fleet := rolloutFleet(4, 2, 4)
	instances := instanceMap(
		observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
		observedInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning, "dpu-1",
			scalev1alpha1.SimInstanceCredentialsReady,
			scalev1alpha1.SimInstanceDeviceCreated,
			scalev1alpha1.SimInstanceChildrenCreated,
		),
		observedInstance(fleet, 2, scalev1alpha1.SimInstanceProvisioning, "",
			scalev1alpha1.SimInstanceCredentialsReady,
			scalev1alpha1.SimInstanceDeviceCreated,
		),
		observedInstance(fleet, 3, scalev1alpha1.SimInstanceFailed, "", scalev1alpha1.SimInstanceDeviceCreated),
		// A scaled-away ordinal still being torn down is observed, not counted.
		observedInstance(fleet, 4, scalev1alpha1.SimInstanceTerminating, "dpu-4", readyInstanceConditions...),
	)

	status := aggregateStatus(fleet, instances, 2)

	for _, count := range []struct {
		name      string
		got, want int
	}{
		{"admittedReplicas", status.AdmittedReplicas, 4},
		{"createdInstances", status.CreatedInstances, 2},
		{"vmReadyInstances", status.VMReadyInstances, 1},
		{"provisionedInstances", status.ProvisionedInstances, 1},
		{"failedInstances", status.FailedInstances, 1},
		{"dpuNodes", status.DPUNodes, 2},
	} {
		if count.got != count.want {
			t.Errorf("%s = %d, want %d", count.name, count.got, count.want)
		}
	}
	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "Progressing")
	expectCondition(t, status, scalev1alpha1.ConditionDPUsObserved, metav1.ConditionFalse, "WaitingForDPUs")
	expectCondition(t, status, scalev1alpha1.ConditionBootstrapReady, metav1.ConditionFalse, "Progressing")
	expectCondition(t, status, scalev1alpha1.ConditionVirtualMachinesReady, metav1.ConditionFalse, "Progressing")
}

func TestAggregateStatusReportsAReadyFleet(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	instances := instanceMap(
		observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
		observedInstance(fleet, 1, scalev1alpha1.SimInstanceReady, "dpu-1", readyInstanceConditions...),
	)

	status := aggregateStatus(fleet, instances, 1)

	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionDPUsObserved, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionBootstrapReady, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionVirtualMachinesReady, metav1.ConditionTrue, "Ready")
}

func TestAggregateStatusInventoryWaitsForAdmission(t *testing.T) {
	fleet := rolloutFleet(250, 10, 100)
	instances := instanceMap(fleetInstances(fleet, 0, 100, readyInstanceConditions...)...)

	status := aggregateStatus(fleet, instances, 10)

	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "Progressing")
}

func TestAggregateStatusInventoryWaitsForEveryAdmittedInstance(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	instances := instanceMap(
		observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
	)

	status := aggregateStatus(fleet, instances, 1)

	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "Progressing")
}

func TestAggregateStatusInventoryIsNotReadyWithAFailedOrDeletingInstance(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	for _, test := range []struct {
		name   string
		mutate func(*scalev1alpha1.SimInstance)
		// A Failed instance keeps its VM and DPU until it is deleted, so only
		// createdInstances drops it; a deleting instance drops from every count.
		wantVMReady, wantProvisioned int
	}{
		{"Failed", func(i *scalev1alpha1.SimInstance) { i.Status.Phase = scalev1alpha1.SimInstanceFailed }, 2, 2},
		{"being deleted", func(i *scalev1alpha1.SimInstance) {
			now := metav1.Now()
			i.DeletionTimestamp = &now
		}, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			second := observedInstance(fleet, 1, scalev1alpha1.SimInstanceReady, "dpu-1", readyInstanceConditions...)
			test.mutate(second)
			instances := instanceMap(
				observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
				second,
			)

			status := aggregateStatus(fleet, instances, 1)

			expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "Progressing")
			if status.CreatedInstances != 1 ||
				status.VMReadyInstances != test.wantVMReady ||
				status.ProvisionedInstances != test.wantProvisioned {
				t.Fatalf("created/vmReady/provisioned = %d/%d/%d, want 1/%d/%d",
					status.CreatedInstances, status.VMReadyInstances, status.ProvisionedInstances,
					test.wantVMReady, test.wantProvisioned)
			}
		})
	}
}

func TestAggregateStatusZeroReplicasIsReady(t *testing.T) {
	status := aggregateStatus(rolloutFleet(0, 1, 0), nil, 0)

	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionDPUsObserved, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionVirtualMachinesReady, metav1.ConditionTrue, "Ready")
}

func TestAggregateStatusReportsDuplicateDPUs(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	duplicate := observedInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning, "",
		scalev1alpha1.SimInstanceCredentialsReady,
		scalev1alpha1.SimInstanceDeviceCreated,
		scalev1alpha1.SimInstanceChildrenCreated,
	)
	setInstanceCondition(duplicate, scalev1alpha1.SimInstanceDPUProvisioned, false, "DuplicateDPU", "2 DPUs")
	instances := instanceMap(
		observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
		duplicate,
	)

	status := aggregateStatus(fleet, instances, 1)

	expectCondition(t, status, scalev1alpha1.ConditionDPUsObserved, metav1.ConditionFalse, "MultipleDPUs")
}

func TestAggregateStatusWaitsForVirtualMachinesToBeReady(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	instances := instanceMap(fleetInstances(fleet, 0, 2,
		scalev1alpha1.SimInstanceCredentialsReady,
		scalev1alpha1.SimInstanceDeviceCreated,
		scalev1alpha1.SimInstanceChildrenCreated,
	)...)

	status := aggregateStatus(fleet, instances, 1)

	if status.CreatedInstances != 2 || status.VMReadyInstances != 0 {
		t.Fatalf("createdInstances = %d vmReadyInstances = %d, want 2 and 0",
			status.CreatedInstances, status.VMReadyInstances)
	}
	expectCondition(t, status, scalev1alpha1.ConditionVirtualMachinesReady, metav1.ConditionFalse, "WaitingForVMs")
	expectCondition(t, status, scalev1alpha1.ConditionBootstrapReady, metav1.ConditionTrue, "Ready")
}

func TestAggregateStatusWithoutAVirtualMachineTemplate(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	fleet.Spec.VirtualMachineTemplate = nil
	instances := instanceMap(fleetInstances(fleet, 0, 2,
		scalev1alpha1.SimInstanceCredentialsReady,
		scalev1alpha1.SimInstanceDeviceCreated,
		scalev1alpha1.SimInstanceChildrenCreated,
	)...)

	status := aggregateStatus(fleet, instances, 1)

	if status.CreatedInstances != 2 {
		t.Fatalf("createdInstances = %d, want 2 without a template", status.CreatedInstances)
	}
	expectCondition(t, status, scalev1alpha1.ConditionVirtualMachinesReady, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionBootstrapReady, metav1.ConditionTrue, "Ready")
	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionTrue, "Ready")
}

func TestAggregateStatusReportsAnInvalidVirtualMachineTemplate(t *testing.T) {
	fleet := rolloutFleet(1, 1, 1)
	fleet.Spec.VirtualMachineTemplate.Storage = &scalev1alpha1.VirtualMachineStorage{Ephemeral: ptr.To(false)}

	status := aggregateStatus(fleet, nil, 0)

	expectCondition(t, status, scalev1alpha1.ConditionBootstrapReady, metav1.ConditionFalse, "UnsupportedStorage")
	expectCondition(t, status, scalev1alpha1.ConditionVirtualMachinesReady, metav1.ConditionFalse, "UnsupportedStorage")
}

func TestAggregateStatusReportsABlockedCredential(t *testing.T) {
	fleet := rolloutFleet(1, 1, 1)
	blocked := fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning)
	setInstanceCondition(blocked, scalev1alpha1.SimInstanceCredentialsReady, false,
		"OwnershipConflict", "ServiceAccount is controlled by another owner")

	status := aggregateStatus(fleet, instanceMap(blocked), 0)

	expectCondition(t, status, scalev1alpha1.ConditionBootstrapReady, metav1.ConditionFalse, "OwnershipConflict")
}

func TestBootstrapReadyCondition(t *testing.T) {
	withTemplate := &scalev1alpha1.SimFleet{
		Spec: scalev1alpha1.SimFleetSpec{
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{Image: "example.com/node@sha256:test"},
		},
	}
	blocked := &bootstrapConditionError{reason: "OwnershipConflict", message: "blocked"}
	cases := []struct {
		name       string
		fleet      *scalev1alpha1.SimFleet
		bootstrap  bootstrapOutcome
		desiredVMs int
		status     metav1.ConditionStatus
		reason     string
	}{
		{
			name:   "no template",
			fleet:  &scalev1alpha1.SimFleet{},
			status: metav1.ConditionTrue,
			reason: "Ready",
		},
		{
			name:       "no device to boot",
			fleet:      withTemplate,
			desiredVMs: 0,
			status:     metav1.ConditionTrue,
			reason:     "Ready",
		},
		{
			name:       "every guest Secret is written",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{cloudInit: map[int]struct{}{0: {}}},
			desiredVMs: 1,
			status:     metav1.ConditionTrue,
			reason:     "Ready",
		},
		{
			name:       "a guest Secret is not written yet",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{cloudInit: map[int]struct{}{}},
			desiredVMs: 1,
			status:     metav1.ConditionFalse,
			reason:     "Progressing",
		},
		{
			name:       "blocked instance",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{cloudInit: map[int]struct{}{}, blocked: blocked},
			desiredVMs: 1,
			status:     metav1.ConditionFalse,
			reason:     "OwnershipConflict",
		},
		{
			name:       "blocked instance with no desired VM",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{blocked: blocked},
			desiredVMs: 0,
			status:     metav1.ConditionTrue,
			reason:     "Ready",
		},
		{
			name:       "bootstrap reason",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{err: &bootstrapConditionError{reason: "SomethingRefused", message: "failed"}},
			desiredVMs: 1,
			status:     metav1.ConditionFalse,
			reason:     "SomethingRefused",
		},
		{
			name:      "error wins over no template",
			fleet:     &scalev1alpha1.SimFleet{},
			bootstrap: bootstrapOutcome{err: errors.New("boom")},
			status:    metav1.ConditionFalse,
			reason:    "ReconcileError",
		},
		{
			name:       "error wins over no desired VM",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{err: errors.New("boom"), blocked: blocked},
			desiredVMs: 0,
			status:     metav1.ConditionFalse,
			reason:     "ReconcileError",
		},
		{
			name:       "template validation reason",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{err: errUnsupportedStorage},
			desiredVMs: 1,
			status:     metav1.ConditionFalse,
			reason:     "UnsupportedStorage",
		},
		{
			name:       "untyped error",
			fleet:      withTemplate,
			bootstrap:  bootstrapOutcome{err: errors.New("boom")},
			desiredVMs: 1,
			status:     metav1.ConditionFalse,
			reason:     "ReconcileError",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			condition := bootstrapReadyCondition(testCase.fleet, testCase.bootstrap, testCase.desiredVMs)
			if condition.Type != scalev1alpha1.ConditionBootstrapReady {
				t.Fatalf("type %q", condition.Type)
			}
			if condition.Status != testCase.status || condition.Reason != testCase.reason {
				t.Fatalf("status %q reason %q want %q/%q",
					condition.Status, condition.Reason, testCase.status, testCase.reason)
			}
			if condition.Message == "" {
				t.Fatal("empty message")
			}
		})
	}
}

func TestPatchFleetStatusNeverLowersAdmittedReplicas(t *testing.T) {
	for _, test := range []struct {
		name                     string
		replicas, stored, passed int
		want                     int
	}{
		{"lower pass value keeps the stored index", 250, 200, 100, 200},
		{"higher pass value raises it", 250, 100, 200, 200},
		{"scale-down lowers it to replicas", 50, 200, 50, 50},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFleetHarness(t, rolloutFleet(test.replicas, 10, test.stored))
			fleet := h.fleet()

			if err := h.reconciler.patchFleetStatus(context.Background(), fleet, test.passed, nil, 0, ""); err != nil {
				t.Fatalf("patchFleetStatus() error = %v", err)
			}

			if got := h.fleet().Status.AdmittedReplicas; got != test.want {
				t.Fatalf("admittedReplicas = %d, want %d", got, test.want)
			}
		})
	}
}
