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

// Package pod renders and validates the privileged Pod that runs a simulated
// DPU when a fleet uses podTemplate instead of KubeVirt, and reads the
// address that Pod reports.
package pod

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/cloudinit"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

const (
	// ContainerName is the one container of a guest Pod.
	ContainerName = "dpu"

	// ReadinessPort is the port the DPU agent serves its BMC on. The guest is
	// Ready once it accepts connections there.
	ReadinessPort = 443

	defaultCPURequest    = "100m"
	defaultMemoryRequest = "256Mi"

	secretMode = int32(0o600)

	identityDir = "/etc/dpf-sim"
	agentDir    = "/etc/mock-dpuagent"
)

// ValidateTemplate refuses a template DesiredPod cannot render. A nil
// template is valid: the fleet then boots no guests. An empty resources
// object is valid too; DesiredPod fills in the missing requests.
func ValidateTemplate(tmpl *scalev1alpha1.PodTemplate) error {
	if tmpl == nil {
		return nil
	}
	if tmpl.Image == "" {
		return errors.New("image must not be empty")
	}
	return nil
}

// requests copies the template requests and defaults CPU and memory when
// either is missing, so every guest is sized the same on every cluster.
func requests(resources *corev1.ResourceRequirements) corev1.ResourceList {
	requests := corev1.ResourceList{}
	if resources != nil {
		for name, quantity := range resources.Requests {
			requests[name] = quantity.DeepCopy()
		}
	}
	if _, ok := requests[corev1.ResourceCPU]; !ok {
		requests[corev1.ResourceCPU] = resource.MustParse(defaultCPURequest)
	}
	if _, ok := requests[corev1.ResourceMemory]; !ok {
		requests[corev1.ResourceMemory] = resource.MustParse(defaultMemoryRequest)
	}
	return requests
}

func labels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	labels := map[string]string{}
	if fleet.Spec.PodTemplate != nil {
		maps.Copy(labels, fleet.Spec.PodTemplate.Metadata.Labels)
	}
	maps.Copy(labels, naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal)))
	return labels
}

// DesiredPod renders the guest for ordinal. The fleet's template must be
// non-nil and have passed ValidateTemplate. The template cannot change the
// privileges, the mounts, the service account token, DNS, the restart policy
// or the readiness probe.
func DesiredPod(fleet *scalev1alpha1.SimFleet, ordinal int) *corev1.Pod {
	tmpl := fleet.Spec.PodTemplate.DeepCopy()

	resources := corev1.ResourceRequirements{Requests: requests(tmpl.Resources)}
	if tmpl.Resources != nil {
		resources.Limits = tmpl.Resources.Limits
		resources.Claims = tmpl.Resources.Claims
	}

	identityItems := []corev1.KeyToPath{{Key: cloudinit.IdentityFileName, Path: cloudinit.IdentityFileName}}
	mounts := []corev1.VolumeMount{
		{Name: "run", MountPath: "/run"},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "containerd", MountPath: "/var/lib/containerd"},
		{Name: "kubelet", MountPath: "/var/lib/kubelet"},
		{Name: "lib-modules", MountPath: "/lib/modules", ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "run", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		}},
		{Name: "containerd", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "kubelet", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "lib-modules", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"},
		}},
	}

	// Kubernetes items belong to the volume, so the two directories the guest
	// reads need two volumes of the one Secret.
	var probe *corev1.Probe
	agentEnabled := dpuagent.Enabled(fleet)
	if agentEnabled {
		identityItems = append(identityItems,
			corev1.KeyToPath{Key: cloudinit.KubeconfigFileName, Path: cloudinit.KubeconfigFileName})
		probe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(ReadinessPort)},
		}}
	}
	secretName := naming.GuestSecretName(fleet.Name, ordinal)
	mounts = append(mounts, corev1.VolumeMount{Name: "guest-dpf-sim", MountPath: identityDir, ReadOnly: true})
	volumes = append(volumes, secretVolume("guest-dpf-sim", secretName, identityItems))
	if agentEnabled {
		mounts = append(mounts, corev1.VolumeMount{Name: "guest-mock-dpuagent", MountPath: agentDir, ReadOnly: true})
		volumes = append(volumes, secretVolume("guest-mock-dpuagent", secretName,
			[]corev1.KeyToPath{{Key: cloudinit.ConfigFileName, Path: cloudinit.ConfigFileName}}))
	}

	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.GuestName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    labels(fleet, ordinal),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			DNSPolicy:                    corev1.DNSClusterFirst,
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			NodeSelector:                 tmpl.NodeSelector,
			Tolerations:                  tmpl.Tolerations,
			Containers: []corev1.Container{{
				Name:            ContainerName,
				Image:           tmpl.Image,
				SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
				Resources:       resources,
				ReadinessProbe:  probe,
				VolumeMounts:    mounts,
			}},
			Volumes: volumes,
		},
	}
}

func secretVolume(name, secretName string, items []corev1.KeyToPath) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName:  secretName,
		Items:       items,
		DefaultMode: ptr.To(secretMode),
	}}}
}

// DesiredGuestSecret renders the create-only Secret a guest Pod reads its
// files from, one key per file. It is the durable record that this ordinal
// already holds a credential. It is pure: callers resolve and validate every
// dependency first.
func DesiredGuestSecret(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	managementKubeconfig []byte,
) (*corev1.Secret, error) {
	files, err := cloudinit.GuestFiles(fleet, ordinal, managementKubeconfig)
	if err != nil {
		return nil, err
	}
	data := make(map[string][]byte, len(files))
	for _, file := range files {
		data[file.Name] = []byte(file.Content)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.GuestSecretName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    GuestSecretLabels(fleet, ordinal),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}, nil
}

// GuestSecretLabels are the labels the guest Secret for ordinal carries.
func GuestSecretLabels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	return naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal))
}

// PodIPv4 is the first IPv4 address in the Pod's status.podIPs, or "" while
// it has none. It is the address DPF opens Redfish against.
func PodIPv4(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, podIP := range pod.Status.PodIPs {
		address, err := netip.ParseAddr(podIP.IP)
		if err == nil && address.Is4() {
			return podIP.IP
		}
	}
	return ""
}

// Ready reports whether the Pod's Ready condition is true.
func Ready(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
