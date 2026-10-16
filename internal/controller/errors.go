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

import "errors"

// bootstrapConditionError names a configuration the controller refuses, so
// status can report the configuration rather than a bare failure.
//
// There is deliberately nothing here that reaches the tenant cluster. Guests
// read DPF's per-DPU join Secret themselves with the management credential
// cloud-init installs, so the controller mints no tenant bootstrap token and
// never needs a DPUCluster to be reachable before a VirtualMachine can boot.
// spec.bootstrap.dpuClusterRef survives only as the cluster identity written
// into the guest's identity.env.
type bootstrapConditionError struct {
	reason  string
	message string
}

func (e *bootstrapConditionError) Error() string {
	return e.message
}

// virtualMachineConditionError names a VirtualMachine template the controller
// refuses to render.
type virtualMachineConditionError struct {
	reason  string
	message string
}

func (e *virtualMachineConditionError) Error() string {
	return e.message
}

// conditionReason names the configuration a condition is blocked on, whether
// that is a refused bootstrap state or the VirtualMachine template.
func conditionReason(err error) string {
	var bootstrapErr *bootstrapConditionError
	if errors.As(err, &bootstrapErr) {
		return bootstrapErr.reason
	}
	var vmErr *virtualMachineConditionError
	if errors.As(err, &vmErr) {
		return vmErr.reason
	}
	return "ReconcileError"
}
