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

package pod

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

func podFleet(tmpl *scalev1alpha1.PodTemplate) *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:          1,
			DevicesPerDPUNode: 1,
			PodTemplate:       tmpl,
		},
	}
}

func agentDisabled(fleet *scalev1alpha1.SimFleet) *scalev1alpha1.SimFleet {
	fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(false)}
	return fleet
}

func volumeByName(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func mountAt(container corev1.Container, path string) *corev1.VolumeMount {
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].MountPath == path {
			return &container.VolumeMounts[i]
		}
	}
	return nil
}

var _ = Describe("ValidateTemplate", func() {
	It("accepts a nil template, because the fleet then boots no guests", func() {
		Expect(ValidateTemplate(nil)).To(Succeed())
	})

	It("accepts an image with no resources and with an empty resources object", func() {
		Expect(ValidateTemplate(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"})).To(Succeed())
		Expect(ValidateTemplate(&scalev1alpha1.PodTemplate{
			Image:     "example.com/node:v1",
			Resources: &corev1.ResourceRequirements{},
		})).To(Succeed())
	})

	It("rejects an empty image", func() {
		Expect(ValidateTemplate(&scalev1alpha1.PodTemplate{})).To(MatchError(ContainSubstring("image")))
	})
})

var _ = Describe("DesiredPod", func() {
	It("renders the identity, labels and scheduling from the template", func() {
		nodeSelector := map[string]string{"kubernetes.io/arch": "amd64"}
		tolerations := []corev1.Toleration{{Key: "dpu", Operator: corev1.TolerationOpExists}}
		fleet := podFleet(&scalev1alpha1.PodTemplate{
			Image: "example.com/node@sha256:deadbeef",
			Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{
				"example.com/template": "true",
				naming.LabelOrdinal:    "template-must-not-win",
				naming.LabelFleetName:  "template-must-not-win",
			}},
			NodeSelector: nodeSelector,
			Tolerations:  tolerations,
		})

		pod := DesiredPod(fleet, 7)

		Expect(pod.Name).To(Equal("scale-3000-dpu-000007"))
		Expect(pod.Namespace).To(Equal("ns"))
		Expect(pod.Labels).To(HaveKeyWithValue("example.com/template", "true"))
		Expect(pod.Labels).To(HaveKeyWithValue(naming.LabelOrdinal, "000007"))
		Expect(pod.Labels).To(HaveKeyWithValue(naming.LabelFleetName, "scale-3000"))
		Expect(pod.Labels).To(HaveKeyWithValue(naming.LabelFleetUID, "uid"))
		Expect(pod.Spec.NodeSelector).To(Equal(nodeSelector))
		Expect(pod.Spec.Tolerations).To(Equal(tolerations))
	})

	It("fixes the settings a template cannot override", func() {
		pod := DesiredPod(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"}), 0)

		Expect(pod.Spec.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.Spec.EnableServiceLinks).To(HaveValue(BeFalse()))
		Expect(pod.Spec.DNSPolicy).To(Equal(corev1.DNSClusterFirst))
		Expect(pod.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyAlways))
		Expect(pod.Spec.Containers).To(HaveLen(1))
		container := pod.Spec.Containers[0]
		Expect(container.Name).To(Equal("dpu"))
		Expect(container.Image).To(Equal("example.com/node:v1"))
		Expect(container.SecurityContext).NotTo(BeNil())
		Expect(container.SecurityContext.Privileged).To(HaveValue(BeTrue()))
	})

	It("probes TCP port 443 when the agent is enabled and not at all when it is disabled", func() {
		tmpl := &scalev1alpha1.PodTemplate{Image: "example.com/node:v1"}

		enabled := DesiredPod(podFleet(tmpl), 0).Spec.Containers[0]
		Expect(enabled.ReadinessProbe).NotTo(BeNil())
		Expect(enabled.ReadinessProbe.TCPSocket).NotTo(BeNil())
		Expect(enabled.ReadinessProbe.TCPSocket.Port).To(Equal(intstr.FromInt32(443)))
		Expect(enabled.ReadinessProbe.HTTPGet).To(BeNil())
		Expect(enabled.ReadinessProbe.Exec).To(BeNil())

		disabled := DesiredPod(agentDisabled(podFleet(tmpl)), 0).Spec.Containers[0]
		Expect(disabled.ReadinessProbe).To(BeNil())
	})

	It("mounts the runtime scratch space, kernel modules and the guest Secret", func() {
		pod := DesiredPod(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"}), 4)
		container := pod.Spec.Containers[0]

		for _, path := range []string{"/run", "/tmp"} {
			mount := mountAt(container, path)
			Expect(mount).NotTo(BeNil(), "mount %s", path)
			volume := volumeByName(pod, mount.Name)
			Expect(volume).NotTo(BeNil())
			Expect(volume.EmptyDir).NotTo(BeNil())
			Expect(volume.EmptyDir.Medium).To(Equal(corev1.StorageMediumMemory), "%s is memory-backed", path)
		}
		for _, path := range []string{"/var/lib/containerd", "/var/lib/kubelet"} {
			mount := mountAt(container, path)
			Expect(mount).NotTo(BeNil(), "mount %s", path)
			volume := volumeByName(pod, mount.Name)
			Expect(volume).NotTo(BeNil())
			Expect(volume.EmptyDir).NotTo(BeNil())
			Expect(volume.EmptyDir.Medium).To(BeEmpty(), "%s is disk-backed", path)
		}

		modules := mountAt(container, "/lib/modules")
		Expect(modules).NotTo(BeNil())
		Expect(modules.ReadOnly).To(BeTrue())
		hostPath := volumeByName(pod, modules.Name)
		Expect(hostPath).NotTo(BeNil())
		Expect(hostPath.HostPath).NotTo(BeNil())
		Expect(hostPath.HostPath.Path).To(Equal("/lib/modules"))

		secretName := naming.GuestSecretName("scale-3000", 4)
		sim := mountAt(container, "/etc/dpf-sim")
		Expect(sim).NotTo(BeNil())
		Expect(sim.ReadOnly).To(BeTrue())
		simVolume := volumeByName(pod, sim.Name)
		Expect(simVolume.Secret).NotTo(BeNil())
		Expect(simVolume.Secret.SecretName).To(Equal(secretName))
		Expect(simVolume.Secret.DefaultMode).To(HaveValue(Equal(int32(0o600))))
		Expect(simVolume.Secret.Items).To(Equal([]corev1.KeyToPath{
			{Key: "identity.env", Path: "identity.env"},
			{Key: "mgmt.kubeconfig", Path: "mgmt.kubeconfig"},
		}))

		agent := mountAt(container, "/etc/mock-dpuagent")
		Expect(agent).NotTo(BeNil())
		Expect(agent.ReadOnly).To(BeTrue())
		agentVolume := volumeByName(pod, agent.Name)
		Expect(agentVolume.Secret).NotTo(BeNil())
		Expect(agentVolume.Secret.SecretName).To(Equal(secretName))
		Expect(agentVolume.Secret.DefaultMode).To(HaveValue(Equal(int32(0o600))))
		Expect(agentVolume.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "config.yaml", Path: "config.yaml"}}))
		Expect(agent.Name).NotTo(Equal(sim.Name))
	})

	It("mounts only identity.env when the agent is disabled", func() {
		pod := DesiredPod(agentDisabled(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"})), 0)
		container := pod.Spec.Containers[0]

		sim := mountAt(container, "/etc/dpf-sim")
		Expect(sim).NotTo(BeNil())
		simVolume := volumeByName(pod, sim.Name)
		Expect(simVolume.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "identity.env", Path: "identity.env"}}))
		Expect(mountAt(container, "/etc/mock-dpuagent")).To(BeNil())
		for _, volume := range pod.Spec.Volumes {
			if volume.Secret != nil {
				Expect(volume.Name).To(Equal(sim.Name), "only one Secret volume")
			}
		}
	})

	It("defaults a missing cpu and memory request and passes everything else through", func() {
		resources := &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("2"),
				corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
		}
		pod := DesiredPod(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1", Resources: resources}), 0)

		got := pod.Spec.Containers[0].Resources
		Expect(got.Requests.Cpu().String()).To(Equal("2"))
		Expect(got.Requests.Memory().String()).To(Equal("256Mi"))
		Expect(got.Requests.StorageEphemeral().String()).To(Equal("1Gi"))
		Expect(got.Limits).To(HaveLen(1))
		Expect(got.Limits.Memory().String()).To(Equal("4Gi"))
		Expect(resources.Requests).To(HaveLen(2), "the template is not modified")
	})

	It("defaults both requests for a nil and for an empty resources object, with no limits", func() {
		for _, resources := range []*corev1.ResourceRequirements{nil, {}} {
			pod := DesiredPod(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1", Resources: resources}), 0)
			got := pod.Spec.Containers[0].Resources
			Expect(got.Requests.Cpu().String()).To(Equal("100m"))
			Expect(got.Requests.Memory().String()).To(Equal("256Mi"))
			Expect(got.Limits).To(BeEmpty())
		}
	})
})

