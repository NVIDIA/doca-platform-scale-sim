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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SimInstanceFinalizer holds a deleted SimInstance until its children are torn
// down in order.
const SimInstanceFinalizer = "scale.dpu.nvidia.com/siminstance"

// SimInstancePhase is the coarse state of one simulated DPU ordinal.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;Terminating
type SimInstancePhase string

// The phases a SimInstance moves through.
const (
	SimInstancePending      SimInstancePhase = "Pending"
	SimInstanceProvisioning SimInstancePhase = "Provisioning"
	SimInstanceReady        SimInstancePhase = "Ready"
	SimInstanceFailed       SimInstancePhase = "Failed"
	SimInstanceTerminating  SimInstancePhase = "Terminating"
)

// Condition types reported on SimInstance status.
const (
	SimInstanceCredentialsReady = "CredentialsReady"
	SimInstanceDeviceCreated    = "DeviceCreated"
	SimInstanceChildrenCreated  = "ChildrenCreated"
	SimInstanceGuestReady       = "GuestReady"
	SimInstanceSelectable       = "Selectable"
	SimInstanceDPUProvisioned   = "DPUProvisioned"
	SimInstanceTeardown         = "Teardown"
)

// SimInstanceSpec identifies one ordinal of the owning SimFleet.
type SimInstanceSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ordinal is immutable"
	Ordinal int `json:"ordinal"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hostIndex is immutable"
	HostIndex int `json:"hostIndex"`
}

// ObservedUIDs are the child UIDs recorded once the instance converged. A
// later mismatch means a child was replaced behind the controller.
type ObservedUIDs struct {
	DPU                    string `json:"dpu,omitempty"`
	DPUDevice              string `json:"dpuDevice,omitempty"`
	VirtualMachine         string `json:"virtualMachine,omitempty"`
	VirtualMachineInstance string `json:"virtualMachineInstance,omitempty"`
}

// SimInstanceStatus is the observed state of one ordinal.
type SimInstanceStatus struct {
	Phase              SimInstancePhase `json:"phase,omitempty"`
	ObservedUIDs       ObservedUIDs     `json:"observedUIDs,omitempty"`
	DPUName            string           `json:"dpuName,omitempty"`
	FailureReason      string           `json:"failureReason,omitempty"`
	TeardownStartedAt  *metav1.Time     `json:"teardownStartedAt,omitempty"`
	ObservedGeneration int64            `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=smi
// +kubebuilder:metadata:annotations=helm.sh/resource-policy=keep
// +kubebuilder:printcolumn:name="Ordinal",type=integer,JSONPath=".spec.ordinal"
// +kubebuilder:printcolumn:name="Host",type=integer,JSONPath=".spec.hostIndex"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='DPUProvisioned')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SimInstance is one simulated DPU ordinal of a SimFleet.
type SimInstance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimInstance
	// +required
	Spec SimInstanceSpec `json:"spec"`

	// status defines the observed state of SimInstance
	// +optional
	Status SimInstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimInstanceList contains a list of SimInstance
type SimInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SimInstance{}, &SimInstanceList{})
		return nil
	})
}
