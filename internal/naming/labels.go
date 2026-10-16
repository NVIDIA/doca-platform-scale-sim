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

package naming

import (
	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

// The identity labels on every object a fleet creates. The manager cache
// selects Secrets, ServiceAccounts, Roles and RoleBindings by LabelFleetName,
// so one of those without it is invisible to the controllers.
const (
	LabelFleetName = "scale.dpu.nvidia.com/fleet-name"
	LabelFleetUID  = "scale.dpu.nvidia.com/fleet-uid"
	LabelOrdinal   = "scale.dpu.nvidia.com/ordinal"
)

// InventoryLabels are the labels every object a fleet creates carries.
// ordinal is the already formatted ordinal or host index.
func InventoryLabels(fleet *scalev1alpha1.SimFleet, ordinal string) map[string]string {
	return map[string]string{
		LabelFleetName: fleet.Name,
		LabelFleetUID:  string(fleet.UID),
		LabelOrdinal:   ordinal,
	}
}
