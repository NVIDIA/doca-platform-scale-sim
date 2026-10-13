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
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func simFleetCRDPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases", "scale.dpu.nvidia.com_simfleets.yaml")
}

func loadSimFleetCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(simFleetCRDPath(t))
	if err != nil {
		t.Fatalf("read SimFleet CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal SimFleet CRD: %v", err)
	}
	return &crd
}

func simFleetSpecSchema(t *testing.T) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	crd := loadSimFleetCRD(t)
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("SimFleet CRD has no versions")
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if schema == nil || schema.Properties == nil {
		t.Fatal("SimFleet CRD schema is missing properties")
	}
	spec, ok := schema.Properties["spec"]
	if !ok {
		t.Fatal("SimFleet CRD schema is missing spec")
	}
	return &spec
}

func simFleetStatusSchema(t *testing.T) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	crd := loadSimFleetCRD(t)
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("SimFleet CRD has no versions")
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if schema == nil || schema.Properties == nil {
		t.Fatal("SimFleet CRD schema is missing properties")
	}
	status, ok := schema.Properties["status"]
	if !ok {
		t.Fatal("SimFleet CRD schema is missing status")
	}
	return &status
}

func hasValidationRule(rules []apiextensionsv1.ValidationRule, rule, message string) bool {
	for _, r := range rules {
		if r.Rule == rule && r.Message == message {
			return true
		}
	}
	return false
}

func propertyDefaultRaw(props map[string]apiextensionsv1.JSONSchemaProps, name string) ([]byte, bool) {
	p, ok := props[name]
	if !ok || p.Default == nil {
		return nil, false
	}
	return p.Default.Raw, true
}
