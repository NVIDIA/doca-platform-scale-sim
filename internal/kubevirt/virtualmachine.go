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

// Package kubevirt renders and validates the KubeVirt VirtualMachine that
// hosts a simulated DPU, and reads the BMC address its guest reports.
package kubevirt

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// TemplateError names a VirtualMachine template this controller refuses to
// render. Reason is the condition reason status reports for it.
type TemplateError struct {
	Reason  string
	Message string
}

func (e *TemplateError) Error() string {
	return e.Message
}

// Fixed TemplateErrors ValidateTemplate returns; match them with errors.Is.
var (
	ErrUnsupportedStorage = &TemplateError{
		Reason:  "UnsupportedStorage",
		Message: "persistent VirtualMachine storage is not supported",
	}
	ErrUnsupportedNetwork = &TemplateError{
		Reason:  "UnsupportedNetwork",
		Message: "only bridge pod networking is supported",
	}
)

func newUnsupportedRunStrategyError(strategy string) error {
	return &TemplateError{
		Reason:  "UnsupportedRunStrategy",
		Message: fmt.Sprintf("run strategy %q is not supported", strategy),
	}
}

// ValidateTemplate refuses a template DesiredVirtualMachine cannot render.
// A nil template is valid: the fleet then boots no guests.
func ValidateTemplate(tmpl *scalev1alpha1.VirtualMachineTemplate) error {
	if tmpl == nil {
		return nil
	}
	if tmpl.Image == "" {
		return fmt.Errorf("image must not be empty")
	}
	if tmpl.Storage != nil && tmpl.Storage.Ephemeral != nil && !*tmpl.Storage.Ephemeral {
		return ErrUnsupportedStorage
	}
	if tmpl.PodNetwork != nil && tmpl.PodNetwork.Bridge != nil && !*tmpl.PodNetwork.Bridge {
		return ErrUnsupportedNetwork
	}

	switch tmpl.RunStrategy {
	case "",
		string(virtv1.RunStrategyAlways),
		string(virtv1.RunStrategyManual),
		string(virtv1.RunStrategyHalted),
		string(virtv1.RunStrategyOnce),
		string(virtv1.RunStrategyRerunOnFailure):
		return nil
	default:
		return newUnsupportedRunStrategyError(tmpl.RunStrategy)
	}
}

const (
	defaultVirtualMachineCPURequest    = "1"
	defaultVirtualMachineMemoryRequest = "1Gi"
	defaultVirtualMachineArchitecture  = "amd64"
	defaultCloudInitNetworkData        = `version: 2
ethernets:
  primary:
    match:
      name: "en*"
    dhcp4: true
`
)

func virtualMachineArchitecture(tmpl *scalev1alpha1.VirtualMachineTemplate) string {
	if tmpl != nil && tmpl.Architecture != "" {
		return tmpl.Architecture
	}
	return defaultVirtualMachineArchitecture
}

// virtualMachineRequests copies the template requests, preserving every
// resource the template names, and defaults CPU and memory when either is
// missing. KubeVirt's admission webhook rejects a VMI with no memory request,
// so SimFleet's optional resources depend on this default. The CPU default
// keeps guest sizing the same on every cluster instead of leaving it to
// KubeVirt's cluster-wide CPU allocation settings.
func virtualMachineRequests(resources *scalev1alpha1.VirtualMachineResources) corev1.ResourceList {
	requests := corev1.ResourceList{}
	if resources != nil {
		for name, quantity := range resources.Requests {
			requests[name] = quantity.DeepCopy()
		}
	}
	if _, ok := requests[corev1.ResourceCPU]; !ok {
		requests[corev1.ResourceCPU] = resource.MustParse(defaultVirtualMachineCPURequest)
	}
	if _, ok := requests[corev1.ResourceMemory]; !ok {
		requests[corev1.ResourceMemory] = resource.MustParse(defaultVirtualMachineMemoryRequest)
	}
	return requests
}

func virtualMachineLabels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	labels := map[string]string{}
	if fleet.Spec.VirtualMachineTemplate != nil {
		maps.Copy(labels, fleet.Spec.VirtualMachineTemplate.Metadata.Labels)
	}
	maps.Copy(labels, naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal)))
	return labels
}

// DesiredVirtualMachine renders the guest for ordinal. The fleet's template
// must be non-nil and have passed ValidateTemplate.
func DesiredVirtualMachine(fleet *scalev1alpha1.SimFleet, ordinal int) *virtv1.VirtualMachine {
	tmpl := fleet.Spec.VirtualMachineTemplate.DeepCopy()
	runStrategy := virtv1.VirtualMachineRunStrategy(tmpl.RunStrategy)
	if runStrategy == "" {
		runStrategy = virtv1.RunStrategyAlways
	}

	labels := virtualMachineLabels(fleet, ordinal)
	templateLabels := maps.Clone(labels)

	resources := virtv1.ResourceRequirements{Requests: virtualMachineRequests(tmpl.Resources)}

	return &virtv1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: virtv1.GroupVersion.String(),
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.VirtualMachineName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    labels,
		},
		Spec: virtv1.VirtualMachineSpec{
			RunStrategy: &runStrategy,
			Template: &virtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: templateLabels},
				Spec: virtv1.VirtualMachineInstanceSpec{
					Architecture: virtualMachineArchitecture(tmpl),
					Domain: virtv1.DomainSpec{
						Resources: resources,
						Devices: virtv1.Devices{
							Disks: []virtv1.Disk{
								{
									Name: "containerdisk",
									DiskDevice: virtv1.DiskDevice{
										Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio},
									},
								},
								{
									Name: "cloud-init",
									DiskDevice: virtv1.DiskDevice{
										Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio},
									},
								},
							},
							Interfaces: []virtv1.Interface{{
								Name: BridgedNetworkName,
								InterfaceBindingMethod: virtv1.InterfaceBindingMethod{
									Bridge: &virtv1.InterfaceBridge{},
								},
							}},
						},
					},
					NodeSelector: tmpl.NodeSelector,
					Tolerations:  tmpl.Tolerations,
					Networks: []virtv1.Network{{
						Name: BridgedNetworkName,
						NetworkSource: virtv1.NetworkSource{
							Pod: &virtv1.PodNetwork{},
						},
					}},
					Volumes: []virtv1.Volume{
						{
							Name: "containerdisk",
							VolumeSource: virtv1.VolumeSource{
								ContainerDisk: &virtv1.ContainerDiskSource{Image: tmpl.Image},
							},
						},
						// cloud-init is the only Secret a guest mounts. Its
						// payload carries the one credential the guest needs,
						// scoped to this ordinal's DPU and join Secret.
						{
							Name: "cloud-init",
							VolumeSource: virtv1.VolumeSource{
								CloudInitNoCloud: &virtv1.CloudInitNoCloudSource{
									UserDataSecretRef: &corev1.LocalObjectReference{
										Name: naming.CloudInitSecretName(fleet.Name, ordinal),
									},
									NetworkData: defaultCloudInitNetworkData,
								},
							},
						},
					},
				},
			},
		},
	}
}
