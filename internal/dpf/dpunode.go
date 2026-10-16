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

package dpf

import (
	"fmt"
	"maps"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// DesiredHostNode lists the host's devices among the first replicas ordinals,
// so a host the rollout has only partly admitted gets only those devices.
func DesiredHostNode(fleet *scalev1alpha1.SimFleet, hostIndex, replicas int) *provisioningv1.DPUNode {
	start, end := naming.HostOrdinalRange(hostIndex, fleet.Spec.DevicesPerDPUNode, replicas)
	dpus := make([]provisioningv1.DPURef, 0, end-start)
	for ordinal := start; ordinal < end; ordinal++ {
		dpus = append(dpus, provisioningv1.DPURef{Name: naming.DeviceName(fleet.Name, ordinal)})
	}
	return &provisioningv1.DPUNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.NodeName(fleet.Name, hostIndex),
			Namespace: fleet.Namespace,
			Labels:    nodeLabels(fleet, hostIndex),
		},
		Spec: provisioningv1.DPUNodeSpec{
			DPUs:             dpus,
			NodeRebootMethod: nodeRebootMethod(fleet),
		},
	}
}

func nodeRebootMethod(fleet *scalev1alpha1.SimFleet) *provisioningv1.NodeRebootMethod {
	if fleet.Spec.DPUNodeTemplate != nil && fleet.Spec.DPUNodeTemplate.Spec.NodeRebootMethod != nil {
		return fleet.Spec.DPUNodeTemplate.Spec.NodeRebootMethod.DeepCopy()
	}
	return &provisioningv1.NodeRebootMethod{
		External: &provisioningv1.External{},
	}
}

func nodeLabels(fleet *scalev1alpha1.SimFleet, hostIndex int) map[string]string {
	labels := map[string]string{}
	if fleet.Spec.DPUNodeTemplate != nil {
		maps.Copy(labels, fleet.Spec.DPUNodeTemplate.Metadata.Labels)
	}
	maps.Copy(labels, naming.InventoryLabels(fleet, fmt.Sprintf("%04d", hostIndex)))
	return labels
}

// AppendMissingDPUs returns existing plus every desired ref it does not already
// list. Existing members survive in place, so a ref DPF or an operator added
// stays and membership only grows. A plain overwrite would fight whoever wrote
// those extra refs; refusing to touch a node whose refs were replaced would
// leave the host short of a device forever, because nothing would ever put the
// desired ref back.
func AppendMissingDPUs(existing, desired []provisioningv1.DPURef) []provisioningv1.DPURef {
	members := make(map[string]struct{}, len(existing))
	for _, dpu := range existing {
		members[dpu.Name] = struct{}{}
	}
	merged := append([]provisioningv1.DPURef(nil), existing...)
	for _, dpu := range desired {
		if _, listed := members[dpu.Name]; listed {
			continue
		}
		merged = append(merged, dpu)
		members[dpu.Name] = struct{}{}
	}
	return merged
}
