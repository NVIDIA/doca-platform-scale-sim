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

	"github.com/go-logr/logr/funcr"
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

func rolloutFleet(replicas, devicesPerDPUNode, admitted int) *scalev1alpha1.SimFleet {
	fleet := instanceFleet()
	fleet.Spec.Replicas = replicas
	fleet.Spec.DevicesPerDPUNode = devicesPerDPUNode
	fleet.Status.AdmittedReplicas = admitted
	return fleet
}

// fleetInstance is an instance of fleet at ordinal with each of trueConditions
// set True, labelled and owned the way the fleet reconciler creates one.
func fleetInstance(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	phase scalev1alpha1.SimInstancePhase,
	trueConditions ...string,
) *scalev1alpha1.SimInstance {
	instance := newInstanceFixture(fleet, ordinal)
	instance.Labels = map[string]string{
		labelFleetName: fleet.Name,
		labelFleetUID:  string(fleet.UID),
		labelOrdinal:   fmt.Sprintf("%06d", ordinal),
	}
	instance.Status.Phase = phase
	for _, conditionType := range trueConditions {
		setInstanceCondition(instance, conditionType, true, "Ready", conditionType)
	}
	return instance
}

func fleetInstances(
	fleet *scalev1alpha1.SimFleet,
	from, to int,
	trueConditions ...string,
) []client.Object {
	objects := make([]client.Object, 0, to-from)
	for ordinal := from; ordinal < to; ordinal++ {
		objects = append(objects, fleetInstance(fleet, ordinal, scalev1alpha1.SimInstanceProvisioning, trueConditions...))
	}
	return objects
}

func instanceMap(objects ...client.Object) map[int]*scalev1alpha1.SimInstance {
	instances := make(map[int]*scalev1alpha1.SimInstance, len(objects))
	for _, object := range objects {
		instance := object.(*scalev1alpha1.SimInstance)
		instances[instance.Spec.Ordinal] = instance
	}
	return instances
}

type fleetHarness struct {
	t          *testing.T
	client     client.Client
	reconciler *SimFleetReconciler
	key        client.ObjectKey
	// calls records SimFleet patches, SimInstance writes, and DPUNode writes
	// in order.
	calls []string
}

