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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

func dpuAgentBMCSchema() apiextensionsv1.JSONSchemaProps {
	GinkgoHelper()
	spec := simFleetSpecSchema()
	Expect(spec.Properties).To(HaveKey("dpuAgent"), "spec schema missing dpuAgent")
	dpuAgent := spec.Properties["dpuAgent"]
	Expect(dpuAgent.Properties).To(HaveKey("bmc"), "dpuAgent schema missing bmc")
	return dpuAgent.Properties["bmc"]
}

func delayOverrideItemSchema() apiextensionsv1.JSONSchemaProps {
	GinkgoHelper()
	bmc := dpuAgentBMCSchema()
	Expect(bmc.Properties).To(HaveKey("responseDelayOverrides"), "bmc schema missing responseDelayOverrides")
	overrides := bmc.Properties["responseDelayOverrides"]
	Expect(overrides.Items).NotTo(BeNil(), "responseDelayOverrides has no item schema")
	Expect(overrides.Items.Schema).NotTo(BeNil(), "responseDelayOverrides has no item schema")
	return *overrides.Items.Schema
}

func enumStrings(enum []apiextensionsv1.JSON) []string {
	GinkgoHelper()
	values := make([]string, 0, len(enum))
	for _, raw := range enum {
		var s string
		Expect(json.Unmarshal(raw.Raw, &s)).To(Succeed(), "enum value %s is not a string", raw.Raw)
		values = append(values, s)
	}
	slices.Sort(values)
	return values
}

func assertStringBounds(field string, p apiextensionsv1.JSONSchemaProps, minLen, maxLen int64) {
	GinkgoHelper()
	Expect(p.MinLength).To(HaveValue(Equal(minLen)), "%s min length", field)
	Expect(p.MaxLength).To(HaveValue(Equal(maxLen)), "%s max length", field)
}

func assertDelayBounds(field string, p apiextensionsv1.JSONSchemaProps) {
	GinkgoHelper()
	for _, name := range []string{"minSeconds", "maxSeconds"} {
		s := p.Properties[name]
		Expect(s.Minimum).To(HaveValue(BeZero()), "%s.%s minimum", field, name)
		Expect(s.Maximum).To(HaveValue(BeEquivalentTo(600)), "%s.%s maximum", field, name)
		Expect(p.Required).To(ContainElement(name), "%s.%s is not required", field, name)
	}
}

func assertNoDefaults(path string, p apiextensionsv1.JSONSchemaProps) {
	GinkgoHelper()
	Expect(p.Default).To(BeNil(), func() string {
		return fmt.Sprintf("%s has default %s; spec.dpuAgent.bmc fields must not default", path, p.Default.Raw)
	})
	for name, child := range p.Properties {
		assertNoDefaults(path+"."+name, child)
	}
	if p.Items != nil && p.Items.Schema != nil {
		assertNoDefaults(path+"[]", *p.Items.Schema)
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

var _ = Describe("SimFleet spec.dpuAgent.bmc schema", func() {
	It("declares no defaults anywhere under bmc", func() {
		assertNoDefaults("spec.dpuAgent.bmc", dpuAgentBMCSchema())
	})

	It("bounds every string, delay and list", func() {
		bmc := dpuAgentBMCSchema()

		assertStringBounds("psid", bmc.Properties["psid"], 1, 32)
		firmware := bmc.Properties["firmware"]
		for _, name := range []string{"bmc", "erot", "uefi", "nic"} {
			assertStringBounds("firmware."+name, firmware.Properties[name], 1, 64)
		}

		assertDelayBounds("responseDelay", bmc.Properties["responseDelay"])

		overrides := bmc.Properties["responseDelayOverrides"]
		Expect(overrides.MaxItems).To(HaveValue(BeEquivalentTo(32)), "responseDelayOverrides maxItems")
		item := delayOverrideItemSchema()
		assertDelayBounds("responseDelayOverrides[]", item)
		Expect(item.Required).To(ContainElement("name"), "responseDelayOverrides[].name is not required")

		methods := item.Properties["methods"]
		Expect(methods.MinItems).To(HaveValue(BeEquivalentTo(1)), "methods min items")
		Expect(methods.MaxItems).To(HaveValue(BeEquivalentTo(4)), "methods max items")
		Expect(methods.XListType).To(HaveValue(Equal("set")), "methods list type")
		Expect(methods.Items).NotTo(BeNil(), "methods has no item schema")
		Expect(methods.Items.Schema).NotTo(BeNil(), "methods has no item schema")
		Expect(enumStrings(methods.Items.Schema.Enum)).To(Equal([]string{"*", "DELETE", "GET", "PATCH", "POST"}),
			"methods enum")

		Expect(overrides.XListType).To(HaveValue(Equal("atomic")), "responseDelayOverrides list type")
		Expect(bmc.Required).To(BeEmpty(), "all bmc fields are optional")
	})

	It("enumerates override names from the mock's route table", func() {
		Expect(scalev1alpha1.MockRedfishOperations).To(HaveLen(35),
			"MockRedfishOperations should hold the 35 operations of the pinned mock")
		got := enumStrings(delayOverrideItemSchema().Properties["name"].Enum)
		Expect(got).To(Equal(slices.Sorted(maps.Keys(scalev1alpha1.MockRedfishOperations))),
			"responseDelayOverrides[].name enum")
	})

	It("restricts override methods to those the named operation serves", func() {
		rule := "!has(self.methods) || self.methods.all(m, m == '*' || m in " +
			mockRedfishOperationsCEL() + "[self.name])"
		Expect(hasValidationRule(delayOverrideItemSchema().XValidations, rule,
			"methods must be served by the named Redfish operation")).To(BeTrue(),
			"responseDelayOverrides[] has no methods rule matching MockRedfishOperations; want rule:\n%s", rule)
	})
})
