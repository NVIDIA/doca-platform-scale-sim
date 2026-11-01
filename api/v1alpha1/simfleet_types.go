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
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SimFleetFinalizer holds a deleted SimFleet until every SimInstance it owns
// is gone.
const SimFleetFinalizer = "scale.dpu.nvidia.com/simfleet"

// Condition types reported on SimFleet status. Names follow the DPF
// conditions pattern: XXXReconciled means this controller has applied its
// changes, and XXXReady means those resources are ready.
const (
	// ConditionInventoryReconciled is true once SimInstances and DPUNodes
	// match the desired counts.
	ConditionInventoryReconciled = "InventoryReconciled"
	// ConditionDPUsReconciled is true once a DPU exists for every instance.
	// Provisioning those DPUs is reported separately.
	ConditionDPUsReconciled = "DPUsReconciled"
	// ConditionGuestsReady is true once every guest is ready.
	ConditionGuestsReady = "GuestsReady"
	// ConditionBootstrapReconciled is true once the guest bootstrap Secrets
	// are in place.
	ConditionBootstrapReconciled = "BootstrapReconciled"
)

// ReservedDeviceLabelKeys are the DPUDevice labels the SimFleet controller
// writes and nothing else may. They decide which fleet a device belongs to,
// which ordinal it is, and which DPUNode groups it, so the controller finds,
// adopts, and cleans up its own inventory by exactly these keys.
//
// The CEL rule on spec.deviceSelectionLabels rejects them at admission and
// the controller strips them from fleet-author input, including the DPUDevice
// template, which CEL does not cover. Keep the rule and the filter in step
// with this list.
var ReservedDeviceLabelKeys = []string{
	"scale.dpu.nvidia.com/fleet-name",
	"scale.dpu.nvidia.com/fleet-uid",
	"scale.dpu.nvidia.com/ordinal",
	provisioningv1.DPUNodeNameLabel,
}

// TemplateMetadata is the metadata copied onto the objects a template
// renders. Labels never override the controller's own identity labels.
type TemplateMetadata struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// DPUNodeTemplateSpec sets DPUNode spec fields when the DPUNode is created;
// later edits do not change existing DPUNodes. An unset nodeRebootMethod
// defaults to external: {}.
type DPUNodeTemplateSpec struct {
	// +optional
	NodeRebootMethod *provisioningv1.NodeRebootMethod `json:"nodeRebootMethod,omitempty"`
}

// DPUNodeTemplate shapes the DPUNode the fleet creates for each host.
type DPUNodeTemplate struct {
	// +optional
	Metadata TemplateMetadata `json:"metadata,omitempty"`
	// +optional
	Spec DPUNodeTemplateSpec `json:"spec,omitempty"`
}

// DPUDeviceTemplateSpec sets DPUDevice spec fields when the DPUDevice is
// created; later edits do not change existing DPUDevices. An unset numberOfPFs
// defaults to 1.
type DPUDeviceTemplateSpec struct {
	// +optional
	NumberOfPFs *int `json:"numberOfPFs,omitempty"`
}

// DPUDeviceTemplate shapes the DPUDevice created for each ordinal. Reserved
// device label keys in its labels are silently dropped.
type DPUDeviceTemplate struct {
	// +optional
	Metadata TemplateMetadata `json:"metadata,omitempty"`
	// +optional
	Spec DPUDeviceTemplateSpec `json:"spec,omitempty"`
}

