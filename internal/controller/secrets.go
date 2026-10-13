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
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

const (
	maxCloudInitUserdataBytes = 16 * 1024
	dpuIdentityPath           = "/etc/dpf-sim/identity.env"
	// Debug console/SSH credentials for simulated guests. Cloud-init unlocks the Ubuntu image's ubuntu user.
	cloudInitDebugUser     = "ubuntu"
	cloudInitDebugPassword = "ubuntu"
)

// bootstrapOutcome is what the fleet's instances report about their
// per-ordinal Secrets. cloudInit holds the ordinals whose credentials and
// cloud-init Secret are in place.
type bootstrapOutcome struct {
	cloudInit map[int]struct{}
	// blocked names why an instance that hit no failure still wrote nothing,
	// so status reports the blockage instead of claiming progress.
	blocked *bootstrapConditionError
	err     error
}

// credentialsEligible reports whether an instance could issue a per-ordinal
// management credential at all: only a fleet that boots guests with the agent
// enabled gets one.
func credentialsEligible(fleet *scalev1alpha1.SimFleet) bool {
	return fleet.Spec.VirtualMachineTemplate != nil &&
		validateVirtualMachineTemplate(fleet.Spec.VirtualMachineTemplate) == nil &&
		dpuAgentEnabled(fleet)
}

func cloudInitSecretLabels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	return inventoryLabels(fleet, fmt.Sprintf("%06d", ordinal))
}

// desiredCloudInitSecret renders a complete, create-only cloud-init payload.
// It is pure: callers must resolve and validate all dependencies first.
func desiredCloudInitSecret(
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
			Name:      utils.CloudInitSecretName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    cloudInitSecretLabels(fleet, ordinal),
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

	identityContent := dpuIdentityContent(fleet, ordinal)
	identityIndicator, indentedIdentity := yamlLiteral(identityContent, "      ")
	writeFiles := fmt.Sprintf(`  - path: %s
    permissions: "0600"
    content: %s
%s`, dpuIdentityPath, identityIndicator, indentedIdentity)
	// An enabled guest needs no runcmd at all: both units are baked in and
	// enabled in the image, and everything they read arrives in write_files.
	var runcmd []string

	if dpuAgentEnabled(fleet) {
		if len(managementKubeconfig) == 0 {
			return "", fmt.Errorf("management kubeconfig for ordinal %d is empty", ordinal)
		}
		conf, err := renderMockDPUAgentConfig(fleet, utils.SerialNumber(fleet.Name, ordinal))
		if err != nil {
			return "", fmt.Errorf("render dpuagent configuration for ordinal %d: %w", ordinal, err)
		}
		confIndicator, indentedConf := yamlLiteral(conf, "      ")
		kubeconfigIndicator, indentedKubeconfig := yamlLiteral(string(managementKubeconfig), "      ")
		writeFiles += fmt.Sprintf(`  - path: %s
    permissions: "0600"
    content: %s
%s  - path: %s
    permissions: "0600"
    content: %s
%s`, dpuAgentConfPath, confIndicator, indentedConf, dpuAgentKubeconfigPath, kubeconfigIndicator, indentedKubeconfig)
	} else {
		// Neither baked unit has anything to read without a configuration and a
		// credential, and the join helper would otherwise fail on a loop.
		//
		// A persistent mask, deliberately not --runtime. systemd searches
		// /etc, then /run, then /usr/lib, and the image installs both units
		// as vendor units under /usr/lib/systemd/system precisely so this
		// can work: the mask writes /etc/systemd/system/<unit> -> /dev/null,
		// which outranks the vendor copy and survives a reboot. --runtime
		// would write to /run, which still outranks /usr/lib, but it would
		// be lost on reboot for no benefit, and it silently stops working
		// the moment anything puts a real unit file in /etc.
		//
		// Ordering holds: runcmd runs inside cloud-final.service, both units
		// are WantedBy=cloud-init.target and After=cloud-final.service, so
		// their jobs are still blocked when this runs and --now cancels
		// them.
		for _, unit := range []string{mockDPUAgentUnit, waitKubeletJoinUnit} {
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
%s%s`, utils.VirtualMachineName(fleet.Name, ordinal),
		cloudInitDebugUser, cloudInitDebugPassword, cloudInitDebugUser,
		writeFiles, runcmdSection), nil
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
		fmt.Sprintf("DPU_NAME=%s", utils.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, ordinal)),
		fmt.Sprintf("DPU_NAMESPACE=%s", fleet.Namespace),
		fmt.Sprintf("DPU_NODE_NAME=%s", utils.NodeName(
			fleet.Name, utils.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
		)),
		fmt.Sprintf("DPU_DEVICE_NAME=%s", utils.DeviceName(fleet.Name, ordinal)),
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
