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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpf"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// deletingFleet is a fleet whose delete was accepted while it held the
// teardown finalizer.
func deletingFleet(replicas, devicesPerDPUNode, admitted int) *scalev1alpha1.SimFleet {
	fleet := rolloutFleet(replicas, devicesPerDPUNode, admitted)
	fleet.Finalizers = []string{scalev1alpha1.SimFleetFinalizer}
	now := metav1.Now()
	fleet.DeletionTimestamp = &now
	return fleet
}

// tearingDownInstance is an instance its own finalizer still holds.
func tearingDownInstance(fleet *scalev1alpha1.SimFleet, ordinal int, deleting bool) *scalev1alpha1.SimInstance {
	instance := fleetInstance(fleet, ordinal, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated)
	instance.Finalizers = []string{scalev1alpha1.SimInstanceFinalizer}
	if deleting {
		now := metav1.Now()
		instance.DeletionTimestamp = &now
		instance.Status.Phase = scalev1alpha1.SimInstanceTerminating
	}
	return instance
}

var _ = Describe("SimFleet reconcile (fake client)", func() {
	Describe("teardown", func() {
		It("adds the finalizer before creating any instance", func() {
			h := newFleetHarness(rolloutFleet(3, 1, 0))

			h.reconcile()

			Expect(h.fleet().Finalizers).To(ContainElement(scalev1alpha1.SimFleetFinalizer))
			Expect(h.calls).NotTo(BeEmpty())
			Expect(h.calls[0]).To(Equal(call(callFinalizers, fmt.Sprintf("[%s]", scalev1alpha1.SimFleetFinalizer))),
				"calls = %v, want the finalizer patch before any other write", h.calls)
			Expect(h.callsOf(callCreateInstance)).To(HaveLen(3))

			h.calls = nil
			h.reconcile()
			Expect(h.callsOf(callFinalizers)).To(BeEmpty(), "want no finalizer patch once it is present")
		})

		Context("when the fleet is deleted", func() {
			It("deletes every instance and holds the fleet while any remains", func() {
				fleet := deletingFleet(3, 2, 3)
				terminating := tearingDownInstance(fleet, 1, true)
				node := ownedHostNode(fleet, 0)
				h := newFleetHarness(fleet,
					tearingDownInstance(fleet, 0, false),
					terminating,
					tearingDownInstance(fleet, 2, false),
					node,
				)

				h.reconcile()

				Expect(h.calls).To(Equal([]string{
					call(callDeleteInstance, naming.InstanceName(fleet.Name, 0)),
					call(callDeleteInstance, naming.InstanceName(fleet.Name, 2)),
				}))
				instances := h.instances()
				Expect(instances).To(HaveLen(3), "want all 3 SimInstances held by their teardown")
				for ordinal, instance := range instances {
					Expect(instance.DeletionTimestamp.IsZero()).To(BeFalse(), "SimInstance %d was not deleted", ordinal)
				}
				Expect(h.fleet().Finalizers).To(ContainElement(scalev1alpha1.SimFleetFinalizer),
					"finalizer was removed while SimInstances remain")

				h.calls = nil
				h.reconcile()
				Expect(h.calls).To(BeEmpty(), "want no calls while every instance is already terminating")
				Expect(h.fleet().Finalizers).To(ContainElement(scalev1alpha1.SimFleetFinalizer),
					"finalizer was removed while terminating SimInstances remain")
				current := &provisioningv1.DPUNode{}
				Expect(h.client.Get(context.Background(), client.ObjectKeyFromObject(node), current)).To(Succeed())
				Expect(current.DeletionTimestamp.IsZero()).To(BeTrue(), "DPUNode was deleted by the fleet reconciler")
			})

			It("releases the fleet once no instance remains", func() {
				fleet := deletingFleet(2, 1, 2)
				h := newFleetHarness(fleet,
					tearingDownInstance(fleet, 0, true),
					tearingDownInstance(fleet, 1, true),
				)
				h.reconcile()
				Expect(h.fleetGone()).To(BeFalse(), "fleet was released while SimInstances remain")

				h.releaseInstances()
				h.reconcile()

				Expect(h.fleetGone()).To(BeTrue(), func() string {
					return fmt.Sprintf("fleet still exists with finalizers %v", h.fleet().Finalizers)
				})
				Expect(h.calls).To(Equal([]string{call(callFinalizers, "[]")}), "want only the finalizer removal")
			})

			It("a deleting fleet creates nothing, leaves its status alone, and keeps other finalizers", func() {
				fleet := deletingFleet(4, 2, 2)
				fleet.Finalizers = append(fleet.Finalizers, "example.com/hold")
				h := newFleetHarness(fleet,
					fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
					fleetInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
				)
				before := h.fleet()

				h.reconcile()
				h.reconcile()

				Expect(h.calls).To(Equal([]string{
					call(callDeleteInstance, naming.InstanceName(fleet.Name, 0)),
					call(callDeleteInstance, naming.InstanceName(fleet.Name, 1)),
					call(callFinalizers, "[example.com/hold]"),
				}), "want only the instance deletes and the finalizer removal while deleting")
				after := h.fleet()
				Expect(after.Status).To(Equal(before.Status))
				Expect(after.Finalizers).NotTo(ContainElement(scalev1alpha1.SimFleetFinalizer),
					"want the teardown finalizer removed once no instance remains")
				Expect(after.Finalizers).To(ContainElement("example.com/hold"), "want the other finalizer kept")
			})

			It("a deleting fleet without the finalizer is left alone", func() {
				fleet := deletingFleet(2, 1, 2)
				fleet.Finalizers = []string{"example.com/hold"}
				h := newFleetHarness(fleet, tearingDownInstance(fleet, 0, false))

				h.reconcile()

				Expect(h.calls).To(BeEmpty())
			})
		})
	})

	Describe("errors", func() {
		It("reports a reconcile error on InventoryReconciled", func() {
			fleet := rolloutFleet(2, 2, 2)
			fleet.Finalizers = []string{scalev1alpha1.SimFleetFinalizer}
			foreign := dpf.DesiredHostNode(fleet, 0, fleet.Spec.Replicas)
			foreign.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: scalev1alpha1.GroupVersion.String(),
				Kind:       "SimFleet",
				Name:       "other",
				UID:        "other-uid",
				Controller: ptr.To(true),
			}}
			h := newFleetHarness(fleet,
				append(fleetInstances(fleet, 0, 2,
					scalev1alpha1.SimInstanceDeviceCreated,
					scalev1alpha1.SimInstanceChildrenCreated,
				), foreign)...,
			)
			reconcile := func() {
				GinkgoHelper()
				_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
				Expect(err).To(HaveOccurred(), "want the DPUNode ownership error")
			}

			reconcile()

			expectCondition(h.fleet().Status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionFalse, "ReconcileError")
			Expect(h.calls).To(Equal([]string{call(callStatus, "admitted=2")}), "want only the status patch")

			h.calls = nil
			reconcile()
			Expect(h.calls).To(BeEmpty(), "want no writes when the condition is unchanged")
		})
	})
})

