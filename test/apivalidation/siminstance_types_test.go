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
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func loadSimInstanceCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases", "scale.dpu.nvidia.com_siminstances.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read SimInstance CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal SimInstance CRD: %v", err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("SimInstance CRD has no versions")
	}
	if crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatal("SimInstance CRD has no schema")
	}
	return &crd
}

func TestSimInstanceShortName(t *testing.T) {
	crd := loadSimInstanceCRD(t)
	if !slices.Contains(crd.Spec.Names.ShortNames, "smi") {
		t.Fatalf("shortNames = %v, want smi", crd.Spec.Names.ShortNames)
	}
}

// The ordinal and host index are the instance's identity: the name, the
// children, and the DPUNode grouping all derive from them, so a changed value
// would silently point the instance at someone else's inventory.
func TestSimInstanceSpecIdentityIsRequiredAndImmutable(t *testing.T) {
	crd := loadSimInstanceCRD(t)
	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		t.Fatal("SimInstance CRD schema is missing spec")
	}
	for _, field := range []string{"ordinal", "hostIndex"} {
		t.Run(field, func(t *testing.T) {
			if !slices.Contains(spec.Required, field) {
				t.Fatalf("spec.required = %v, want %s", spec.Required, field)
			}
			prop, ok := spec.Properties[field]
			if !ok {
				t.Fatalf("spec schema is missing %s", field)
			}
			if prop.Minimum == nil || *prop.Minimum != 0 {
				t.Fatalf("%s minimum = %v, want 0", field, prop.Minimum)
			}
			if !hasValidationRule(prop.XValidations, "self == oldSelf", field+" is immutable") {
				t.Fatalf("%s is missing the immutability rule: %v", field, prop.XValidations)
			}
		})
	}
}

func TestSimInstanceStatusSubresourceEnabled(t *testing.T) {
	crd := loadSimInstanceCRD(t)
	sub := crd.Spec.Versions[0].Subresources
	if sub == nil || sub.Status == nil {
		t.Fatal("SimInstance CRD does not enable the status subresource")
	}
}

func TestSimInstancePrintColumns(t *testing.T) {
	crd := loadSimInstanceCRD(t)
	var got []string
	for _, c := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		got = append(got, c.Name)
	}
	want := []string{"Ordinal", "Host", "Phase", "Ready", "Age"}
	if !slices.Equal(got, want) {
		t.Fatalf("print columns = %v, want %v", got, want)
	}
}
