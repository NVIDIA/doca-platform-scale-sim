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

	"github.com/go-logr/logr/funcr"
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

func dpuRefs(names ...string) []provisioningv1.DPURef {
	refs := make([]provisioningv1.DPURef, 0, len(names))
	for _, name := range names {
		refs = append(refs, provisioningv1.DPURef{Name: name})
	}
	return refs
}

var _ = Describe("SimFleet rollout", func() {
	It("admits the first batch before creating it", func() {
		h := newFleetHarness(rolloutFleet(250, 10, 0))

		h.reconcile()

		instances := h.instances()
		Expect(instances).To(HaveLen(admitBatch))
		for ordinal := range admitBatch {
			Expect(instances).To(HaveKey(ordinal), "SimInstance for ordinal %d was not created", ordinal)
		}
		Expect(h.fleet().Status.AdmittedReplicas).To(Equal(admitBatch))
		writes := h.callsWithPrefix("status ", "create ")
		Expect(writes).NotTo(BeEmpty(), "calls = %v", h.calls)
		Expect(writes[0]).To(Equal(fmt.Sprintf("status admitted=%d", admitBatch)),
			"writes = %v, want the admittedReplicas status patch before any create", h.calls)
		Expect(h.callsWithPrefix("create ")).To(HaveLen(admitBatch))
	})

	It("reports an instance name held by another owner", func() {
		fleet := rolloutFleet(2, 1, 2)
		foreign := fleetInstance(fleet, 1, scalev1alpha1.SimInstanceReady)
		foreign.OwnerReferences = foreignOwner()
		h := newFleetHarness(fleet, foreign)

		h.reconcile()

		Expect(h.instances()).To(HaveKey(0), "the conflict on ordinal 1 stopped ordinal 0 from being created")
		status := h.fleet().Status
		expectCondition(status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "OwnershipConflict")
		condition := meta.FindStatusCondition(status.Conditions, scalev1alpha1.ConditionInventoryReady)
		Expect(condition.Message).To(ContainSubstring(foreign.Name))
		current := &scalev1alpha1.SimInstance{}
		Expect(h.client.Get(context.Background(), client.ObjectKeyFromObject(foreign), current)).To(Succeed())
		Expect(current.DeletionTimestamp.IsZero()).To(BeTrue(), "foreign SimInstance was deleted: %+v", current.ObjectMeta)
		Expect(current.OwnerReferences).To(Equal(foreign.OwnerReferences), "foreign SimInstance was modified: %+v", current.ObjectMeta)
	})

	// An earlier fleet's instance of the same name that is still tearing down is
	// waited out: the create is skipped and logged, and it is not a conflict.
	It("waits out a terminating earlier instance", func() {
		fleet := rolloutFleet(1, 1, 1)
		earlier := fleetInstance(fleet, 0, scalev1alpha1.SimInstanceTerminating)
		earlier.OwnerReferences[0].UID = "earlier-fleet-uid"
		earlier.Finalizers = []string{simInstanceTeardownFinalizer}
		now := metav1.Now()
		earlier.DeletionTimestamp = &now
		h := newFleetHarness(fleet, earlier)
		var logs []string
		logger := funcr.New(func(prefix, args string) {
			logs = append(logs, args)
		}, funcr.Options{Verbosity: 1})

		_, err := h.reconciler.Reconcile(logf.IntoContext(context.Background(), logger), ctrl.Request{NamespacedName: h.key})
		Expect(err).NotTo(HaveOccurred())

		expectCondition(h.fleet().Status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "Progressing")
		Expect(logs).To(ContainElement(SatisfyAll(
			ContainSubstring(`"msg"="Skipped creating SimInstance that still exists"`),
			ContainSubstring(`"simInstance"="`+earlier.Name+`"`),
		)), "want a skipped create of %s", earlier.Name)
	})

	It("recreates a deleted instance below admitted", func() {
		fleet := rolloutFleet(3, 1, 3)
		h := newFleetHarness(fleet,
			fleetInstance(fleet, 0, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
			fleetInstance(fleet, 2, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
		)

		h.reconcile()

		Expect(h.instances()).To(HaveKey(1), "SimInstance for ordinal 1 was not recreated")
		Expect(h.callsWithPrefix("create ")).To(Equal([]string{"create " + utils.InstanceName(fleet.Name, 1)}),
			"want only ordinal 1 created")
	})

	It("replaces a failed instance", func() {
		fleet := rolloutFleet(3, 1, 3)
		failed := fleetInstance(fleet, 1, scalev1alpha1.SimInstanceFailed, scalev1alpha1.SimInstanceChildrenCreated)
		h := newFleetHarness(fleet, append(
			fleetInstances(fleet, 0, 1, scalev1alpha1.SimInstanceChildrenCreated),
			failed,
			fleetInstance(fleet, 2, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
		)...)

		h.reconcile()

		Expect(h.callsWithPrefix("delete ")).To(Equal([]string{"delete " + failed.Name}),
			"want only the Failed instance deleted")
		err := h.client.Get(context.Background(), client.ObjectKeyFromObject(failed), &scalev1alpha1.SimInstance{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Get(Failed instance) error = %v, want NotFound", err)

		h.reconcile()

		replacement := h.instances()[1]
		Expect(replacement).NotTo(BeNil(), "want a fresh instance for ordinal 1")
		Expect(replacement.Status.Phase).NotTo(Equal(scalev1alpha1.SimInstanceFailed),
			"replacement = %+v, want a fresh instance for ordinal 1", replacement)
	})

	// A Failed instance whose teardown is already running is not deleted again
	// and not replaced until it is gone.
	It("leaves a failed instance alone while it tears down", func() {
		fleet := rolloutFleet(3, 1, 3)
		failed := fleetInstance(fleet, 1, scalev1alpha1.SimInstanceFailed, scalev1alpha1.SimInstanceChildrenCreated)
		failed.Status.FailureReason = "DPUDevice"
		failed.Finalizers = []string{simInstanceTeardownFinalizer}
		now := metav1.Now()
		failed.DeletionTimestamp = &now
		h := newFleetHarness(fleet, append(
			fleetInstances(fleet, 0, 1, scalev1alpha1.SimInstanceChildrenCreated),
			failed,
			fleetInstance(fleet, 2, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
		)...)

		h.reconcile()

		Expect(h.callsWithPrefix("delete ", "create ")).To(BeEmpty(), "want no SimInstance writes while ordinal 1 tears down")
	})

	It("scales down", func() {
		fleet := rolloutFleet(150, 10, 150)
		h := newFleetHarness(fleet, fleetInstances(fleet, 0, 150, scalev1alpha1.SimInstanceChildrenCreated)...)
		current := h.fleet()
		current.Spec.Replicas = 50
		Expect(h.client.Update(context.Background(), current)).To(Succeed(), "scale SimFleet")

		h.reconcile()

		instances := h.instances()
		Expect(instances).To(HaveLen(50))
		for ordinal := range 50 {
			Expect(instances).To(HaveKey(ordinal), "SimInstance for ordinal %d was deleted", ordinal)
		}
		Expect(h.callsWithPrefix("delete ")).To(HaveLen(100))
		Expect(h.fleet().Status.AdmittedReplicas).To(Equal(50))
	})

	Describe("DPUNodes", func() {
		It("creates DPUNodes for created hosts", func() {
			fleet := rolloutFleet(5, 2, 5)
			h := newFleetHarness(fleet,
				fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
				fleetInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
				fleetInstance(fleet, 2, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
				fleetInstance(fleet, 3, scalev1alpha1.SimInstanceProvisioning),
				fleetInstance(fleet, 4, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
			)

			h.reconcile()

			h.expectNode(fleet, 0, 0, 1)
			h.expectNoNode(fleet, 1)
			h.expectNode(fleet, 2, 4)
			Expect(h.fleet().Status.DPUNodes).To(Equal(2))
		})

		It("gives the partial admitted host its admitted devices", func() {
			fleet := rolloutFleet(5, 2, 3)
			h := newFleetHarness(fleet, fleetInstances(fleet, 0, 3, scalev1alpha1.SimInstanceDeviceCreated)...)

			h.reconcile()

			h.expectNode(fleet, 0, 0, 1)
			h.expectNode(fleet, 1, 2)
			h.expectNoNode(fleet, 2)
		})

		It("adds a missing device to an existing DPUNode", func() {
			fleet := rolloutFleet(2, 2, 2)
			node := ownedHostNode(fleet, 0)
			node.Spec.DPUs = dpuRefs(utils.DeviceName(fleet.Name, 0), "added-by-dpf")
			h := newFleetHarness(fleet, append(
				fleetInstances(fleet, 0, 2, scalev1alpha1.SimInstanceDeviceCreated, scalev1alpha1.SimInstanceChildrenCreated),
				node,
			)...)

			h.reconcile()

			current := &provisioningv1.DPUNode{}
			Expect(h.client.Get(context.Background(), client.ObjectKeyFromObject(node), current)).To(Succeed())
			Expect(current.Spec.DPUs).To(Equal(
				dpuRefs(utils.DeviceName(fleet.Name, 0), "added-by-dpf", utils.DeviceName(fleet.Name, 1))))
			Expect(h.callsWithPrefix("patch dpunode ")).To(HaveLen(1))

			h.calls = nil
			h.reconcile()
			Expect(h.callsWithPrefix("patch dpunode ")).To(BeEmpty(), "want no DPUNode patch once it lists every device")
		})

		It("refuses a DPUNode without a controller", func() {
			fleet := rolloutFleet(2, 2, 2)
			orphan := desiredHostNode(fleet, 0, fleet.Spec.Replicas)
			orphan.Spec.DPUs = nil
			h := newFleetHarness(fleet, append(
				fleetInstances(fleet, 0, 2, scalev1alpha1.SimInstanceDeviceCreated, scalev1alpha1.SimInstanceChildrenCreated),
				orphan,
			)...)

			_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})

			Expect(err).To(MatchError(ContainSubstring("missing owner reference")))
			Expect(h.callsWithPrefix("patch dpunode ", "new dpunode ")).To(BeEmpty())
		})

		// A host whose devices are not all created keeps the DPUNode it already has:
		// it is counted, and not patched until the missing device exists.
		It("counts an existing DPUNode of an incomplete host", func() {
			fleet := rolloutFleet(2, 2, 2)
			h := newFleetHarness(fleet,
				fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
				fleetInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning),
				ownedHostNode(fleet, 0),
			)

			h.reconcile()

			Expect(h.fleet().Status.DPUNodes).To(Equal(1))
			Expect(h.callsWithPrefix("patch dpunode ", "new dpunode ", "drop dpunode ")).To(BeEmpty())
		})
	})

	Describe("nextAdmitted", func() {
		allCreated := func(fleet *scalev1alpha1.SimFleet, from, to int) map[int]*scalev1alpha1.SimInstance {
			return instanceMap(fleetInstances(fleet, from, to, scalev1alpha1.SimInstanceChildrenCreated)...)
		}
		allCreatedTo := func(to int) func(*scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
			return func(fleet *scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
				return allCreated(fleet, 0, to)
			}
		}

		DescribeTable("advances the admitted index only past created instances",
			func(admitted int, instances func(*scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance, want int) {
				fleet := rolloutFleet(250, 10, 0)
				Expect(nextAdmitted(fleet, admitted, instances(fleet))).To(Equal(want), "nextAdmitted(%d)", admitted)
			},
			Entry("nothing admitted", 0, func(*scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
				return nil
			}, 100),
			Entry("one admitted instance without ChildrenCreated", 100,
				func(fleet *scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
					oneMissing := allCreated(fleet, 0, 100)
					oneMissing[42] = fleetInstance(fleet, 42, scalev1alpha1.SimInstanceProvisioning,
						scalev1alpha1.SimInstanceDeviceCreated)
					return oneMissing
				}, 100),
			Entry("one admitted instance missing", 100,
				func(fleet *scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
					gap := allCreated(fleet, 0, 100)
					delete(gap, 7)
					return gap
				}, 100),
			Entry("one admitted instance Failed", 100,
				func(fleet *scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
					oneFailed := allCreated(fleet, 0, 100)
					oneFailed[42] = fleetInstance(fleet, 42, scalev1alpha1.SimInstanceFailed,
						scalev1alpha1.SimInstanceChildrenCreated)
					return oneFailed
				}, 100),
			Entry("one admitted instance being deleted", 100,
				func(fleet *scalev1alpha1.SimFleet) map[int]*scalev1alpha1.SimInstance {
					oneDeleting := allCreated(fleet, 0, 100)
					now := metav1.Now()
					oneDeleting[42].DeletionTimestamp = &now
					return oneDeleting
				}, 100),
			Entry("every admitted instance created", 100, allCreatedTo(100), 200),
			Entry("capped at replicas", 200, allCreatedTo(200), 250),
			Entry("already at replicas", 250, allCreatedTo(250), 250),
		)
	})

	Describe("desiredInstance", func() {
		It("names, labels, and owns the SimInstance for an ordinal", func() {
			fleet := rolloutFleet(250, 10, 0)
			instance := desiredInstance(fleet, 42)

			Expect(instance.Name).To(Equal(utils.InstanceName(fleet.Name, 42)))
			Expect(instance.Namespace).To(Equal(fleet.Namespace))
			Expect(instance.Labels).To(HaveKeyWithValue(labelFleetName, fleet.Name))
			Expect(instance.Labels).To(HaveKeyWithValue(labelFleetUID, string(fleet.UID)))
			Expect(instance.Labels).To(HaveKeyWithValue(labelOrdinal, "000042"))
			Expect(instance.Spec.Ordinal).To(Equal(42))
			Expect(instance.Spec.HostIndex).To(Equal(4))
			owner := metav1.GetControllerOf(instance)
			Expect(owner).NotTo(BeNil())
			Expect(owner.UID).To(Equal(fleet.UID), "controller owner = %+v", owner)
			Expect(owner.Kind).To(Equal("SimFleet"), "controller owner = %+v", owner)
		})
	})

	Describe("persistAdmitted", func() {
		It("conflicts with a stale fleet", func() {
			fleet := rolloutFleet(500, 10, 100)
			h := newFleetHarness(fleet)
			stale := h.fleet()
			current := stale.DeepCopy()
			current.Status.AdmittedReplicas = 300
			Expect(h.client.Status().Update(context.Background(), current)).To(Succeed(), "advance SimFleet status")

			err := h.reconciler.persistAdmitted(context.Background(), stale, 200)

			Expect(apierrors.IsConflict(err)).To(BeTrue(), "persistAdmitted() error = %v, want a conflict", err)
			Expect(h.fleet().Status.AdmittedReplicas).To(Equal(300))
		})
	})

	Describe("desiredHostNode", func() {
		It("uses the DPUNode template", func() {
			fleet := rolloutFleet(4, 2, 4)

			method := desiredHostNode(fleet, 0, 4).Spec.NodeRebootMethod
			Expect(method).NotTo(BeNil(), "want External without a template")
			Expect(method.External).NotTo(BeNil(), "nodeRebootMethod = %+v, want External without a template", method)

			fleet.Spec.DPUNodeTemplate = &scalev1alpha1.DPUNodeTemplate{
				Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{
					"team":       "scale",
					labelOrdinal: "9999",
				}},
				Spec: scalev1alpha1.DPUNodeTemplateSpec{
					NodeRebootMethod: &provisioningv1.NodeRebootMethod{HostAgent: &provisioningv1.HostAgent{}},
				},
			}
			node := desiredHostNode(fleet, 1, 4)

			Expect(node.Labels).To(HaveKeyWithValue("team", "scale"), "want the template label")
			Expect(node.Labels).To(HaveKeyWithValue(labelOrdinal, "0001"), "want the host index over the template")
			method = node.Spec.NodeRebootMethod
			Expect(method).NotTo(BeNil(), "want the template's HostAgent")
			Expect(method.HostAgent).NotTo(BeNil(), "nodeRebootMethod = %+v, want the template's HostAgent", method)
			Expect(method.External).To(BeNil(), "nodeRebootMethod = %+v, want the template's HostAgent", method)
			Expect(method).NotTo(BeIdenticalTo(fleet.Spec.DPUNodeTemplate.Spec.NodeRebootMethod),
				"nodeRebootMethod aliases the fleet template")
		})
	})

	// A node missing any desired ref has to be one the patch path can bring back
	// into line, without dropping a ref someone else added.
	Describe("appendMissingDPUs", func() {
		DescribeTable("brings a node's refs into line without dropping any",
			func(existing, desired, want []provisioningv1.DPURef) {
				Expect(appendMissingDPUs(existing, desired)).To(Equal(want))
			},
			Entry("appends every missing ref to an empty node", nil, dpuRefs("d0", "d1"), dpuRefs("d0", "d1")),
			Entry("leaves a node that already lists every desired ref",
				dpuRefs("d0", "d1"), dpuRefs("d0", "d1"), dpuRefs("d0", "d1")),
			Entry("keeps refs the fleet no longer desires",
				dpuRefs("d0", "stale"), dpuRefs("d0", "d1"), dpuRefs("d0", "stale", "d1")),
			Entry("restores a ref replaced by an equal number of foreign refs",
				dpuRefs("foreign"), dpuRefs("d0"), dpuRefs("foreign", "d0")),
		)
	})

	// A Create that returns AlreadyExists while the cache has not caught up leaves
	// the follow-up Get empty. That is cache lag, not a broken fleet, so the pass
	// has to report the node missing and come back rather than fail the reconcile.
	Describe("ensureNode", func() {
		It("tolerates cache lag", func() {
			fleet := inventoryFleet()
			scheme := runtime.NewScheme()
			Expect(scalev1alpha1.AddToScheme(scheme)).To(Succeed(), "add SimFleet types to scheme")
			Expect(provisioningv1.AddToScheme(scheme)).To(Succeed(), "add DPF types to scheme")

			staleCache := interceptor.Funcs{
				Create: func(
					_ context.Context,
					_ client.WithWatch,
					obj client.Object,
					_ ...client.CreateOption,
				) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{}, obj.GetName())
				},
			}
			reconciler := &SimFleetReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme).
					WithInterceptorFuncs(staleCache).
					Build(),
				Scheme: scheme,
			}

			ready, err := reconciler.ensureNode(context.Background(), fleet,
				desiredHostNode(fleet, 0, fleet.Spec.Replicas), true)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready).To(BeFalse())
		})
	})
})
