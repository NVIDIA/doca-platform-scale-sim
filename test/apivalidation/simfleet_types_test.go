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

package apivalidation_test

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"testing"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

const dpuAgentImmutableRule = "!has(oldSelf.dpuAgent) ? !has(self.dpuAgent) : self.dpuAgent == oldSelf.dpuAgent"

func TestSimFleetInventoryConditionConstants(t *testing.T) {
	if scalev1alpha1.ConditionInventoryReady != "InventoryReady" {
		t.Fatalf("ConditionInventoryReady = %q, want InventoryReady", scalev1alpha1.ConditionInventoryReady)
	}
	if scalev1alpha1.ConditionDPUsObserved != "DPUsObserved" {
		t.Fatalf("ConditionDPUsObserved = %q, want DPUsObserved", scalev1alpha1.ConditionDPUsObserved)
	}
	if scalev1alpha1.ConditionBootstrapReady != "BootstrapReady" {
		t.Fatalf("ConditionBootstrapReady = %q, want BootstrapReady", scalev1alpha1.ConditionBootstrapReady)
	}
}

func TestSimFleetInventorySpecDeepCopy(t *testing.T) {
	pfs := 2
	spec := scalev1alpha1.SimFleetSpec{
		Replicas:              20,
		DevicesPerDPUNode:     10,
		DeviceSelectionLabels: map[string]string{"simfleet": "scale-3000"},
		DPUNodeTemplate: &scalev1alpha1.DPUNodeTemplate{
			Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{"role": "dpu-node"}},
			Spec: scalev1alpha1.DPUNodeTemplateSpec{
				NodeRebootMethod: &provisioningv1.NodeRebootMethod{
					External: &provisioningv1.External{},
				},
			},
		},
		DPUDeviceTemplate: &scalev1alpha1.DPUDeviceTemplate{
			Spec: scalev1alpha1.DPUDeviceTemplateSpec{NumberOfPFs: &pfs},
		},
	}

	copied := spec.DeepCopy()
	if copied.Replicas != spec.Replicas ||
		copied.DevicesPerDPUNode != spec.DevicesPerDPUNode ||
		copied.DeviceSelectionLabels["simfleet"] != "scale-3000" ||
		copied.DPUNodeTemplate == nil ||
		copied.DPUDeviceTemplate == nil ||
		copied.DPUDeviceTemplate.Spec.NumberOfPFs == nil ||
		*copied.DPUDeviceTemplate.Spec.NumberOfPFs != pfs {
		t.Fatal("DeepCopy did not preserve inventory spec fields")
	}

	copied.DeviceSelectionLabels["simfleet"] = "mutated"
	if spec.DeviceSelectionLabels["simfleet"] != "scale-3000" {
		t.Fatal("DeepCopy should deep-copy DeviceSelectionLabels")
	}
}

func TestSimFleetInventoryStatusDeepCopy(t *testing.T) {
	status := scalev1alpha1.SimFleetStatus{
		ObservedGeneration:   3,
		AdmittedReplicas:     20,
		CreatedInstances:     19,
		VMReadyInstances:     18,
		ProvisionedInstances: 17,
		FailedInstances:      1,
		DPUNodes:             2,
		Conditions: []metav1.Condition{
			{Type: scalev1alpha1.ConditionInventoryReady, Status: metav1.ConditionTrue},
			{Type: scalev1alpha1.ConditionDPUsObserved, Status: metav1.ConditionFalse},
		},
	}

	copied := status.DeepCopy()
	if copied.ObservedGeneration != 3 ||
		copied.AdmittedReplicas != 20 ||
		copied.CreatedInstances != 19 ||
		copied.VMReadyInstances != 18 ||
		copied.ProvisionedInstances != 17 ||
		copied.FailedInstances != 1 ||
		copied.DPUNodes != 2 ||
		len(copied.Conditions) != 2 {
		t.Fatal("DeepCopy did not preserve inventory status fields")
	}

	copied.Conditions[0].Status = metav1.ConditionUnknown
	if status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatal("DeepCopy should deep-copy Conditions")
	}
}

// The rollout index survives a restart only if the generated CRD carries it.
// A Go field the schema prunes would look correct in memory and be lost on
// every write.
func TestSimFleetStatusSchemaCarriesTheRolloutCounts(t *testing.T) {
	status := simFleetStatusSchema(t)
	got := make([]string, 0, len(status.Properties))
	for name, property := range status.Properties {
		if name != "conditions" && property.Type != "integer" {
			t.Errorf("status.%s type = %q, want integer", name, property.Type)
		}
		got = append(got, name)
	}
	slices.Sort(got)
	want := []string{
		"admittedReplicas", "conditions", "createdInstances", "dpuNodes", "failedInstances",
		"observedGeneration", "provisionedInstances", "vmReadyInstances",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("status properties = %v, want %v", got, want)
	}
}

