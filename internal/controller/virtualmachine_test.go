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
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	virtv1 "kubevirt.io/api/core/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

func TestDesiredVirtualMachineShape(t *testing.T) {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	nodeSelector := map[string]string{"kubernetes.io/arch": "amd64"}
	tolerations := []corev1.Toleration{{
		Key:      "virtualization",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000",
			Namespace: "ns",
			UID:       types.UID("uid"),
		},
		Spec: scalev1alpha1.SimFleetSpec{
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
				Image: "example.com/node@sha256:deadbeef",
				Resources: &scalev1alpha1.VirtualMachineResources{
					Requests: requests,
				},
				Storage: &scalev1alpha1.VirtualMachineStorage{
					RootDiskSize: "100Gi",
				},
				Metadata: scalev1alpha1.TemplateMetadata{
					Labels: map[string]string{
						"example.com/template": "true",
						labelOrdinal:           "template-must-not-win",
					},
				},
				NodeSelector: nodeSelector,
				Tolerations:  tolerations,
			},
		},
	}

	vm := desiredVirtualMachine(fleet, 0)

	if vm.Name != "scale-3000-dpu-000000" || vm.Namespace != "ns" {
		t.Fatalf("VM identity = %s/%s", vm.Namespace, vm.Name)
	}
	if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyAlways {
		t.Fatalf("runStrategy = %v, want Always", vm.Spec.RunStrategy)
	}
	wantLabels := map[string]string{
		"example.com/template": "true",
		labelFleetName:         "scale-3000",
		labelFleetUID:          "uid",
		labelOrdinal:           "000000",
	}
	if !reflect.DeepEqual(vm.Labels, wantLabels) {
		t.Fatalf("VM labels = %#v, want %#v", vm.Labels, wantLabels)
	}
	if vm.Spec.Template == nil || !reflect.DeepEqual(vm.Spec.Template.ObjectMeta.Labels, wantLabels) {
		t.Fatalf("VMI template labels = %#v, want %#v", vm.Spec.Template.ObjectMeta.Labels, wantLabels)
	}

	spec := vm.Spec.Template.Spec
	if spec.Architecture != defaultVirtualMachineArchitecture {
		t.Fatalf("architecture = %q, want %s", spec.Architecture, defaultVirtualMachineArchitecture)
	}
	if !reflect.DeepEqual(spec.Domain.Resources.Requests, requests) {
		t.Fatalf("resource requests = %#v, want %#v", spec.Domain.Resources.Requests, requests)
	}
	if !reflect.DeepEqual(spec.NodeSelector, nodeSelector) {
		t.Fatalf("nodeSelector = %#v, want %#v", spec.NodeSelector, nodeSelector)
	}
	if !reflect.DeepEqual(spec.Tolerations, tolerations) {
		t.Fatalf("tolerations = %#v, want %#v", spec.Tolerations, tolerations)
	}

	// Two disks, not three. The guest reads DPF's per-DPU join Secret with
	// the credential in its cloud-init payload, so there is no fleet-wide
	// join material left to hand it on a disk.
	disks := spec.Domain.Devices.Disks
	if len(disks) != 2 {
		t.Fatalf("disks = %#v, want two", disks)
	}
	for i, name := range []string{"containerdisk", "cloud-init"} {
		if disks[i].Name != name || disks[i].Disk == nil || disks[i].Disk.Bus != virtv1.DiskBusVirtio {
			t.Fatalf("disk %d = %#v, want %q virtio", i, disks[i], name)
		}
	}

	if len(spec.Domain.Devices.Interfaces) != 1 ||
		spec.Domain.Devices.Interfaces[0].Name != "default" ||
		spec.Domain.Devices.Interfaces[0].Bridge == nil {
		t.Fatalf("interfaces = %#v, want default bridge", spec.Domain.Devices.Interfaces)
	}
	if len(spec.Networks) != 1 || spec.Networks[0].Name != "default" || spec.Networks[0].Pod == nil {
		t.Fatalf("networks = %#v, want default pod network", spec.Networks)
	}

	if len(spec.Volumes) != 2 {
		t.Fatalf("volumes = %#v, want two", spec.Volumes)
	}
	if volume := spec.Volumes[0]; volume.Name != "containerdisk" ||
		volume.ContainerDisk == nil || volume.ContainerDisk.Image != fleet.Spec.VirtualMachineTemplate.Image {
		t.Fatalf("containerdisk volume = %#v", volume)
	}
	if volume := spec.Volumes[1]; volume.Name != "cloud-init" ||
		volume.CloudInitNoCloud == nil || volume.CloudInitNoCloud.UserDataSecretRef == nil ||
		volume.CloudInitNoCloud.UserDataSecretRef.Name != "scale-3000-dpu-000000-cloud-init" ||
		volume.CloudInitNoCloud.NetworkData != defaultCloudInitNetworkData {
		t.Fatalf("cloud-init volume = %#v", volume)
	}
	// cloud-init is the only Secret-backed volume a guest gets.
	for _, volume := range spec.Volumes {
		if volume.Secret != nil {
			t.Fatalf("VirtualMachine mounts Secret volume %#v", volume)
		}
	}
	volumesJSON, err := json.Marshal(spec.Volumes)
	if err != nil {
		t.Fatalf("marshal volumes: %v", err)
	}
	if strings.Contains(string(volumesJSON), "rootDiskSize") ||
		strings.Contains(string(volumesJSON), "100Gi") {
		t.Fatalf("rootDiskSize leaked into volumes: %s", volumesJSON)
	}
}