// VirtualMachineResources are the guest's resource requests. A missing cpu
// request defaults to 1 and a missing memory request to 1Gi; other requests
// pass through unchanged.
type VirtualMachineResources struct {
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`
}

// VirtualMachineStorage describes the guest's root disk. Only ephemeral
// container-disk storage is supported; the controller refuses ephemeral: false.
// rootDiskSize is accepted but not yet applied.
type VirtualMachineStorage struct {
	// +optional
	RootDiskSize string `json:"rootDiskSize,omitempty"`
	// +kubebuilder:default=true
	// +optional
	Ephemeral *bool `json:"ephemeral,omitempty"`
}

// PodNetwork configures the guest's pod network interface. Only bridge
// binding is supported; the controller refuses bridge: false.
type PodNetwork struct {
	// +kubebuilder:default=true
	// +optional
	Bridge *bool `json:"bridge,omitempty"`
}

// VirtualMachineTemplate shapes the KubeVirt VirtualMachine that backs each
// simulated DPU. A fleet without one boots no guests.
type VirtualMachineTemplate struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Image string `json:"image"`

	// Guest ISA copied onto the KubeVirt VirtualMachine.
	// +kubebuilder:default=amd64
	// +kubebuilder:validation:Enum=amd64;arm64
	// +optional
	Architecture string `json:"architecture,omitempty"`

	// Unsupported values are rejected by the controller rather than the API
	// server, so the status condition can name the strategy that was refused.
	// +kubebuilder:default=Always
	// +optional
	RunStrategy string `json:"runStrategy,omitempty"`

	// +optional
	Resources *VirtualMachineResources `json:"resources,omitempty"`

	// +optional
	Storage *VirtualMachineStorage `json:"storage,omitempty"`

	// +optional
	PodNetwork *PodNetwork `json:"podNetwork,omitempty"`

	// +optional
	Metadata TemplateMetadata `json:"metadata,omitempty"`

	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// DPUClusterReference names a DPF DPUCluster.
type DPUClusterReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// BootstrapSpec records the DPUCluster the fleet's guests are expected to
// join. The controller does not read or contact that cluster; it writes the
// reference into each guest's identity file. The cluster a guest joins is
// decided by the join Secret DPF creates for its DPU.
type BootstrapSpec struct {
	DPUClusterRef DPUClusterReference `json:"dpuClusterRef"`
}

// DPUAgentSpec configures the mock DPU agent each guest runs. Disabling it
// also stops the guest from joining the DPU cluster: the guest gets no agent
// configuration and no management credential, and both the agent and the
// join unit are masked.
type DPUAgentSpec struct {
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// +kubebuilder:default=bf3
	// +kubebuilder:validation:Enum=bf3;bf4
	// +optional
	DPUType *string `json:"dpuType,omitempty"`

	// Comma-separated mock reboot methods. Kind e2e uses the default.
	// +kubebuilder:default="NoAction"
	// +kubebuilder:validation:Pattern=`^(NoAction|PowerCycle|SLR)(,(NoAction|PowerCycle|SLR))*$`
	// +optional
	RebootMethod *string `json:"rebootMethod,omitempty"`

	// Mock BMC settings. Unset fields keep the values the controller has
	// always rendered, which are the mock's own defaults.
	// +optional
	BMC *DPUAgentBMCSpec `json:"bmc,omitempty"`

	// Extra time the mock spends in the steps it controls while the DPU is in
	// a phase; see the mock-dpuagent "Phase delays" section. The range applies
	// on every pass through the phase. OS Installing counts against DPF's
	// --os-install-timeout.
	// +listType=map
	// +listMapKey=phase
	// +optional
	PhaseDelays []DPUAgentPhaseDelay `json:"phaseDelays,omitempty"`
}

// DPUAgentPhaseDelay is a closed range of whole seconds the mock adds to one
// DPU phase.
// +kubebuilder:validation:XValidation:rule="self.minSeconds <= self.maxSeconds",message="minSeconds must not exceed maxSeconds"
type DPUAgentPhaseDelay struct {
	// DPU phase as shown in DPU.status.phase, see MockDelayablePhases.
	// +kubebuilder:validation:Enum="Update Firmware";"OS Installing";Rebooting;"DPU Config";"DPU Cluster Config";"Service Readiness"
	// +required
	Phase string `json:"phase"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +required
	MinSeconds int32 `json:"minSeconds"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +required
	MaxSeconds int32 `json:"maxSeconds"`
}

// DPUAgentBMCSpec configures the mock BMC; see the mock-dpuagent bmc block.
type DPUAgentBMCSpec struct {
	// PSID the mock BMC reports. Only BF4 reads it: it must be a key of
	// BlueFieldSoftware.spec.pldmFwBundle.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +optional
	PSID *string `json:"psid,omitempty"`

	// Firmware versions the mock BMC reports at power-on.
	// +optional
	Firmware *DPUAgentFirmware `json:"firmware,omitempty"`

	// Random delay added to every Redfish response that no override covers.
	// +optional
	ResponseDelay *DPUAgentResponseDelay `json:"responseDelay,omitempty"`

	// Per-operation delay ranges that replace responseDelay.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:XValidation:rule="self.all(a, self.exists_one(b, b.name == a.name && (!has(a.methods) || !has(b.methods) || '*' in a.methods || '*' in b.methods || a.methods.exists(m, m in b.methods))))",message="responseDelayOverrides must not cover the same operation and method twice"
	// +listType=atomic
	// +optional
	ResponseDelayOverrides []DPUAgentResponseDelayOverride `json:"responseDelayOverrides,omitempty"`
}

// DPUAgentFirmware sets the power-on firmware versions. On BF3 a bmc version
// below BF-24.10-17 makes DPF run the BMC upgrade; on BF4 DPF applies the
// PLDM bundle unless all four match it.
type DPUAgentFirmware struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	BMC *string `json:"bmc,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ERoT *string `json:"erot,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	UEFI *string `json:"uefi,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	NIC *string `json:"nic,omitempty"`
}

// DPUAgentResponseDelay is a closed range of whole seconds.
// +kubebuilder:validation:XValidation:rule="self.minSeconds <= self.maxSeconds",message="minSeconds must not exceed maxSeconds"
type DPUAgentResponseDelay struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=600
	// +required
	MinSeconds int `json:"minSeconds"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=600
	// +required
	MaxSeconds int `json:"maxSeconds"`
}