var _ = Describe("PodIPv4", func() {
	podWithIPs := func(ips ...string) *corev1.Pod {
		pod := &corev1.Pod{}
		for _, ip := range ips {
			pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
		}
		return pod
	}

	It("returns the first IPv4 address", func() {
		Expect(PodIPv4(podWithIPs("10.244.1.5", "10.244.1.6"))).To(Equal("10.244.1.5"))
	})

	It("skips IPv6 addresses ahead of the IPv4", func() {
		Expect(PodIPv4(podWithIPs("fd00::5", "10.244.1.5"))).To(Equal("10.244.1.5"))
	})

	It("returns empty without an IPv4, with an invalid address, and for a nil Pod", func() {
		Expect(PodIPv4(podWithIPs())).To(BeEmpty())
		Expect(PodIPv4(podWithIPs("fd00::5"))).To(BeEmpty())
		Expect(PodIPv4(podWithIPs("not-an-ip"))).To(BeEmpty())
		Expect(PodIPv4(nil)).To(BeEmpty())
	})

	It("reads status.podIPs only, not the legacy status.podIP", func() {
		pod := &corev1.Pod{Status: corev1.PodStatus{PodIP: "10.244.1.9"}}
		Expect(PodIPv4(pod)).To(BeEmpty())
	})
})