func TestDesiredVirtualMachineArchitecture(t *testing.T) {
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
		Spec: scalev1alpha1.SimFleetSpec{
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
				Image:        "example.com/node@sha256:deadbeef",
				Architecture: "arm64",
			},
		},
	}

	got := desiredVirtualMachine(fleet, 0).Spec.Template.Spec.Architecture
	if got != "arm64" {
		t.Fatalf("architecture = %q, want arm64", got)
	}
}

// KubeVirt v1.9 rejects a VMI with no memory, so an omitted or partial
// resources block must still produce both CPU and memory requests.
func TestDesiredVirtualMachineDefaultsResourceRequests(t *testing.T) {
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
		Spec: scalev1alpha1.SimFleetSpec{
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
				Image: "example.com/node@sha256:deadbeef",
			},
		},
	}

	requests := desiredVirtualMachine(fleet, 0).Spec.Template.Spec.Domain.Resources.Requests

	memory := requests[corev1.ResourceMemory]
	if memory.IsZero() {
		t.Fatalf("memory request = %s, want the 1Gi default", memory.String())
	}
	if want := resource.MustParse("1Gi"); memory.Cmp(want) != 0 {
		t.Fatalf("memory request = %s, want 1Gi", memory.String())
	}
	cpu := requests[corev1.ResourceCPU]
	if want := resource.MustParse("1"); cpu.Cmp(want) != 0 {
		t.Fatalf("cpu request = %s, want 1", cpu.String())
	}
}

func TestDesiredVirtualMachineOverlaysPartialResourceRequests(t *testing.T) {
	supplied := corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("8"),
		corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
	}
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
		Spec: scalev1alpha1.SimFleetSpec{
			VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
				Image:     "example.com/node@sha256:deadbeef",
				Resources: &scalev1alpha1.VirtualMachineResources{Requests: supplied},
			},
		},
	}

	requests := desiredVirtualMachine(fleet, 0).Spec.Template.Spec.Domain.Resources.Requests

	if cpu := requests[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("8")) != 0 {
		t.Fatalf("cpu request = %s, want the supplied 8", cpu.String())
	}
	if memory := requests[corev1.ResourceMemory]; memory.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("memory request = %s, want the 1Gi default", memory.String())
	}
	if storage := requests[corev1.ResourceEphemeralStorage]; storage.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("ephemeral-storage request = %s, want the supplied 2Gi", storage.String())
	}

	if len(supplied) != 2 {
		t.Fatalf("template requests = %#v, want the input map left untouched", supplied)
	}
	if _, ok := supplied[corev1.ResourceMemory]; ok {
		t.Fatalf("template requests = %#v, want no defaulted memory in the input map", supplied)
	}
	if &requests == &fleet.Spec.VirtualMachineTemplate.Resources.Requests {
		t.Fatal("VM requests must not alias the SimFleet spec map")
	}
	fleetRequests := fleet.Spec.VirtualMachineTemplate.Resources.Requests
	if len(fleetRequests) != 2 {
		t.Fatalf("SimFleet spec requests = %#v, want two entries", fleetRequests)
	}
}

func TestValidateVirtualMachineTemplate(t *testing.T) {
	falseValue := false
	tests := []struct {
		name       string
		template   *scalev1alpha1.VirtualMachineTemplate
		wantReason string
		wantMsg    string
	}{
		{name: "nil template", template: nil},
		{
			name:     "Always run strategy",
			template: &scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyAlways)},
		},
		{
			name:     "Manual run strategy",
			template: &scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyManual)},
		},
		{
			name:     "Halted run strategy",
			template: &scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyHalted)},
		},
		{
			name:     "Once run strategy",
			template: &scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyOnce)},
		},
		{
			name:     "RerunOnFailure run strategy",
			template: &scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyRerunOnFailure)},
		},
		{
			name:       "empty image",
			template:   &scalev1alpha1.VirtualMachineTemplate{},
			wantReason: "ReconcileError",
			wantMsg:    "image must not be empty",
		},
		{
			name: "persistent storage",
			template: &scalev1alpha1.VirtualMachineTemplate{
				Image:   "img",
				Storage: &scalev1alpha1.VirtualMachineStorage{Ephemeral: &falseValue},
			},
			wantReason: "UnsupportedStorage",
		},
		{
			name: "disabled bridge network",
			template: &scalev1alpha1.VirtualMachineTemplate{
				Image:      "img",
				PodNetwork: &scalev1alpha1.PodNetwork{Bridge: &falseValue},
			},
			wantReason: "UnsupportedNetwork",
		},
		{
			name: "unknown run strategy",
			template: &scalev1alpha1.VirtualMachineTemplate{
				Image:       "img",
				RunStrategy: "foo",
			},
			wantReason: "UnsupportedRunStrategy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateVirtualMachineTemplate(test.template)
			if test.wantReason == "" && test.wantMsg == "" {
				if err != nil {
					t.Fatalf("validateVirtualMachineTemplate() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateVirtualMachineTemplate() error = nil, want error")
			}
			if got := conditionReason(err); got != test.wantReason {
				t.Fatalf("conditionReason() = %q, want %q", got, test.wantReason)
			}
			if test.wantReason != "ReconcileError" {
				var condErr *virtualMachineConditionError
				if !errors.As(err, &condErr) {
					t.Fatalf("validateVirtualMachineTemplate() error = %T, want *virtualMachineConditionError", err)
				}
				if condErr.reason != test.wantReason {
					t.Fatalf("condition reason = %q, want %q", condErr.reason, test.wantReason)
				}
			}
			if test.wantMsg != "" && !strings.Contains(err.Error(), test.wantMsg) {
				t.Fatalf("validateVirtualMachineTemplate() message = %q, want substring %q", err.Error(), test.wantMsg)
			}
		})
	}
}