// DPUAgentResponseDelayOverride gives one Redfish operation its own delay range.
// +kubebuilder:validation:XValidation:rule="self.minSeconds <= self.maxSeconds",message="minSeconds must not exceed maxSeconds"
// +kubebuilder:validation:XValidation:rule="!has(self.methods) || self.methods.all(m, m == '*' || m in {'Account': ['PATCH'], 'AccountService': ['PATCH'], 'BMCRshim': ['GET', 'PATCH'], 'Bios': ['GET'], 'BiosSettings': ['PATCH'], 'CertificateService.GenerateCSR': ['POST'], 'CertificateService.ReplaceCertificate': ['POST'], 'Chassis': ['GET'], 'ComputerSystem.Reset': ['POST'], 'FirmwareInventory': ['GET'], 'HostPrivilegeConfig': ['PATCH'], 'HostRshim.Set': ['POST'], 'Manager': ['GET'], 'Manager.Reset': ['POST'], 'Manager.ResetToDefaults': ['POST'], 'Managers': ['GET'], 'Mode.Set': ['POST'], 'NetworkDeviceFunction': ['GET'], 'NvidiaChassis.Reset': ['POST'], 'SELEntries': ['GET'], 'SOC.ForceReset': ['POST'], 'SecureBoot': ['GET', 'PATCH'], 'ServerCertificate': ['GET'], 'ServiceRoot': ['GET'], 'System': ['GET'], 'SystemOem': ['GET'], 'SystemSettings': ['GET', 'PATCH'], 'Systems': ['GET'], 'Task': ['GET'], 'TruststoreCertificates': ['DELETE', 'GET', 'POST'], 'UpdateMultipart': ['POST'], 'UpdateService': ['GET', 'POST'], 'UpdateService.Activate': ['POST'], 'UpdateService.SimpleUpdate': ['POST'], 'VirtualMedia': ['GET', 'POST']}[self.name])",message="methods must be served by the named Redfish operation"
type DPUAgentResponseDelayOverride struct {
	// Operation name from the mock's route table, see MockRedfishOperations.
	// +kubebuilder:validation:Enum=Account;AccountService;BMCRshim;Bios;BiosSettings;CertificateService.GenerateCSR;CertificateService.ReplaceCertificate;Chassis;ComputerSystem.Reset;FirmwareInventory;HostPrivilegeConfig;HostRshim.Set;Manager;Manager.Reset;Manager.ResetToDefaults;Managers;Mode.Set;NetworkDeviceFunction;NvidiaChassis.Reset;SELEntries;SOC.ForceReset;SecureBoot;ServerCertificate;ServiceRoot;System;SystemOem;SystemSettings;Systems;Task;TruststoreCertificates;UpdateMultipart;UpdateService;UpdateService.Activate;UpdateService.SimpleUpdate;VirtualMedia
	// +required
	Name string `json:"name"`

	// HTTP methods the override covers. Unset or ["*"] covers every method
	// the operation serves.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:items:Enum=GET;PATCH;POST;DELETE;"*"
	// +kubebuilder:validation:XValidation:rule="!('*' in self) || size(self) == 1",message="\"*\" cannot be combined with other methods"
	// +listType=set
	// +optional
	Methods []string `json:"methods,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=600
	// +required
	MinSeconds int `json:"minSeconds"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=600
	// +required
	MaxSeconds int `json:"maxSeconds"`
}

