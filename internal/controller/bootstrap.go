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

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/pod"
)

// bootstrapConditionError names a configuration the controller refuses, so
// status can report the configuration rather than a bare failure.
//
// There is deliberately nothing here that reaches the tenant cluster. Guests
// read DPF's per-DPU join Secret themselves with the management credential
// cloud-init installs, so the controller mints no tenant bootstrap token and
// never needs a DPUCluster to be reachable before a VirtualMachine can boot.
type bootstrapConditionError struct {
	reason  string
	message string
}

func (e *bootstrapConditionError) Error() string {
	return e.message
}

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
// management credential at all: only a fleet that boots guests with the dpuagent
// enabled gets one.
func credentialsEligible(fleet *scalev1alpha1.SimFleet) bool {
	switch instanceGuestOf(fleet) {
	case instanceGuestPod:
		return pod.ValidateTemplate(fleet.Spec.PodTemplate) == nil && dpuagent.Enabled(fleet)
	case instanceGuestVirtualMachine:
		return kubevirt.ValidateTemplate(fleet.Spec.VirtualMachineTemplate) == nil && dpuagent.Enabled(fleet)
	}
	return false
}

// fleetBootsGuests reports whether the fleet asks for a guest per ordinal,
// as a VirtualMachine or as a Pod.
func fleetBootsGuests(fleet *scalev1alpha1.SimFleet) bool {
	return instanceGuestOf(fleet) != instanceGuestNone
}

// guestTemplateError is the error the fleet's guest template carries, or nil
// when it can be rendered. A VirtualMachine template needs KubeVirt; a
// fleet with a Pod template never does, and one with no guest has no
// template to refuse.
func guestTemplateError(fleet *scalev1alpha1.SimFleet, kubeVirtInstalled bool) error {
	switch instanceGuestOf(fleet) {
	case instanceGuestPod:
		return pod.ValidateTemplate(fleet.Spec.PodTemplate)
	case instanceGuestVirtualMachine:
		if !kubeVirtInstalled {
			return kubevirt.ErrNotInstalled
		}
		return kubevirt.ValidateTemplate(fleet.Spec.VirtualMachineTemplate)
	}
	return nil
}

// conditionReason names the configuration a condition is blocked on, whether
// that is a refused bootstrap state or the VirtualMachine template.
func conditionReason(err error) string {
	var bootstrapErr *bootstrapConditionError
	if errors.As(err, &bootstrapErr) {
		return bootstrapErr.reason
	}
	var vmErr *kubevirt.TemplateError
	if errors.As(err, &vmErr) {
		return vmErr.Reason
	}
	return "ReconcileError"
}
