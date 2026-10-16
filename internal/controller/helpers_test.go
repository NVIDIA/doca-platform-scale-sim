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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpf"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

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
			Name:      naming.InstanceName(fleet.Name, ordinal),
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
			HostIndex: naming.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
		},
	}
}

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
		naming.LabelFleetName: fleet.Name,
		naming.LabelFleetUID:  string(fleet.UID),
		naming.LabelOrdinal:   fmt.Sprintf("%06d", ordinal),
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

func ownedHostNode(fleet *scalev1alpha1.SimFleet, hostIndex int) *provisioningv1.DPUNode {
	node := dpf.DesiredHostNode(fleet, hostIndex, fleet.Spec.Replicas)
	node.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(fleet, scalev1alpha1.GroupVersion.WithKind("SimFleet")),
	}
	return node
}

func inventoryFleet() *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000",
			Namespace: "ns",
			UID:       types.UID("uid"),
		},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              20,
			DevicesPerDPUNode:     10,
			DeviceSelectionLabels: map[string]string{"scale.dpu.nvidia.com/pool": "scale-3000"},
		},
	}
}

func fleetWith(agent *scalev1alpha1.DPUAgentSpec) *scalev1alpha1.SimFleet {
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "test-ns"},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              1,
			DevicesPerDPUNode:     1,
			DeviceSelectionLabels: map[string]string{"pool": "scale-3000"},
		},
	}
	if agent != nil {
		fleet.Spec.DPUAgent = agent
	}
	return fleet
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

func instanceTestScheme() *runtime.Scheme {
	GinkgoHelper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		scalev1alpha1.AddToScheme,
		provisioningv1.AddToScheme,
		virtv1.AddToScheme,
	} {
		Expect(add(scheme)).To(Succeed(), "build scheme")
	}
	return scheme
}

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

func expectCondition(
	status scalev1alpha1.SimFleetStatus,
	conditionType string,
	want metav1.ConditionStatus,
	reason string,
) {
	GinkgoHelper()
	condition := meta.FindStatusCondition(status.Conditions, conditionType)
	Expect(condition).NotTo(BeNil(), "condition %s missing from %+v", conditionType, status.Conditions)
	Expect(condition.Status).To(Equal(want), "condition %s status (%s)", conditionType, condition.Message)
	Expect(condition.Reason).To(Equal(reason), "condition %s reason (%s)", conditionType, condition.Message)
}

// Kinds of client write the fleet harness records. Each recorded call is the
// kind, a space, and the written object's name or state.
const (
	callFinalizers     = "finalizers"
	callStatus         = "status"
	callCreateInstance = "create"
	callDeleteInstance = "delete"
	callCreateNode     = "new dpunode"
	callPatchNode      = "patch dpunode"
	callDeleteNode     = "drop dpunode"
)

// nodeCalls are every kind of DPUNode write.
var nodeCalls = []string{callCreateNode, callPatchNode, callDeleteNode}

// call formats a recorded write the way the fleet harness records it.
func call(kind, detail string) string {
	return kind + " " + detail
}

type fleetHarness struct {
	client     client.Client
	reconciler *SimFleetReconciler
	key        client.ObjectKey
	// calls records SimFleet patches, SimInstance writes, and DPUNode writes
	// in order.
	calls []string
}