// SimFleetSpec defines the desired state of SimFleet
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.dpuAgent) ? !has(self.dpuAgent) : self.dpuAgent == oldSelf.dpuAgent",message="spec.dpuAgent is immutable"
type SimFleetSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +required
	Replicas int `json:"replicas"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="devicesPerDPUNode is immutable"
	// +required
	DevicesPerDPUNode int `json:"devicesPerDPUNode"`

	// deviceSelectionLabels is the DPUSet selector the controller publishes
	// onto a DPUDevice once that device has a reachable BMC. It may not carry
	// a key from ReservedDeviceLabelKeys: those decide which fleet, ordinal,
	// and DPUNode a device belongs to, and the controller finds, adopts, and
	// cleans up its own inventory by them. Rejecting the selector
	// here is what tells the fleet author; the controller filters the same
	// keys anyway, so an object admitted before this rule still cannot
	// corrupt a device's identity.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k in ['scale.dpu.nvidia.com/fleet-name','scale.dpu.nvidia.com/fleet-uid','scale.dpu.nvidia.com/ordinal','provisioning.dpu.nvidia.com/dpunode-name'])",message="deviceSelectionLabels must not contain the controller-reserved keys scale.dpu.nvidia.com/fleet-name, scale.dpu.nvidia.com/fleet-uid, scale.dpu.nvidia.com/ordinal, or provisioning.dpu.nvidia.com/dpunode-name"
	// +required
	DeviceSelectionLabels map[string]string `json:"deviceSelectionLabels"`

	// +optional
	DPUNodeTemplate *DPUNodeTemplate `json:"dpuNodeTemplate,omitempty"`

	// +optional
	DPUDeviceTemplate *DPUDeviceTemplate `json:"dpuDeviceTemplate,omitempty"`

	// +optional
	VirtualMachineTemplate *VirtualMachineTemplate `json:"virtualMachineTemplate,omitempty"`

	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`

	// +optional
	DPUAgent *DPUAgentSpec `json:"dpuAgent,omitempty"`
}

// SimFleetStatus defines the observed state of SimFleet. The instance counts
// cover only ordinals below spec.replicas.
type SimFleetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AdmittedReplicas is the rollout index: SimInstances [0, admittedReplicas)
	// should exist.
	// +optional
	AdmittedReplicas int `json:"admittedReplicas,omitempty"`
	// CreatedInstances counts SimInstances whose children all exist
	// (ChildrenCreated), excluding Failed and deleting instances.
	// +optional
	CreatedInstances int `json:"createdInstances,omitempty"`
	// GuestReadyInstances counts SimInstances whose guest is ready
	// (GuestReady), excluding deleting instances.
	// +optional
	GuestReadyInstances int `json:"guestReadyInstances,omitempty"`
	// ProvisionedInstances counts SimInstances whose DPU has kubelet
	// configured (DPUProvisioned), excluding deleting instances.
	// +optional
	ProvisionedInstances int `json:"provisionedInstances,omitempty"`
	// FailedInstances counts SimInstances awaiting replacement.
	// +optional
	FailedInstances int `json:"failedInstances,omitempty"`
	// +optional
	DPUNodes int `json:"dpuNodes,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations=helm.sh/resource-policy=keep
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Admitted",type=integer,JSONPath=".status.admittedReplicas"
// +kubebuilder:printcolumn:name="Provisioned",type=integer,JSONPath=".status.provisionedInstances"
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=".status.failedInstances"
// +kubebuilder:printcolumn:name="Reconciled",type=string,JSONPath=".status.conditions[?(@.type=='InventoryReconciled')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SimFleet is the Schema for the simfleets API
type SimFleet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SimFleet
	// +required
	Spec SimFleetSpec `json:"spec"`

	// status defines the observed state of SimFleet
	// +optional
	Status SimFleetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SimFleetList contains a list of SimFleet
type SimFleetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SimFleet `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SimFleet{}, &SimFleetList{})
		return nil
	})
}
