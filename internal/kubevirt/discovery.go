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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
	virtv1 "kubevirt.io/api/core/v1"
)

// Installed reports whether the cluster serves the KubeVirt kinds the
// controller reads: the VirtualMachine and VirtualMachineInstance of
// kubevirt.io/v1. A cluster without them is a normal configuration, so only a
// failure to ask is an error.
func Installed(client discovery.DiscoveryInterface) (bool, error) {
	list, err := client.ServerResourcesForGroupVersion(virtv1.GroupVersion.String())
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("discover %s: %w", virtv1.GroupVersion, err)
	}
	served := map[string]bool{}
	for _, resource := range list.APIResources {
		served[resource.Name] = true
	}
	return served["virtualmachines"] && served["virtualmachineinstances"], nil
}
