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

// Package cloudinit renders the create-only cloud-init Secret a simulated
// DPU's guest boots with.
package cloudinit

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

const (
	maxCloudInitUserdataBytes = 16 * 1024
	dpuIdentityPath           = "/etc/dpf-sim/identity.env"
	// Debug console/SSH credentials for simulated guests. Cloud-init unlocks the Ubuntu image's ubuntu user.
	cloudInitDebugUser     = "ubuntu"
	cloudInitDebugPassword = "ubuntu"
)

// SecretLabels are the labels the cloud-init Secret for ordinal carries.
func SecretLabels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	return naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal))
}

// DesiredSecret renders a complete, create-only cloud-init payload.
// It is pure: callers must resolve and validate all dependencies first.
func DesiredSecret(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	managementKubeconfig []byte,
) (*corev1.Secret, error) {
	userdata, err := cloudInitUserdata(fleet, ordinal, managementKubeconfig)
	if err != nil {
		return nil, err
	}
	return newCloudInitSecret(fleet, ordinal, userdata)
}

func newCloudInitSecret(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	userdata string,
) (*corev1.Secret, error) {
	if len(userdata) >= maxCloudInitUserdataBytes {
		return nil, fmt.Errorf(
			"cloud-init userdata for ordinal %d exceeds the %d-byte limit",
			ordinal,
			maxCloudInitUserdataBytes,
		)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.CloudInitSecretName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    SecretLabels(fleet, ordinal),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"userdata": []byte(userdata),
		},
	}, nil
}

func cloudInitUserdata(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	managementKubeconfig []byte,
) (string, error) {
	if fleet == nil {
		return "", errors.New("SimFleet is required to render cloud-init userdata")
	}

	files, err := GuestFiles(fleet, ordinal, managementKubeconfig)
	if err != nil {
		return "", err
	}
	var writeFiles strings.Builder
	for _, file := range files {
		indicator, indented := yamlLiteral(file.Content, "      ")
		fmt.Fprintf(&writeFiles, `  - path: %s
    permissions: "0600"
    content: %s
%s`, file.Path, indicator, indented)
	}
	// An enabled guest needs no runcmd at all: both units are baked in and
	// enabled in the image, and everything they read arrives in write_files.
	var runcmd []string

	if !dpuagent.Enabled(fleet) {
		// Without a configuration and a credential neither baked unit has work
		// to do, and the join helper would fail in a loop, so mask both.
		//
		// The mask must be persistent: runcmd runs once per instance, so a
		// --runtime mask in /run would be gone after a reboot and both units
		// would start. A persistent mask links /etc/systemd/system/<unit> to
		// /dev/null, which outranks the image's vendor units in
		// /usr/lib/systemd/system. It fails if the image ever ships either unit
		// in /etc instead.
		//
		// runcmd runs inside cloud-final.service, and the image orders both
		// units After=cloud-final.service, so their start jobs are still queued
		// here and --now replaces them with stops.
		for _, unit := range []string{dpuagent.MockDPUAgentUnit, dpuagent.WaitKubeletJoinUnit} {
			runcmd = append(runcmd, "  - systemctl mask --now "+unit)
		}
	}

	// A runcmd key with nothing under it parses as null and makes cloud-init
	// log an error for a section that has no work in it, so leave it out.
	runcmdSection := ""
	if len(runcmd) > 0 {
		runcmdSection = "runcmd:\n" + strings.Join(runcmd, "\n") + "\n"
	}

	return fmt.Sprintf(`#cloud-config
hostname: %s
ssh_pwauth: true
chpasswd:
  expire: false
  list: |
    %s:%s
users:
  - name: %s
    lock_passwd: false
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    groups: sudo
    shell: /bin/bash
write_files:
%s%s`, naming.VirtualMachineName(fleet.Name, ordinal),
		cloudInitDebugUser, cloudInitDebugPassword, cloudInitDebugUser,
		writeFiles.String(), runcmdSection), nil
}

// dpuIdentityContent states which DPU this guest is going to become. Every
// value is derived from the fleet and the ordinal, because the guest has to
// hold this identity before DPF creates the DPU that carries it. The join
// helper reads DPU_NAME to build the join Secret name its credential allows.
func dpuIdentityContent(fleet *scalev1alpha1.SimFleet, ordinal int) string {
	ref := scalev1alpha1.DPUClusterReference{}
	if fleet.Spec.Bootstrap != nil {
		ref = fleet.Spec.Bootstrap.DPUClusterRef
	}
	identityLines := []string{
		fmt.Sprintf("DPU_NAME=%s", naming.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, ordinal)),
		fmt.Sprintf("DPU_NAMESPACE=%s", fleet.Namespace),
		fmt.Sprintf("DPU_NODE_NAME=%s", naming.NodeName(
			fleet.Name, naming.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
		)),
		fmt.Sprintf("DPU_DEVICE_NAME=%s", naming.DeviceName(fleet.Name, ordinal)),
		fmt.Sprintf("DPU_CLUSTER_NAME=%s", ref.Name),
		fmt.Sprintf("DPU_CLUSTER_NAMESPACE=%s", ref.Namespace),
	}
	return strings.Join(identityLines, "\n") + "\n"
}

func yamlLiteral(content, indent string) (string, string) {
	indicator := "|2-"
	if strings.HasSuffix(content, "\n\n") {
		indicator = "|2+"
	} else if strings.HasSuffix(content, "\n") {
		indicator = "|2"
	}
	return indicator, indentBlock(content, indent)
}

func indentBlock(content, indent string) string {
	content = strings.TrimSuffix(content, "\n")
	return indent + strings.ReplaceAll(content, "\n", "\n"+indent) + "\n"
}