var _ = Describe("Ready", func() {
	conditions := func(conditions ...corev1.PodCondition) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: conditions}}
	}

	It("is true only for a true Ready condition", func() {
		Expect(Ready(conditions(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}))).To(BeTrue())
		Expect(Ready(conditions(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}))).To(BeFalse())
		Expect(Ready(conditions(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}))).To(BeFalse())
		Expect(Ready(conditions())).To(BeFalse())
		Expect(Ready(nil)).To(BeFalse())
	})
})

var _ = Describe("DesiredGuestSecret", func() {
	It("holds the three guest files under their file names for an enabled guest", func() {
		fleet := podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"})

		secret, err := DesiredGuestSecret(fleet, 12, []byte("kubeconfig-body"))
		Expect(err).NotTo(HaveOccurred())

		Expect(secret.Name).To(Equal("scale-3000-dpu-000012-guest"))
		Expect(secret.Namespace).To(Equal("ns"))
		Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
		Expect(secret.Labels).To(Equal(naming.InventoryLabels(fleet, "000012")))
		Expect(secret.Data).To(HaveLen(3))
		Expect(string(secret.Data["identity.env"])).To(ContainSubstring("DPU_DEVICE_NAME=scale-3000-device-000012\n"))
		Expect(string(secret.Data["config.yaml"])).NotTo(BeEmpty())
		Expect(secret.Data["mgmt.kubeconfig"]).To(Equal([]byte("kubeconfig-body")))
	})

	It("holds only identity.env when the agent is disabled", func() {
		fleet := agentDisabled(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"}))

		secret, err := DesiredGuestSecret(fleet, 0, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(secret.Data).To(HaveLen(1))
		Expect(secret.Data).To(HaveKey("identity.env"))
	})

	It("rejects an empty management kubeconfig for an enabled guest", func() {
		_, err := DesiredGuestSecret(podFleet(&scalev1alpha1.PodTemplate{Image: "example.com/node:v1"}), 0, nil)
		Expect(err).To(HaveOccurred())
	})
})