func TestSimFleetPrintColumns(t *testing.T) {
	crd := loadSimFleetCRD(t)
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("SimFleet CRD has no versions")
	}
	want := []apiextensionsv1.CustomResourceColumnDefinition{
		{Name: "Replicas", Type: "integer", JSONPath: ".spec.replicas"},
		{Name: "Admitted", Type: "integer", JSONPath: ".status.admittedReplicas"},
		{Name: "Provisioned", Type: "integer", JSONPath: ".status.provisionedInstances"},
		{Name: "Failed", Type: "integer", JSONPath: ".status.failedInstances"},
		{Name: "Ready", Type: "string", JSONPath: ".status.conditions[?(@.type=='InventoryReady')].status"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}
	if got := crd.Spec.Versions[0].AdditionalPrinterColumns; !slices.Equal(got, want) {
		t.Fatalf("printer columns = %+v, want %+v", got, want)
	}
}

// The controller filters reserved keys out of a selector, but filtering is
// silent: a fleet author who uses one gets a device that never matches their
// DPUSet and nothing to explain it. Rejecting the selector at admission is
// where the author actually finds out.
func TestSimFleetDeviceSelectionLabelsRejectReservedKeys(t *testing.T) {
	spec := simFleetSpecSchema(t)
	selector, ok := spec.Properties["deviceSelectionLabels"]
	if !ok {
		t.Fatal("spec schema is missing deviceSelectionLabels")
	}
	if len(selector.XValidations) == 0 {
		t.Fatal("deviceSelectionLabels has no CEL validation for reserved keys")
	}

	rule := selector.XValidations[0]
	for _, reserved := range scalev1alpha1.ReservedDeviceLabelKeys {
		if !strings.Contains(rule.Rule, "'"+reserved+"'") {
			t.Errorf("deviceSelectionLabels rule does not reject reserved key %q: %s",
				reserved, rule.Rule)
		}
		if !strings.Contains(rule.Message, reserved) {
			t.Errorf("deviceSelectionLabels message does not name reserved key %q: %s",
				reserved, rule.Message)
		}
	}
	// A rule that only listed keys would still pass the checks above if it
	// never actually looked at the selector's keys.
	if !strings.Contains(rule.Rule, "self.exists(") {
		t.Fatalf("deviceSelectionLabels rule does not test the selector's own keys: %s", rule.Rule)
	}
}

// Nothing consumes the reserved list unless it is complete, so pin it here
// too: this is the list the CEL rule above and the controller filter share.
func TestReservedDeviceLabelKeysAreTheIdentityLabels(t *testing.T) {
	want := []string{
		"scale.dpu.nvidia.com/fleet-name",
		"scale.dpu.nvidia.com/fleet-uid",
		"scale.dpu.nvidia.com/ordinal",
		"provisioning.dpu.nvidia.com/dpunode-name",
	}
	if !slices.Equal(scalev1alpha1.ReservedDeviceLabelKeys, want) {
		t.Fatalf("ReservedDeviceLabelKeys = %v, want %v",
			scalev1alpha1.ReservedDeviceLabelKeys, want)
	}
}

func TestSimFleetBootstrapSpecDeepCopy(t *testing.T) {
	spec := scalev1alpha1.SimFleetSpec{
		Replicas:              1,
		DevicesPerDPUNode:     1,
		DeviceSelectionLabels: map[string]string{"simfleet": "scale-3000"},
		Bootstrap: &scalev1alpha1.BootstrapSpec{
			DPUClusterRef: scalev1alpha1.DPUClusterReference{
				Name:      "tenant1",
				Namespace: "dpf-operator-system",
			},
		},
	}

	copied := spec.DeepCopy()
	if copied.Bootstrap == nil ||
		copied.Bootstrap.DPUClusterRef.Name != "tenant1" ||
		copied.Bootstrap.DPUClusterRef.Namespace != "dpf-operator-system" {
		t.Fatal("DeepCopy did not preserve bootstrap spec fields")
	}

	copied.Bootstrap.DPUClusterRef.Name = "mutated"
	if spec.Bootstrap.DPUClusterRef.Name != "tenant1" {
		t.Fatal("DeepCopy should deep-copy Bootstrap")
	}
}