func newFleetHarness(fleet *scalev1alpha1.SimFleet, objects ...client.Object) *fleetHarness {
	GinkgoHelper()
	h := &fleetHarness{key: client.ObjectKeyFromObject(fleet)}
	scheme := instanceTestScheme()
	h.client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&scalev1alpha1.SimFleet{}, &scalev1alpha1.SimInstance{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch obj.(type) {
				case *scalev1alpha1.SimInstance:
					h.calls = append(h.calls, call(callCreateInstance, obj.GetName()))
				case *provisioningv1.DPUNode:
					h.calls = append(h.calls, call(callCreateNode, obj.GetName()))
				}
				return c.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				switch obj.(type) {
				case *scalev1alpha1.SimInstance:
					h.calls = append(h.calls, call(callDeleteInstance, obj.GetName()))
				case *provisioningv1.DPUNode:
					h.calls = append(h.calls, call(callDeleteNode, obj.GetName()))
				}
				return c.Delete(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.(type) {
				case *scalev1alpha1.SimFleet:
					h.calls = append(h.calls, call(callFinalizers, fmt.Sprintf("%v", obj.GetFinalizers())))
				case *provisioningv1.DPUNode:
					h.calls = append(h.calls, call(callPatchNode, obj.GetName()))
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
					h.calls = append(h.calls, call(callStatus, fmt.Sprintf("admitted=%d", fleet.Status.AdmittedReplicas)))
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
	GinkgoHelper()
	result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	Expect(err).NotTo(HaveOccurred())
	Expect(result).To(Equal(ctrl.Result{}))
}

func (h *fleetHarness) fleet() *scalev1alpha1.SimFleet {
	GinkgoHelper()
	fleet := &scalev1alpha1.SimFleet{}
	Expect(h.client.Get(context.Background(), h.key, fleet)).To(Succeed(), "get SimFleet")
	return fleet
}

func (h *fleetHarness) instances() map[int]*scalev1alpha1.SimInstance {
	GinkgoHelper()
	list := &scalev1alpha1.SimInstanceList{}
	Expect(h.client.List(context.Background(), list, client.InNamespace(h.key.Namespace))).To(Succeed(), "list SimInstances")
	instances := make(map[int]*scalev1alpha1.SimInstance, len(list.Items))
	for i := range list.Items {
		instances[list.Items[i].Spec.Ordinal] = &list.Items[i]
	}
	return instances
}

func (h *fleetHarness) fleetGone() bool {
	GinkgoHelper()
	err := h.client.Get(context.Background(), h.key, &scalev1alpha1.SimFleet{})
	if apierrors.IsNotFound(err) {
		return true
	}
	Expect(err).NotTo(HaveOccurred(), "get SimFleet")
	return false
}

// releaseInstances stands in for each instance's teardown finishing.
func (h *fleetHarness) releaseInstances() {
	GinkgoHelper()
	for _, instance := range h.instances() {
		base := instance.DeepCopy()
		controllerutil.RemoveFinalizer(instance, scalev1alpha1.SimInstanceFinalizer)
		Expect(h.client.Patch(context.Background(), instance, client.MergeFrom(base))).
			To(Succeed(), "release SimInstance %s", instance.Name)
	}
	h.calls = nil
}

// callsOf returns the recorded client writes of any of kinds. Calls keep the
// order the client made them in, across kinds, so a test can assert how
// different kinds of write interleave.
func (h *fleetHarness) callsOf(kinds ...string) []string {
	var calls []string
	for _, recorded := range h.calls {
		for _, kind := range kinds {
			if strings.HasPrefix(recorded, kind+" ") {
				calls = append(calls, recorded)
				break
			}
		}
	}
	return calls
}

func (h *fleetHarness) expectNode(fleet *scalev1alpha1.SimFleet, hostIndex int, ordinals ...int) {
	GinkgoHelper()
	node := &provisioningv1.DPUNode{}
	key := client.ObjectKey{Name: naming.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
	Expect(h.client.Get(context.Background(), key, node)).To(Succeed(), "get DPUNode for host %d", hostIndex)
	got := make([]string, 0, len(node.Spec.DPUs))
	for _, dpu := range node.Spec.DPUs {
		got = append(got, dpu.Name)
	}
	want := make([]string, 0, len(ordinals))
	for _, ordinal := range ordinals {
		want = append(want, naming.DeviceName(fleet.Name, ordinal))
	}
	Expect(got).To(Equal(want), "DPUNode host %d DPUs", hostIndex)
	owner := metav1.GetControllerOf(node)
	Expect(owner).NotTo(BeNil(), "DPUNode host %d has no controller owner", hostIndex)
	Expect(owner.UID).To(Equal(fleet.UID), "DPUNode host %d controller owner = %+v, want the SimFleet", hostIndex, owner)
}

func (h *fleetHarness) expectNoNode(fleet *scalev1alpha1.SimFleet, hostIndex int) {
	GinkgoHelper()
	key := client.ObjectKey{Name: naming.NodeName(fleet.Name, hostIndex), Namespace: fleet.Namespace}
	err := h.client.Get(context.Background(), key, &provisioningv1.DPUNode{})
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Get(DPUNode host %d) error = %v, want NotFound", hostIndex, err)
}
