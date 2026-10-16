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
	"fmt"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	virtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/ownership"
)

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
				g.Expect(ownership.ControlledBy(instance, scalev1alpha1.GroupVersion.String(), "SimFleet", fleet.Name, fleet.UID)).
					To(BeTrue(), "SimInstance %d owner = %+v", ordinal, instance.OwnerReferences)
				g.Expect(instanceConditionTrue(instance, scalev1alpha1.SimInstanceChildrenCreated)).To(BeTrue(),
					"SimInstance %d conditions = %+v", ordinal, instance.Status.Conditions)
				for _, child := range []ordinalChild{
					{naming.DeviceName(fleet.Name, ordinal), &provisioningv1.DPUDevice{}},
					{naming.VirtualMachineName(fleet.Name, ordinal), &virtv1.VirtualMachine{}},
				} {
					key := client.ObjectKey{Name: child.name, Namespace: fleet.Namespace}
					g.Expect(k8sClient.Get(ctx, key, child.obj)).To(Succeed(), "get %T %s", child.obj, child.name)
					g.Expect(ownership.ControlledBy(child.obj, scalev1alpha1.GroupVersion.String(), "SimInstance",
						instance.Name, instance.UID)).
						To(BeTrue(), "%T %s owner = %+v", child.obj, child.name, child.obj.GetOwnerReferences())
				}
			}
			for hostIndex := range naming.DesiredNodeCount(fleet.Spec.Replicas, fleet.Spec.DevicesPerDPUNode) {
				node := &provisioningv1.DPUNode{}
				key := client.ObjectKey{Name: naming.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
				g.Expect(k8sClient.Get(ctx, key, node)).To(Succeed(), "get DPUNode host %d", hostIndex)
				g.Expect(ownership.ControlledBy(node, scalev1alpha1.GroupVersion.String(), "SimFleet", fleet.Name, fleet.UID)).
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
			Name: naming.DeviceName(fleet.Name, 0), Namespace: fleet.Namespace,
		}, device)).To(Succeed())
		oldDeviceUID := device.UID
		removeWithoutFinalizers(device)

		instance := &scalev1alpha1.SimInstance{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: naming.InstanceName(fleet.Name, 1), Namespace: fleet.Namespace,
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
				Name: naming.DeviceName(fleet.Name, 0), Namespace: fleet.Namespace,
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
				Name: naming.VirtualMachineName(fleet.Name, ordinal), Namespace: fleet.Namespace,
			}, vm)).To(Succeed())
			bootGuest(vm, fmt.Sprintf("10.244.0.%d", 10+ordinal))
		}
		Eventually(func(g Gomega) {
			for ordinal := range fleet.Spec.Replicas {
				device := &provisioningv1.DPUDevice{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{
					Name: naming.DeviceName(fleet.Name, ordinal), Namespace: fleet.Namespace,
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
				g.Expect(current.Finalizers).To(ContainElement(scalev1alpha1.SimFleetFinalizer))
			}
			g.Expect(instances.Items).To(BeEmpty())
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "SimFleet still exists: %v", err)
		}, time.Minute, 100*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			recordDeviceEvents()
			for ordinal := range fleet.Spec.Replicas {
				name := naming.DeviceName(fleet.Name, ordinal)
				wasQuarantined, deleted := deletedQuarantined[name]
				g.Expect(deleted).To(BeTrue(), "no delete event for DPUDevice %s", name)
				if !wasQuarantined {
					StopTrying("DPUDevice " + name + " was deleted with its selection labels").Now()
				}
			}
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		// The DPUNodes are left to garbage collection, which envtest does not run.
		for hostIndex := range naming.DesiredNodeCount(fleet.Spec.Replicas, fleet.Spec.DevicesPerDPUNode) {
			node := &provisioningv1.DPUNode{}
			key := client.ObjectKey{Name: naming.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
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
					Name: naming.InstanceName(fleet.Name, ordinal), Namespace: fleet.Namespace,
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