var readyInstanceConditions = []string{
	scalev1alpha1.SimInstanceCredentialsReady,
	scalev1alpha1.SimInstanceDeviceCreated,
	scalev1alpha1.SimInstanceChildrenCreated,
	scalev1alpha1.SimInstanceGuestReady,
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

type bootstrapReadyCase struct {
	fleet      *scalev1alpha1.SimFleet
	bootstrap  bootstrapOutcome
	desiredVMs int
	status     metav1.ConditionStatus
	reason     string
}

var _ = Describe("SimFleet status", func() {
	Describe("aggregateStatus", func() {
		It("counts instances", func() {
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

			status := aggregateStatus(fleet, instances, 2, true)

			Expect(status.AdmittedReplicas).To(Equal(4), "admittedReplicas")
			Expect(status.CreatedInstances).To(Equal(2), "createdInstances")
			Expect(status.GuestReadyInstances).To(Equal(1), "guestReadyInstances")
			Expect(status.ProvisionedInstances).To(Equal(1), "provisionedInstances")
			Expect(status.FailedInstances).To(Equal(1), "failedInstances")
			Expect(status.DPUNodes).To(Equal(2), "dpuNodes")
			expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionFalse, "Progressing")
			expectCondition(status, scalev1alpha1.ConditionDPUsReconciled, metav1.ConditionFalse, "WaitingForDPUs")
			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionFalse, "Progressing")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionFalse, "Progressing")
		})

		It("reports a ready fleet", func() {
			fleet := rolloutFleet(2, 2, 2)
			instances := instanceMap(
				observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
				observedInstance(fleet, 1, scalev1alpha1.SimInstanceReady, "dpu-1", readyInstanceConditions...),
			)

			status := aggregateStatus(fleet, instances, 1, true)

			expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionDPUsReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionTrue, "Ready")
		})

		It("keeps inventory progressing until every replica is admitted", func() {
			fleet := rolloutFleet(250, 10, 100)
			instances := instanceMap(fleetInstances(fleet, 0, 100, readyInstanceConditions...)...)

			status := aggregateStatus(fleet, instances, 10, true)

			expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionFalse, "Progressing")
		})

		It("keeps inventory progressing until every admitted instance exists", func() {
			fleet := rolloutFleet(2, 2, 2)
			instances := instanceMap(
				observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
			)

			status := aggregateStatus(fleet, instances, 1, true)

			expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionFalse, "Progressing")
		})

		DescribeTable("keeps inventory not ready with a failed or deleting instance",
			func(mutate func(*scalev1alpha1.SimInstance), wantVMReady, wantProvisioned int) {
				fleet := rolloutFleet(2, 2, 2)
				second := observedInstance(fleet, 1, scalev1alpha1.SimInstanceReady, "dpu-1", readyInstanceConditions...)
				mutate(second)
				instances := instanceMap(
					observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
					second,
				)

				status := aggregateStatus(fleet, instances, 1, true)

				expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionFalse, "Progressing")
				Expect(status.CreatedInstances).To(Equal(1), "createdInstances")
				Expect(status.GuestReadyInstances).To(Equal(wantVMReady), "guestReadyInstances")
				Expect(status.ProvisionedInstances).To(Equal(wantProvisioned), "provisionedInstances")
			},
			// A Failed instance keeps its VM and DPU until it is deleted, so only
			// createdInstances drops it; a deleting instance drops from every count.
			Entry("Failed", func(i *scalev1alpha1.SimInstance) { i.Status.Phase = scalev1alpha1.SimInstanceFailed }, 2, 2),
			Entry("being deleted", func(i *scalev1alpha1.SimInstance) {
				now := metav1.Now()
				i.DeletionTimestamp = &now
			}, 1, 1),
		)

		It("reports a fleet of zero replicas as ready", func() {
			status := aggregateStatus(rolloutFleet(0, 1, 0), nil, 0, true)

			expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionDPUsReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionTrue, "Ready")
		})

		It("reports duplicate DPUs", func() {
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

			status := aggregateStatus(fleet, instances, 1, true)

			expectCondition(status, scalev1alpha1.ConditionDPUsReconciled, metav1.ConditionFalse, "MultipleDPUs")
		})

		It("waits for virtual machines to be ready", func() {
			fleet := rolloutFleet(2, 2, 2)
			instances := instanceMap(fleetInstances(fleet, 0, 2,
				scalev1alpha1.SimInstanceCredentialsReady,
				scalev1alpha1.SimInstanceDeviceCreated,
				scalev1alpha1.SimInstanceChildrenCreated,
			)...)

			status := aggregateStatus(fleet, instances, 1, true)

			Expect(status.CreatedInstances).To(Equal(2), "createdInstances")
			Expect(status.GuestReadyInstances).To(Equal(0), "guestReadyInstances")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionFalse, "WaitingForGuests")
			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionTrue, "Ready")
		})

		It("is ready without a virtual machine template", func() {
			fleet := rolloutFleet(2, 2, 2)
			fleet.Spec.VirtualMachineTemplate = nil
			instances := instanceMap(fleetInstances(fleet, 0, 2,
				scalev1alpha1.SimInstanceCredentialsReady,
				scalev1alpha1.SimInstanceDeviceCreated,
				scalev1alpha1.SimInstanceChildrenCreated,
			)...)

			status := aggregateStatus(fleet, instances, 1, true)

			Expect(status.CreatedInstances).To(Equal(2), "createdInstances without a template")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionInventoryReconciled, metav1.ConditionTrue, "Ready")
		})

		It("reports an invalid virtual machine template", func() {
			fleet := rolloutFleet(1, 1, 1)
			fleet.Spec.VirtualMachineTemplate.Storage = &scalev1alpha1.VirtualMachineStorage{Ephemeral: ptr.To(false)}

			status := aggregateStatus(fleet, nil, 0, true)

			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionFalse, "UnsupportedStorage")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionFalse, "UnsupportedStorage")
		})

		It("reports KubeVirtNotInstalled for a virtual machine fleet on a cluster without KubeVirt", func() {
			fleet := rolloutFleet(1, 1, 1)

			status := aggregateStatus(fleet, nil, 0, false)

			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionFalse, "KubeVirtNotInstalled")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionFalse, "KubeVirtNotInstalled")
		})

		It("reports a pod fleet ready without KubeVirt", func() {
			fleet := rolloutFleet(2, 2, 2)
			fleet.Spec.VirtualMachineTemplate = nil
			fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{Image: "example.com/node@sha256:test"}
			instances := instanceMap(
				observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
				observedInstance(fleet, 1, scalev1alpha1.SimInstanceReady, "dpu-1", readyInstanceConditions...),
			)

			status := aggregateStatus(fleet, instances, 1, false)

			Expect(status.GuestReadyInstances).To(Equal(2), "guestReadyInstances")
			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionTrue, "Ready")
			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionTrue, "Ready")
		})

		It("waits for a pod fleet's guests", func() {
			fleet := rolloutFleet(2, 2, 2)
			fleet.Spec.VirtualMachineTemplate = nil
			fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{Image: "example.com/node@sha256:test"}
			instances := instanceMap(
				observedInstance(fleet, 0, scalev1alpha1.SimInstanceReady, "dpu-0", readyInstanceConditions...),
				observedInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning, "dpu-1",
					scalev1alpha1.SimInstanceCredentialsReady,
					scalev1alpha1.SimInstanceDeviceCreated,
					scalev1alpha1.SimInstanceChildrenCreated,
				),
			)

			status := aggregateStatus(fleet, instances, 1, false)

			expectCondition(status, scalev1alpha1.ConditionGuestsReady, metav1.ConditionFalse, "WaitingForGuests")
		})

		It("reports a blocked credential", func() {
			fleet := rolloutFleet(1, 1, 1)
			blocked := fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning)
			setInstanceCondition(blocked, scalev1alpha1.SimInstanceCredentialsReady, false,
				"OwnershipConflict", "ServiceAccount is controlled by another owner")

			status := aggregateStatus(fleet, instanceMap(blocked), 0, true)

			expectCondition(status, scalev1alpha1.ConditionBootstrapReconciled, metav1.ConditionFalse, "OwnershipConflict")
		})
	})

	Describe("bootstrapReconciledCondition", func() {
		withTemplate := &scalev1alpha1.SimFleet{
			Spec: scalev1alpha1.SimFleetSpec{
				VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{Image: "example.com/node@sha256:test"},
			},
		}
		blocked := &bootstrapConditionError{reason: "OwnershipConflict", message: "blocked"}

		DescribeTable("reports BootstrapReconciled",
			func(testCase bootstrapReadyCase) {
				condition := bootstrapReconciledCondition(testCase.fleet, testCase.bootstrap, testCase.desiredVMs)
				Expect(condition.Type).To(Equal(scalev1alpha1.ConditionBootstrapReconciled))
				Expect(condition.Status).To(Equal(testCase.status))
				Expect(condition.Reason).To(Equal(testCase.reason))
				Expect(condition.Message).NotTo(BeEmpty())
			},
			Entry("no template", bootstrapReadyCase{
				fleet:  &scalev1alpha1.SimFleet{},
				status: metav1.ConditionTrue,
				reason: "Ready",
			}),
			Entry("no device to boot", bootstrapReadyCase{
				fleet:      withTemplate,
				desiredVMs: 0,
				status:     metav1.ConditionTrue,
				reason:     "Ready",
			}),
			Entry("every guest Secret is written", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{cloudInit: map[int]struct{}{0: {}}},
				desiredVMs: 1,
				status:     metav1.ConditionTrue,
				reason:     "Ready",
			}),
			Entry("a guest Secret is not written yet", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{cloudInit: map[int]struct{}{}},
				desiredVMs: 1,
				status:     metav1.ConditionFalse,
				reason:     "Progressing",
			}),
			Entry("blocked instance", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{cloudInit: map[int]struct{}{}, blocked: blocked},
				desiredVMs: 1,
				status:     metav1.ConditionFalse,
				reason:     "OwnershipConflict",
			}),
			Entry("blocked instance with no desired VM", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{blocked: blocked},
				desiredVMs: 0,
				status:     metav1.ConditionTrue,
				reason:     "Ready",
			}),
			Entry("bootstrap reason", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{err: &bootstrapConditionError{reason: "SomethingRefused", message: "failed"}},
				desiredVMs: 1,
				status:     metav1.ConditionFalse,
				reason:     "SomethingRefused",
			}),
			Entry("error wins over no template", bootstrapReadyCase{
				fleet:     &scalev1alpha1.SimFleet{},
				bootstrap: bootstrapOutcome{err: errors.New("boom")},
				status:    metav1.ConditionFalse,
				reason:    "ReconcileError",
			}),
			Entry("error wins over no desired VM", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{err: errors.New("boom"), blocked: blocked},
				desiredVMs: 0,
				status:     metav1.ConditionFalse,
				reason:     "ReconcileError",
			}),
			Entry("template validation reason", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{err: kubevirt.ErrUnsupportedStorage},
				desiredVMs: 1,
				status:     metav1.ConditionFalse,
				reason:     "UnsupportedStorage",
			}),
			Entry("untyped error", bootstrapReadyCase{
				fleet:      withTemplate,
				bootstrap:  bootstrapOutcome{err: errors.New("boom")},
				desiredVMs: 1,
				status:     metav1.ConditionFalse,
				reason:     "ReconcileError",
			}),
		)
	})

	Describe("patchFleetStatus", func() {
		DescribeTable("never lowers admittedReplicas below the stored index except to replicas",
			func(replicas, stored, passed, want int) {
				h := newFleetHarness(rolloutFleet(replicas, 10, stored))
				fleet := h.fleet()

				Expect(h.reconciler.patchFleetStatus(context.Background(), fleet, passed, nil, 0, "")).To(Succeed())

				Expect(h.fleet().Status.AdmittedReplicas).To(Equal(want))
			},
			Entry("lower pass value keeps the stored index", 250, 200, 100, 200),
			Entry("higher pass value raises it", 250, 100, 200, 200),
			Entry("scale-down lowers it to replicas", 50, 200, 50, 50),
		)
	})
})