func TestSimFleetDPUAgentSchemaDefaultsAndImmutability(t *testing.T) {
	spec := simFleetSpecSchema(t)

	if !hasValidationRule(spec.XValidations, dpuAgentImmutableRule, "spec.dpuAgent is immutable") {
		t.Fatal("spec schema missing dpuAgent immutability validation")
	}

	dpuAgent, ok := spec.Properties["dpuAgent"]
	if !ok {
		t.Fatal("spec schema missing dpuAgent property")
	}
	if dpuAgent.Properties == nil {
		t.Fatal("dpuAgent schema missing properties")
	}
	props := dpuAgent.Properties

	assertDefaultJSON(t, props, "enabled", `true`)
	assertDefaultJSON(t, props, "dpuType", `"bf3"`)
	assertDefaultJSON(t, props, "rebootMethod", `"NoAction"`)

	const rebootMethodPattern = `^(NoAction|PowerCycle|SLR)(,(NoAction|PowerCycle|SLR))*$`
	if got := props["rebootMethod"].Pattern; got != rebootMethodPattern {
		t.Fatalf("dpuAgent.rebootMethod pattern = %q, want %q", got, rebootMethodPattern)
	}
	re := regexp.MustCompile(props["rebootMethod"].Pattern)
	for _, value := range []string{"NoAction", "PowerCycle", "SLR", "NoAction,PowerCycle,SLR"} {
		if !re.MatchString(value) {
			t.Errorf("rebootMethod pattern rejects valid value %q", value)
		}
	}
	for _, value := range []string{"", "External", "NoAction,", "NoAction,SLRPowerCycle", " NoAction"} {
		if re.MatchString(value) {
			t.Errorf("rebootMethod pattern accepts invalid value %q", value)
		}
	}

	for _, removed := range []string{
		"controlPlaneMTU", "nicDeviceCount", "extraArgs", "skipSysctl",
		"skipNetworkConfig", "skipDNSConfig", "skipContainerdConfig",
		"skipSFConfig", "skipVFMac", "skipOVSRawScript", "skipKernelCmdLine",
		"skipRemoveBuiltinKubelet", "skipConfigureKubelet", "skipStartKubelet",
		"skipRebootMethodDiscovery", "skipNodeLabeling", "skipAstra",
		"skipDPUMode", "skipNVConfig", "skipReboot",
	} {
		if _, ok := props[removed]; ok {
			t.Errorf("dpuAgent schema still contains removed property %q", removed)
		}
	}
}

func assertDefaultJSON(t *testing.T, props map[string]apiextensionsv1.JSONSchemaProps, field, want string) {
	t.Helper()
	raw, ok := propertyDefaultRaw(props, field)
	if !ok {
		t.Fatalf("dpuAgent.%s missing default", field)
	}
	if !bytes.Equal(raw, []byte(want)) {
		t.Fatalf("dpuAgent.%s default = %s, want %s", field, string(raw), want)
	}
}

func TestSimFleetDPUAgentSpecDeepCopy(t *testing.T) {
	enabled := true
	dpuType := "bf4"
	rebootMethod := "NoAction"
	spec := scalev1alpha1.SimFleetSpec{
		Replicas:              1,
		DevicesPerDPUNode:     1,
		DeviceSelectionLabels: map[string]string{"simfleet": "scale-3000"},
		DPUAgent: &scalev1alpha1.DPUAgentSpec{
			Enabled:      &enabled,
			DPUType:      &dpuType,
			RebootMethod: &rebootMethod,
		},
	}

	copied := spec.DeepCopy()
	if copied.DPUAgent == nil ||
		copied.DPUAgent.Enabled == nil ||
		copied.DPUAgent.DPUType == nil ||
		copied.DPUAgent.RebootMethod == nil {
		t.Fatal("DeepCopy did not preserve dpuAgent spec fields")
	}

	*copied.DPUAgent.Enabled = false
	*copied.DPUAgent.DPUType = "bf3"
	*copied.DPUAgent.RebootMethod = "External"

	if *spec.DPUAgent.Enabled != true {
		t.Fatal("DeepCopy should deep-copy Enabled pointer")
	}
	if *spec.DPUAgent.DPUType != "bf4" {
		t.Fatal("DeepCopy should deep-copy DPUType pointer")
	}
	if *spec.DPUAgent.RebootMethod != "NoAction" {
		t.Fatal("DeepCopy should deep-copy RebootMethod pointer")
	}
}
