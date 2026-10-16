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

package kubevirt

import (
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

var _ = Describe("VirtualMachine", func() {
	Describe("DesiredVirtualMachine", func() {
		It("renders the identity, labels, scheduling, devices and volumes from the template", func() {
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
								naming.LabelOrdinal:    "template-must-not-win",
							},
						},
						NodeSelector: nodeSelector,
						Tolerations:  tolerations,
					},
				},
			}

			vm := DesiredVirtualMachine(fleet, 0)

			Expect(vm.Name).To(Equal("scale-3000-dpu-000000"))
			Expect(vm.Namespace).To(Equal("ns"))
			Expect(vm.Spec.RunStrategy).To(HaveValue(Equal(virtv1.RunStrategyAlways)))
			wantLabels := map[string]string{
				"example.com/template": "true",
				naming.LabelFleetName:  "scale-3000",
				naming.LabelFleetUID:   "uid",
				naming.LabelOrdinal:    "000000",
			}
			Expect(vm.Labels).To(Equal(wantLabels), "VM labels")
			Expect(vm.Spec.Template).NotTo(BeNil())
			Expect(vm.Spec.Template.ObjectMeta.Labels).To(Equal(wantLabels), "VMI template labels")

			spec := vm.Spec.Template.Spec
			Expect(spec.Architecture).To(Equal(defaultVirtualMachineArchitecture))
			Expect(spec.Domain.Resources.Requests).To(Equal(requests))
			Expect(spec.NodeSelector).To(Equal(nodeSelector))
			Expect(spec.Tolerations).To(Equal(tolerations))

			// The guest gets only its boot image and its cloud-init payload. Join
			// material reaches it through the per-DPU credential in cloud-init,
			// never on a disk shared across the fleet.
			disks := spec.Domain.Devices.Disks
			Expect(disks).To(HaveLen(2))
			for i, name := range []string{"containerdisk", "cloud-init"} {
				Expect(disks[i].Name).To(Equal(name), "disk %d", i)
				Expect(disks[i].Disk).NotTo(BeNil(), "disk %d", i)
				Expect(disks[i].Disk.Bus).To(Equal(virtv1.DiskBusVirtio), "disk %d", i)
			}

			Expect(spec.Domain.Devices.Interfaces).To(HaveLen(1))
			Expect(spec.Domain.Devices.Interfaces[0].Name).To(Equal("default"))
			Expect(spec.Domain.Devices.Interfaces[0].Bridge).NotTo(BeNil(), "default interface is not a bridge")
			Expect(spec.Networks).To(HaveLen(1))
			Expect(spec.Networks[0].Name).To(Equal("default"))
			Expect(spec.Networks[0].Pod).NotTo(BeNil(), "default network is not the pod network")

			Expect(spec.Volumes).To(HaveLen(2))
			containerDisk := spec.Volumes[0]
			Expect(containerDisk.Name).To(Equal("containerdisk"))
			Expect(containerDisk.ContainerDisk).NotTo(BeNil())
			Expect(containerDisk.ContainerDisk.Image).To(Equal(fleet.Spec.VirtualMachineTemplate.Image))
			cloudInit := spec.Volumes[1]
			Expect(cloudInit.Name).To(Equal("cloud-init"))
			Expect(cloudInit.CloudInitNoCloud).NotTo(BeNil())
			Expect(cloudInit.CloudInitNoCloud.UserDataSecretRef).NotTo(BeNil())
			Expect(cloudInit.CloudInitNoCloud.UserDataSecretRef.Name).To(Equal("scale-3000-dpu-000000-cloud-init"))
			Expect(cloudInit.CloudInitNoCloud.NetworkData).To(Equal(defaultCloudInitNetworkData))
			// cloud-init is the only Secret-backed volume a guest gets.
			for _, volume := range spec.Volumes {
				Expect(volume.Secret).To(BeNil(), "VirtualMachine mounts Secret volume %q", volume.Name)
			}
			volumesJSON, err := json.Marshal(spec.Volumes)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(volumesJSON)).NotTo(ContainSubstring("rootDiskSize"), "rootDiskSize leaked into volumes")
			Expect(string(volumesJSON)).NotTo(ContainSubstring("100Gi"), "rootDiskSize leaked into volumes")
		})

		It("uses the template architecture", func() {
			fleet := &scalev1alpha1.SimFleet{
				ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
				Spec: scalev1alpha1.SimFleetSpec{
					VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
						Image:        "example.com/node@sha256:deadbeef",
						Architecture: "arm64",
					},
				},
			}

			Expect(DesiredVirtualMachine(fleet, 0).Spec.Template.Spec.Architecture).To(Equal("arm64"))
		})

		// KubeVirt v1.9 rejects a VMI with no memory, so an omitted or partial
		// resources block must still produce both CPU and memory requests.
		It("defaults CPU and memory requests when resources are omitted", func() {
			fleet := &scalev1alpha1.SimFleet{
				ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
				Spec: scalev1alpha1.SimFleetSpec{
					VirtualMachineTemplate: &scalev1alpha1.VirtualMachineTemplate{
						Image: "example.com/node@sha256:deadbeef",
					},
				},
			}

			requests := DesiredVirtualMachine(fleet, 0).Spec.Template.Spec.Domain.Resources.Requests

			memory := requests[corev1.ResourceMemory]
			Expect(memory.IsZero()).To(BeFalse(), "memory request = %s, want the 1Gi default", memory.String())
			Expect(memory.Cmp(resource.MustParse("1Gi"))).To(BeZero(), "memory request = %s, want 1Gi", memory.String())
			cpu := requests[corev1.ResourceCPU]
			Expect(cpu.Cmp(resource.MustParse("1"))).To(BeZero(), "cpu request = %s, want 1", cpu.String())
		})

		It("overlays partial resource requests without mutating the SimFleet spec", func() {
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

			requests := DesiredVirtualMachine(fleet, 0).Spec.Template.Spec.Domain.Resources.Requests

			cpu := requests[corev1.ResourceCPU]
			Expect(cpu.Cmp(resource.MustParse("8"))).To(BeZero(), "cpu request = %s, want the supplied 8", cpu.String())
			memory := requests[corev1.ResourceMemory]
			Expect(memory.Cmp(resource.MustParse("1Gi"))).To(BeZero(), "memory request = %s, want the 1Gi default", memory.String())
			storage := requests[corev1.ResourceEphemeralStorage]
			Expect(storage.Cmp(resource.MustParse("2Gi"))).To(BeZero(),
				"ephemeral-storage request = %s, want the supplied 2Gi", storage.String())

			Expect(supplied).To(HaveLen(2), "want the input map left untouched")
			Expect(supplied).NotTo(HaveKey(corev1.ResourceMemory), "want no defaulted memory in the input map")
			Expect(&requests).NotTo(BeIdenticalTo(&fleet.Spec.VirtualMachineTemplate.Resources.Requests),
				"VM requests must not alias the SimFleet spec map")
			Expect(fleet.Spec.VirtualMachineTemplate.Resources.Requests).To(HaveLen(2), "SimFleet spec requests")
		})
	})

	Describe("ValidateTemplate", func() {
		DescribeTable("accepts supported templates and rejects the rest",
			func(template *scalev1alpha1.VirtualMachineTemplate, wantReason, wantMsg string) {
				err := ValidateTemplate(template)
				if wantReason == "" && wantMsg == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(HaveOccurred())
				var condErr *TemplateError
				if wantReason == "" {
					Expect(errors.As(err, &condErr)).To(BeFalse(), "error = %T, want a plain error", err)
				} else {
					Expect(errors.As(err, &condErr)).To(BeTrue(), "error = %T, want *TemplateError", err)
					Expect(condErr.Reason).To(Equal(wantReason))
				}
				if wantMsg != "" {
					Expect(err).To(MatchError(ContainSubstring(wantMsg)))
				}
			},
			Entry("nil template", nil, "", ""),
			Entry("Always run strategy",
				&scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyAlways)}, "", ""),
			Entry("Manual run strategy",
				&scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyManual)}, "", ""),
			Entry("Halted run strategy",
				&scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyHalted)}, "", ""),
			Entry("Once run strategy",
				&scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyOnce)}, "", ""),
			Entry("RerunOnFailure run strategy",
				&scalev1alpha1.VirtualMachineTemplate{Image: "img", RunStrategy: string(virtv1.RunStrategyRerunOnFailure)}, "", ""),
			Entry("empty image", &scalev1alpha1.VirtualMachineTemplate{}, "", "image must not be empty"),
			Entry("persistent storage", &scalev1alpha1.VirtualMachineTemplate{
				Image:   "img",
				Storage: &scalev1alpha1.VirtualMachineStorage{Ephemeral: ptr.To(false)},
			}, "UnsupportedStorage", ""),
			Entry("disabled bridge network", &scalev1alpha1.VirtualMachineTemplate{
				Image:      "img",
				PodNetwork: &scalev1alpha1.PodNetwork{Bridge: ptr.To(false)},
			}, "UnsupportedNetwork", ""),
			Entry("unknown run strategy", &scalev1alpha1.VirtualMachineTemplate{
				Image:       "img",
				RunStrategy: "foo",
			}, "UnsupportedRunStrategy", ""),
		)
	})
})