func newFleetHarness(t *testing.T, fleet *scalev1alpha1.SimFleet, objects ...client.Object) *fleetHarness {
	t.Helper()
	h := &fleetHarness{t: t, key: client.ObjectKeyFromObject(fleet)}
	scheme := instanceTestScheme(t)
	h.client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&scalev1alpha1.SimFleet{}, &scalev1alpha1.SimInstance{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch obj.(type) {
				case *scalev1alpha1.SimInstance:
					h.calls = append(h.calls, "create "+obj.GetName())
				case *provisioningv1.DPUNode:
					h.calls = append(h.calls, "new dpunode "+obj.GetName())
				}
				return c.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				switch obj.(type) {
				case *scalev1alpha1.SimInstance:
					h.calls = append(h.calls, "delete "+obj.GetName())
				case *provisioningv1.DPUNode:
					h.calls = append(h.calls, "drop dpunode "+obj.GetName())
				}
				return c.Delete(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.(type) {
				case *scalev1alpha1.SimFleet:
					h.calls = append(h.calls, fmt.Sprintf("finalizers %v", obj.GetFinalizers()))
				case *provisioningv1.DPUNode:
					h.calls = append(h.calls, "patch dpunode "+obj.GetName())
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourcePatch: func(
				ctx context.Context,
				c client.Client,
				subResourceName string,
				obj client.Object,
				patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				if fleet, ok := obj.(*scalev1alpha1.SimFleet); ok {
					h.calls = append(h.calls, fmt.Sprintf("status admitted=%d", fleet.Status.AdmittedReplicas))
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		WithObjects(append([]client.Object{fleet}, objects...)...).
		Build()
	h.reconciler = &SimFleetReconciler{Client: h.client, Scheme: scheme}
	return h
}

func (h *fleetHarness) reconcile() {
	h.t.Helper()
	result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	if err != nil {
		h.t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (ctrl.Result{}) {
		h.t.Fatalf("Reconcile() result = %+v, want empty", result)
	}
}

func (h *fleetHarness) fleet() *scalev1alpha1.SimFleet {
	h.t.Helper()
	fleet := &scalev1alpha1.SimFleet{}
	if err := h.client.Get(context.Background(), h.key, fleet); err != nil {
		h.t.Fatalf("get SimFleet: %v", err)
	}
	return fleet
}

func (h *fleetHarness) instances() map[int]*scalev1alpha1.SimInstance {
	h.t.Helper()
	list := &scalev1alpha1.SimInstanceList{}
	if err := h.client.List(context.Background(), list, client.InNamespace(h.key.Namespace)); err != nil {
		h.t.Fatalf("list SimInstances: %v", err)
	}
	instances := make(map[int]*scalev1alpha1.SimInstance, len(list.Items))
	for i := range list.Items {
		instances[list.Items[i].Spec.Ordinal] = &list.Items[i]
	}
	return instances
}

func (h *fleetHarness) fleetGone() bool {
	h.t.Helper()
	err := h.client.Get(context.Background(), h.key, &scalev1alpha1.SimFleet{})
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("get SimFleet: %v", err)
	}
	return apierrors.IsNotFound(err)
}

// releaseInstances stands in for each instance's teardown finishing.
func (h *fleetHarness) releaseInstances() {
	h.t.Helper()
	for _, instance := range h.instances() {
		base := instance.DeepCopy()
		controllerutil.RemoveFinalizer(instance, simInstanceTeardownFinalizer)
		if err := h.client.Patch(context.Background(), instance, client.MergeFrom(base)); err != nil {
			h.t.Fatalf("release SimInstance %s: %v", instance.Name, err)
		}
	}
	h.calls = nil
}

// callsWithPrefix keeps the recorded calls matching any of prefixes, in order.
func (h *fleetHarness) callsWithPrefix(prefixes ...string) []string {
	var calls []string
	for _, call := range h.calls {
		for _, prefix := range prefixes {
			if strings.HasPrefix(call, prefix) {
				calls = append(calls, call)
				break
			}
		}
	}
	return calls
}

func (h *fleetHarness) expectNode(fleet *scalev1alpha1.SimFleet, hostIndex int, ordinals ...int) {
	h.t.Helper()
	node := &provisioningv1.DPUNode{}
	key := client.ObjectKey{Name: utils.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
	if err := h.client.Get(context.Background(), key, node); err != nil {
		h.t.Fatalf("get DPUNode for host %d: %v", hostIndex, err)
	}
	got := make([]string, 0, len(node.Spec.DPUs))
	for _, dpu := range node.Spec.DPUs {
		got = append(got, dpu.Name)
	}
	want := make([]string, 0, len(ordinals))
	for _, ordinal := range ordinals {
		want = append(want, utils.DeviceName(fleet.Name, ordinal))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		h.t.Fatalf("DPUNode host %d DPUs = %v, want %v", hostIndex, got, want)
	}
	if owner := metav1.GetControllerOf(node); owner == nil || owner.UID != fleet.UID {
		h.t.Fatalf("DPUNode host %d controller owner = %+v, want the SimFleet", hostIndex, owner)
	}
}

func (h *fleetHarness) expectNoNode(fleet *scalev1alpha1.SimFleet, hostIndex int) {
	h.t.Helper()
	key := client.ObjectKey{Name: utils.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
	err := h.client.Get(context.Background(), key, &provisioningv1.DPUNode{})
	if !apierrors.IsNotFound(err) {
		h.t.Fatalf("Get(DPUNode host %d) error = %v, want NotFound", hostIndex, err)
	}
}

func TestNextAdmitted(t *testing.T) {
	fleet := rolloutFleet(250, 10, 0)
	allCreated := func(from, to int) map[int]*scalev1alpha1.SimInstance {
		return instanceMap(fleetInstances(fleet, from, to, scalev1alpha1.SimInstanceChildrenCreated)...)
	}
	oneMissing := allCreated(0, 100)
	oneMissing[42] = fleetInstance(fleet, 42, scalev1alpha1.SimInstanceProvisioning,
		scalev1alpha1.SimInstanceDeviceCreated)
	gap := allCreated(0, 100)
	delete(gap, 7)
	oneFailed := allCreated(0, 100)
	oneFailed[42] = fleetInstance(fleet, 42, scalev1alpha1.SimInstanceFailed,
		scalev1alpha1.SimInstanceChildrenCreated)
	oneDeleting := allCreated(0, 100)
	now := metav1.Now()
	oneDeleting[42].DeletionTimestamp = &now

	for _, test := range []struct {
		name      string
		admitted  int
		instances map[int]*scalev1alpha1.SimInstance
		want      int
	}{
		{"nothing admitted", 0, nil, 100},
		{"one admitted instance without ChildrenCreated", 100, oneMissing, 100},
		{"one admitted instance missing", 100, gap, 100},
		{"one admitted instance Failed", 100, oneFailed, 100},
		{"one admitted instance being deleted", 100, oneDeleting, 100},
		{"every admitted instance created", 100, allCreated(0, 100), 200},
		{"capped at replicas", 200, allCreated(0, 200), 250},
		{"already at replicas", 250, allCreated(0, 250), 250},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nextAdmitted(fleet, test.admitted, test.instances); got != test.want {
				t.Fatalf("nextAdmitted(%d) = %d, want %d", test.admitted, got, test.want)
			}
		})
	}
}

func TestDesiredInstance(t *testing.T) {
	fleet := rolloutFleet(250, 10, 0)
	instance := desiredInstance(fleet, 42)

	if instance.Name != utils.InstanceName(fleet.Name, 42) || instance.Namespace != fleet.Namespace {
		t.Fatalf("instance = %s/%s, want %s/%s",
			instance.Namespace, instance.Name, fleet.Namespace, utils.InstanceName(fleet.Name, 42))
	}
	wantLabels := map[string]string{
		labelFleetName: fleet.Name,
		labelFleetUID:  string(fleet.UID),
		labelOrdinal:   "000042",
	}
	for key, want := range wantLabels {
		if got := instance.Labels[key]; got != want {
			t.Fatalf("label %s = %q, want %q", key, got, want)
		}
	}
	if instance.Spec.Ordinal != 42 || instance.Spec.HostIndex != 4 {
		t.Fatalf("spec = %+v, want ordinal 42 hostIndex 4", instance.Spec)
	}
	owner := metav1.GetControllerOf(instance)
	if owner == nil || owner.UID != fleet.UID || owner.Kind != "SimFleet" {
		t.Fatalf("controller owner = %+v, want SimFleet %s", owner, fleet.UID)
	}
}

func TestSimFleetReconcileAdmitsFirstBatchBeforeCreatingIt(t *testing.T) {
	h := newFleetHarness(t, rolloutFleet(250, 10, 0))

	h.reconcile()

	instances := h.instances()
	if len(instances) != admitBatch {
		t.Fatalf("created %d SimInstances, want %d", len(instances), admitBatch)
	}
	for ordinal := range admitBatch {
		if instances[ordinal] == nil {
			t.Fatalf("SimInstance for ordinal %d was not created", ordinal)
		}
	}
	if got := h.fleet().Status.AdmittedReplicas; got != admitBatch {
		t.Fatalf("admittedReplicas = %d, want %d", got, admitBatch)
	}
	writes := h.callsWithPrefix("status ", "create ")
	if len(writes) == 0 || writes[0] != fmt.Sprintf("status admitted=%d", admitBatch) {
		t.Fatalf("writes = %v, want the admittedReplicas status patch before any create", h.calls)
	}
	if creates := h.callsWithPrefix("create "); len(creates) != admitBatch {
		t.Fatalf("SimInstance creates = %d, want %d", len(creates), admitBatch)
	}
}

func TestSimFleetReconcileReportsAnInstanceNameHeldByAnotherOwner(t *testing.T) {
	fleet := rolloutFleet(2, 1, 2)
	foreign := fleetInstance(fleet, 1, scalev1alpha1.SimInstanceReady)
	foreign.OwnerReferences = foreignOwner()
	h := newFleetHarness(t, fleet, foreign)

	h.reconcile()

	if h.instances()[0] == nil {
		t.Fatal("the conflict on ordinal 1 stopped ordinal 0 from being created")
	}
	status := h.fleet().Status
	expectCondition(t, status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "OwnershipConflict")
	condition := meta.FindStatusCondition(status.Conditions, scalev1alpha1.ConditionInventoryReady)
	if !strings.Contains(condition.Message, foreign.Name) {
		t.Fatalf("InventoryReady message = %q, want it to name %s", condition.Message, foreign.Name)
	}
	current := &scalev1alpha1.SimInstance{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(foreign), current); err != nil {
		t.Fatalf("get foreign SimInstance: %v", err)
	}
	if !current.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(current.OwnerReferences, foreign.OwnerReferences) {
		t.Fatalf("foreign SimInstance was modified: %+v", current.ObjectMeta)
	}
}

// An earlier fleet's instance of the same name that is still tearing down is
// waited out: the create is skipped and logged, and it is not a conflict.
func TestSimFleetReconcileWaitsOutATerminatingEarlierInstance(t *testing.T) {
	fleet := rolloutFleet(1, 1, 1)
	earlier := fleetInstance(fleet, 0, scalev1alpha1.SimInstanceTerminating)
	earlier.OwnerReferences[0].UID = "earlier-fleet-uid"
	earlier.Finalizers = []string{simInstanceTeardownFinalizer}
	now := metav1.Now()
	earlier.DeletionTimestamp = &now
	h := newFleetHarness(t, fleet, earlier)
	var logs []string
	logger := funcr.New(func(prefix, args string) {
		logs = append(logs, args)
	}, funcr.Options{Verbosity: 1})

	_, err := h.reconciler.Reconcile(logf.IntoContext(context.Background(), logger), ctrl.Request{NamespacedName: h.key})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	expectCondition(t, h.fleet().Status, scalev1alpha1.ConditionInventoryReady, metav1.ConditionFalse, "Progressing")
	for _, line := range logs {
		if strings.Contains(line, `"msg"="Skipped creating SimInstance that still exists"`) &&
			strings.Contains(line, `"simInstance"="`+earlier.Name+`"`) {
			return
		}
	}
	t.Fatalf("logs = %v, want a skipped create of %s", logs, earlier.Name)
}

func TestPersistAdmittedConflictsWithAStaleFleet(t *testing.T) {
	fleet := rolloutFleet(500, 10, 100)
	h := newFleetHarness(t, fleet)
	stale := h.fleet()
	current := stale.DeepCopy()
	current.Status.AdmittedReplicas = 300
	if err := h.client.Status().Update(context.Background(), current); err != nil {
		t.Fatalf("advance SimFleet status: %v", err)
	}

	err := h.reconciler.persistAdmitted(context.Background(), stale, 200)

	if !apierrors.IsConflict(err) {
		t.Fatalf("persistAdmitted() error = %v, want a conflict", err)
	}
	if got := h.fleet().Status.AdmittedReplicas; got != 300 {
		t.Fatalf("admittedReplicas = %d, want 300", got)
	}
}

func TestSimFleetReconcileRecreatesADeletedInstanceBelowAdmitted(t *testing.T) {
	fleet := rolloutFleet(3, 1, 3)
	h := newFleetHarness(t, fleet,
		fleetInstance(fleet, 0, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
		fleetInstance(fleet, 2, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
	)

	h.reconcile()

	if h.instances()[1] == nil {
		t.Fatal("SimInstance for ordinal 1 was not recreated")
	}
	if creates := h.callsWithPrefix("create "); len(creates) != 1 ||
		creates[0] != "create "+utils.InstanceName(fleet.Name, 1) {
		t.Fatalf("creates = %v, want only ordinal 1", creates)
	}
}

func TestSimFleetReconcileReplacesAFailedInstance(t *testing.T) {
	fleet := rolloutFleet(3, 1, 3)
	failed := fleetInstance(fleet, 1, scalev1alpha1.SimInstanceFailed, scalev1alpha1.SimInstanceChildrenCreated)
	h := newFleetHarness(t, fleet, append(
		fleetInstances(fleet, 0, 1, scalev1alpha1.SimInstanceChildrenCreated),
		failed,
		fleetInstance(fleet, 2, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
	)...)

	h.reconcile()

	if deletes := h.callsWithPrefix("delete "); len(deletes) != 1 || deletes[0] != "delete "+failed.Name {
		t.Fatalf("deletes = %v, want only the Failed instance", deletes)
	}
	err := h.client.Get(context.Background(), client.ObjectKeyFromObject(failed), &scalev1alpha1.SimInstance{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get(Failed instance) error = %v, want NotFound", err)
	}

	h.reconcile()

	if replacement := h.instances()[1]; replacement == nil || replacement.Status.Phase == scalev1alpha1.SimInstanceFailed {
		t.Fatalf("replacement = %+v, want a fresh instance for ordinal 1", replacement)
	}
}

// A Failed instance whose teardown is already running is not deleted again
// and not replaced until it is gone.
func TestSimFleetReconcileLeavesAFailedInstanceAloneWhileItTearsDown(t *testing.T) {
	fleet := rolloutFleet(3, 1, 3)
	failed := fleetInstance(fleet, 1, scalev1alpha1.SimInstanceFailed, scalev1alpha1.SimInstanceChildrenCreated)
	failed.Status.FailureReason = "DPUDevice"
	failed.Finalizers = []string{simInstanceTeardownFinalizer}
	now := metav1.Now()
	failed.DeletionTimestamp = &now
	h := newFleetHarness(t, fleet, append(
		fleetInstances(fleet, 0, 1, scalev1alpha1.SimInstanceChildrenCreated),
		failed,
		fleetInstance(fleet, 2, scalev1alpha1.SimInstanceReady, scalev1alpha1.SimInstanceChildrenCreated),
	)...)

	h.reconcile()

	if writes := h.callsWithPrefix("delete ", "create "); len(writes) != 0 {
		t.Fatalf("SimInstance writes = %v, want none while ordinal 1 tears down", writes)
	}
}

func TestSimFleetReconcileScalesDown(t *testing.T) {
	fleet := rolloutFleet(150, 10, 150)
	h := newFleetHarness(t, fleet, fleetInstances(fleet, 0, 150, scalev1alpha1.SimInstanceChildrenCreated)...)
	current := h.fleet()
	current.Spec.Replicas = 50
	if err := h.client.Update(context.Background(), current); err != nil {
		t.Fatalf("scale SimFleet: %v", err)
	}

	h.reconcile()

	instances := h.instances()
	if len(instances) != 50 {
		t.Fatalf("SimInstances = %d, want 50", len(instances))
	}
	for ordinal := range 50 {
		if instances[ordinal] == nil {
			t.Fatalf("SimInstance for ordinal %d was deleted", ordinal)
		}
	}
	if deletes := h.callsWithPrefix("delete "); len(deletes) != 100 {
		t.Fatalf("deletes = %d, want 100", len(deletes))
	}
	if got := h.fleet().Status.AdmittedReplicas; got != 50 {
		t.Fatalf("admittedReplicas = %d, want 50", got)
	}
}

func TestSimFleetReconcileCreatesDPUNodesForCreatedHosts(t *testing.T) {
	fleet := rolloutFleet(5, 2, 5)
	h := newFleetHarness(t, fleet,
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
	if got := h.fleet().Status.DPUNodes; got != 2 {
		t.Fatalf("status.dpuNodes = %d, want 2", got)
	}
}

func TestSimFleetReconcileGivesThePartialAdmittedHostItsAdmittedDevices(t *testing.T) {
	fleet := rolloutFleet(5, 2, 3)
	h := newFleetHarness(t, fleet, fleetInstances(fleet, 0, 3, scalev1alpha1.SimInstanceDeviceCreated)...)

	h.reconcile()

	h.expectNode(fleet, 0, 0, 1)
	h.expectNode(fleet, 1, 2)
	h.expectNoNode(fleet, 2)
}

func TestSimFleetReconcileAddsAMissingDeviceToAnExistingDPUNode(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	node := ownedHostNode(fleet, 0)
	node.Spec.DPUs = dpuRefs(utils.DeviceName(fleet.Name, 0), "added-by-dpf")
	h := newFleetHarness(t, fleet, append(
		fleetInstances(fleet, 0, 2, scalev1alpha1.SimInstanceDeviceCreated, scalev1alpha1.SimInstanceChildrenCreated),
		node,
	)...)

	h.reconcile()

	current := &provisioningv1.DPUNode{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(node), current); err != nil {
		t.Fatalf("get DPUNode: %v", err)
	}
	want := dpuRefs(utils.DeviceName(fleet.Name, 0), "added-by-dpf", utils.DeviceName(fleet.Name, 1))
	if !equality.Semantic.DeepEqual(current.Spec.DPUs, want) {
		t.Fatalf("DPUs = %v, want %v", current.Spec.DPUs, want)
	}
	if patches := h.callsWithPrefix("patch dpunode "); len(patches) != 1 {
		t.Fatalf("DPUNode patches = %v, want one", patches)
	}

	h.calls = nil
	h.reconcile()
	if patches := h.callsWithPrefix("patch dpunode "); len(patches) != 0 {
		t.Fatalf("DPUNode patches = %v, want none once it lists every device", patches)
	}
}

func TestSimFleetReconcileRefusesADPUNodeWithoutAController(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	orphan := desiredHostNode(fleet, 0, fleet.Spec.Replicas)
	orphan.Spec.DPUs = nil
	h := newFleetHarness(t, fleet, append(
		fleetInstances(fleet, 0, 2, scalev1alpha1.SimInstanceDeviceCreated, scalev1alpha1.SimInstanceChildrenCreated),
		orphan,
	)...)

	_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})

	if err == nil || !strings.Contains(err.Error(), "missing owner reference") {
		t.Fatalf("Reconcile() error = %v, want the missing owner reference", err)
	}
	if writes := h.callsWithPrefix("patch dpunode ", "new dpunode "); len(writes) != 0 {
		t.Fatalf("DPUNode writes = %v, want none", writes)
	}
}

// A host whose devices are not all created keeps the DPUNode it already has:
// it is counted, and not patched until the missing device exists.
func TestSimFleetReconcileCountsAnExistingDPUNodeOfAnIncompleteHost(t *testing.T) {
	fleet := rolloutFleet(2, 2, 2)
	h := newFleetHarness(t, fleet,
		fleetInstance(fleet, 0, scalev1alpha1.SimInstanceProvisioning, scalev1alpha1.SimInstanceDeviceCreated),
		fleetInstance(fleet, 1, scalev1alpha1.SimInstanceProvisioning),
		ownedHostNode(fleet, 0),
	)

	h.reconcile()

	if got := h.fleet().Status.DPUNodes; got != 1 {
		t.Fatalf("status.dpuNodes = %d, want 1", got)
	}
	if writes := h.callsWithPrefix("patch dpunode ", "new dpunode ", "drop dpunode "); len(writes) != 0 {
		t.Fatalf("DPUNode writes = %v, want none", writes)
	}
}

func TestDesiredHostNodeUsesTheDPUNodeTemplate(t *testing.T) {
	fleet := rolloutFleet(4, 2, 4)

	if method := desiredHostNode(fleet, 0, 4).Spec.NodeRebootMethod; method == nil || method.External == nil {
		t.Fatalf("nodeRebootMethod = %+v, want External without a template", method)
	}

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

	if node.Labels["team"] != "scale" {
		t.Fatalf("labels = %v, want the template label team=scale", node.Labels)
	}
	if node.Labels[labelOrdinal] != "0001" {
		t.Fatalf("ordinal label = %q, want the host index 0001 over the template", node.Labels[labelOrdinal])
	}
	method := node.Spec.NodeRebootMethod
	if method == nil || method.HostAgent == nil || method.External != nil {
		t.Fatalf("nodeRebootMethod = %+v, want the template's HostAgent", method)
	}
	if method == fleet.Spec.DPUNodeTemplate.Spec.NodeRebootMethod {
		t.Fatal("nodeRebootMethod aliases the fleet template")
	}
}

func dpuRefs(names ...string) []provisioningv1.DPURef {
	refs := make([]provisioningv1.DPURef, 0, len(names))
	for _, name := range names {
		refs = append(refs, provisioningv1.DPURef{Name: name})
	}
	return refs
}

// A node missing any desired ref has to be one the patch path can bring back
// into line, without dropping a ref someone else added.
func TestAppendMissingDPUs(t *testing.T) {
	tests := []struct {
		name     string
		existing []provisioningv1.DPURef
		desired  []provisioningv1.DPURef
		want     []provisioningv1.DPURef
	}{
		{
			name:    "appends every missing ref to an empty node",
			desired: dpuRefs("d0", "d1"),
			want:    dpuRefs("d0", "d1"),
		},
		{
			name:     "leaves a node that already lists every desired ref",
			existing: dpuRefs("d0", "d1"),
			desired:  dpuRefs("d0", "d1"),
			want:     dpuRefs("d0", "d1"),
		},
		{
			name:     "keeps refs the fleet no longer desires",
			existing: dpuRefs("d0", "stale"),
			desired:  dpuRefs("d0", "d1"),
			want:     dpuRefs("d0", "stale", "d1"),
		},
		{
			name:     "restores a ref replaced by an equal number of foreign refs",
			existing: dpuRefs("foreign"),
			desired:  dpuRefs("d0"),
			want:     dpuRefs("foreign", "d0"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := appendMissingDPUs(test.existing, test.desired)
			if !equality.Semantic.DeepEqual(got, test.want) {
				t.Fatalf("appendMissingDPUs() = %#v, want %#v", got, test.want)
			}
		})
	}
}

// A Create that returns AlreadyExists while the cache has not caught up leaves
// the follow-up Get empty. That is cache lag, not a broken fleet, so the pass
// has to report the node missing and come back rather than fail the reconcile.
func TestEnsureNodeToleratesCacheLag(t *testing.T) {
	fleet := inventoryFleet()
	scheme := runtime.NewScheme()
	if err := scalev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add SimFleet types to scheme: %v", err)
	}
	if err := provisioningv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add DPF types to scheme: %v", err)
	}

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
	if err != nil || ready {
		t.Fatalf("ensureNode() = (%t, %v), want (false, nil)", ready, err)
	}
}
