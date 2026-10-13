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
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

func dpuAgentBMCSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	dpuAgent, ok := simFleetSpecSchema(t).Properties["dpuAgent"]
	if !ok {
		t.Fatal("spec schema missing dpuAgent")
	}
	bmc, ok := dpuAgent.Properties["bmc"]
	if !ok {
		t.Fatal("dpuAgent schema missing bmc")
	}
	return bmc
}

func delayOverrideItemSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	overrides, ok := dpuAgentBMCSchema(t).Properties["responseDelayOverrides"]
	if !ok {
		t.Fatal("bmc schema missing responseDelayOverrides")
	}
	if overrides.Items == nil || overrides.Items.Schema == nil {
		t.Fatal("responseDelayOverrides has no item schema")
	}
	return *overrides.Items.Schema
}

func enumStrings(t *testing.T, enum []apiextensionsv1.JSON) []string {
	t.Helper()
	values := make([]string, 0, len(enum))
	for _, raw := range enum {
		var s string
		if err := json.Unmarshal(raw.Raw, &s); err != nil {
			t.Fatalf("enum value %s is not a string: %v", raw.Raw, err)
		}
		values = append(values, s)
	}
	slices.Sort(values)
	return values
}

func assertStringBounds(t *testing.T, field string, p apiextensionsv1.JSONSchemaProps, minLen, maxLen int64) {
	t.Helper()
	if p.MinLength == nil || *p.MinLength != minLen || p.MaxLength == nil || *p.MaxLength != maxLen {
		t.Errorf("%s length bounds = %v..%v, want %d..%d", field, p.MinLength, p.MaxLength, minLen, maxLen)
	}
}

func assertDelayBounds(t *testing.T, field string, p apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	for _, name := range []string{"minSeconds", "maxSeconds"} {
		s := p.Properties[name]
		if s.Minimum == nil || *s.Minimum != 0 || s.Maximum == nil || *s.Maximum != 600 {
			t.Errorf("%s.%s bounds = %v..%v, want 0..600", field, name, s.Minimum, s.Maximum)
		}
		if !slices.Contains(p.Required, name) {
			t.Errorf("%s.%s is not required", field, name)
		}
	}
}

func assertNoDefaults(t *testing.T, path string, p apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	if p.Default != nil {
		t.Errorf("%s has default %s; spec.dpuAgent.bmc fields must not default", path, p.Default.Raw)
	}
	for name, child := range p.Properties {
		assertNoDefaults(t, path+"."+name, child)
	}
	if p.Items != nil && p.Items.Schema != nil {
		assertNoDefaults(t, path+"[]", *p.Items.Schema)
	}
}

func TestDPUAgentBMCSchemaHasNoDefaults(t *testing.T) {
	assertNoDefaults(t, "spec.dpuAgent.bmc", dpuAgentBMCSchema(t))
}

func TestDPUAgentBMCSchemaBounds(t *testing.T) {
	bmc := dpuAgentBMCSchema(t)

	assertStringBounds(t, "psid", bmc.Properties["psid"], 1, 32)
	firmware := bmc.Properties["firmware"]
	for _, name := range []string{"bmc", "erot", "uefi", "nic"} {
		assertStringBounds(t, "firmware."+name, firmware.Properties[name], 1, 64)
	}

	assertDelayBounds(t, "responseDelay", bmc.Properties["responseDelay"])

	overrides := bmc.Properties["responseDelayOverrides"]
	if overrides.MaxItems == nil || *overrides.MaxItems != 32 {
		t.Errorf("responseDelayOverrides maxItems = %v, want 32", overrides.MaxItems)
	}
	item := delayOverrideItemSchema(t)
	assertDelayBounds(t, "responseDelayOverrides[]", item)
	if !slices.Contains(item.Required, "name") {
		t.Error("responseDelayOverrides[].name is not required")
	}

	methods := item.Properties["methods"]
	if methods.MinItems == nil || *methods.MinItems != 1 || methods.MaxItems == nil || *methods.MaxItems != 4 {
		t.Errorf("methods item bounds = %v..%v, want 1..4", methods.MinItems, methods.MaxItems)
	}
	if methods.XListType == nil || *methods.XListType != "set" {
		t.Errorf("methods list type = %v, want set", methods.XListType)
	}
	if methods.Items == nil || methods.Items.Schema == nil {
		t.Fatal("methods has no item schema")
	}
	if got, want := enumStrings(t, methods.Items.Schema.Enum), []string{"*", "DELETE", "GET", "PATCH", "POST"}; !slices.Equal(got, want) {
		t.Errorf("methods enum = %v, want %v", got, want)
	}

	if overrides.XListType == nil || *overrides.XListType != "atomic" {
		t.Errorf("responseDelayOverrides list type = %v, want atomic", overrides.XListType)
	}
	if len(bmc.Required) != 0 {
		t.Errorf("bmc.Required = %v, want empty (all fields optional)", bmc.Required)
	}
}

func TestDPUAgentOverrideNameEnumMatchesRouteTable(t *testing.T) {
	if got := len(scalev1alpha1.MockRedfishOperations); got != 35 {
		t.Fatalf("MockRedfishOperations has %d operations, want the 35 of the pinned mock", got)
	}
	got := enumStrings(t, delayOverrideItemSchema(t).Properties["name"].Enum)
	want := slices.Sorted(maps.Keys(scalev1alpha1.MockRedfishOperations))
	if !slices.Equal(got, want) {
		t.Fatalf("responseDelayOverrides[].name enum\ngot:  %v\nwant: %v", got, want)
	}
}

func mockRedfishOperationsCEL() string {
	names := slices.Sorted(maps.Keys(scalev1alpha1.MockRedfishOperations))
	entries := make([]string, 0, len(names))
	for _, name := range names {
		methods := slices.Sorted(slices.Values(scalev1alpha1.MockRedfishOperations[name]))
		quoted := make([]string, len(methods))
		for i, m := range methods {
			quoted[i] = "'" + m + "'"
		}
		entries = append(entries, fmt.Sprintf("'%s': [%s]", name, strings.Join(quoted, ", ")))
	}
	return "{" + strings.Join(entries, ", ") + "}"
}

func TestDPUAgentOverrideMethodsRuleMatchesRouteTable(t *testing.T) {
	rule := "!has(self.methods) || self.methods.all(m, m == '*' || m in " +
		mockRedfishOperationsCEL() + "[self.name])"
	if !hasValidationRule(delayOverrideItemSchema(t).XValidations, rule,
		"methods must be served by the named Redfish operation") {
		t.Fatalf("responseDelayOverrides[] has no methods rule matching MockRedfishOperations; want rule:\n%s", rule)
	}
}
