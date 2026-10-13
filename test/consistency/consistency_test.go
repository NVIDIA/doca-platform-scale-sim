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

// Package consistency guards the facts that are spelled out in more than one
// file in this repository: the Kubernetes version baked into the simfleet-node
// containerDisk, the shape of the release rules, and the tooling the CI jobs
// are allowed to assume. None of it is Go code, but this is the only test
// runner the pipeline has, and a drifted literal is exactly the kind of
// mistake that only shows up when a guest boots with the wrong kubelet.
package consistency

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Could not resolve the repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("Resolved %q as the repository root, but it has no go.mod: %v", root, err)
	}
	return root
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	if err != nil {
		t.Fatalf("Could not read %s: %v", path, err)
	}
	return string(data)
}

func TestManagerRBACCoversCredentialDelegation(t *testing.T) {
	data := readRepoFile(t, "config/rbac/role.yaml")
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal([]byte(data), role); err != nil {
		t.Fatalf("Could not decode manager Role: %v", err)
	}

	// Kubernetes refuses to create a Role granting more than its creator
	// holds, so every verb the per-ordinal Role delegates has to appear here.
	required := []struct {
		group    string
		resource string
		verbs    []string
	}{
		{group: "", resource: "secrets", verbs: []string{"get"}},
		{group: "", resource: "serviceaccounts", verbs: []string{"create", "delete", "get", "patch"}},
		{group: "", resource: "serviceaccounts/token", verbs: []string{"create"}},
		{group: "events.k8s.io", resource: "events", verbs: []string{"create", "patch"}},
		{group: "provisioning.dpu.nvidia.com", resource: "dpus", verbs: []string{"delete", "get", "patch"}},
		{group: "kubevirt.io", resource: "virtualmachineinstances", verbs: []string{"delete", "patch"}},
		{group: rbacv1.GroupName, resource: "roles", verbs: []string{"create", "delete", "get", "list", "patch"}},
		{group: rbacv1.GroupName, resource: "rolebindings", verbs: []string{"create", "delete", "get", "patch"}},
	}
	for _, permission := range required {
		for _, verb := range permission.verbs {
			if !clusterRoleAllows(role.Rules, permission.group, permission.resource, verb) {
				t.Errorf("manager Role lacks %s %s/%s required for per-DPU credentials",
					verb, permission.group, permission.resource)
			}
		}
	}
	// The guest credential no longer patches DPU status or reads the trust
	// bundle, so the manager has no reason to hold either. Keeping a
	// delegation the guest cannot use is how a narrowed Role quietly widens
	// again.
	for _, withdrawn := range []struct {
		group    string
		resource string
		verb     string
	}{
		{group: "", resource: "configmaps", verb: "get"},
		{group: "provisioning.dpu.nvidia.com", resource: "dpus/status", verb: "patch"},
	} {
		if clusterRoleAllows(role.Rules, withdrawn.group, withdrawn.resource, withdrawn.verb) {
			t.Errorf("manager Role still grants %s %s/%s, which nothing delegates any more",
				withdrawn.verb, withdrawn.group, withdrawn.resource)
		}
	}
	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			if verb == "escalate" || verb == "bind" {
				t.Errorf("manager Role must not grant %q", verb)
			}
		}
	}
	// The credential kinds are cached through an informer scoped to the
	// fleet label, which needs list and watch.
	for _, resource := range []string{"serviceaccounts", "roles", "rolebindings"} {
		for _, verb := range []string{"list", "watch"} {
			if !clusterRoleAllows(role.Rules, credentialResourceGroup(resource), resource, verb) {
				t.Errorf("manager Role lacks %s on label-cached resource %s", verb, resource)
			}
		}
	}
}

func clusterRoleAllows(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) &&
			slices.Contains(rule.Resources, resource) &&
			slices.Contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func credentialResourceGroup(resource string) string {
	if resource == "serviceaccounts" {
		return ""
	}
	return rbacv1.GroupName
}

// watchRegistration matches each type handed to For, Owns, or Watches in
// SetupWithManager, capturing the Go type name.
var watchRegistration = regexp.MustCompile(`(?:For|Owns|Watches)\(\s*&(?:\w+)\.(\w+)\{\}`)

// watchedTypeResources names the RBAC rule behind every type the controller is
// allowed to watch. An unlisted type fails the test on purpose: adding a watch
// is exactly the moment to check its RBAC.
var watchedTypeResources = map[string]struct{ group, resource string }{
	"VirtualMachine":         {"kubevirt.io", "virtualmachines"},
	"VirtualMachineInstance": {"kubevirt.io", "virtualmachineinstances"},
	"Secret":                 {"", "secrets"},
	"DPU":                    {"provisioning.dpu.nvidia.com", "dpus"},
	"DPUDevice":              {"provisioning.dpu.nvidia.com", "dpudevices"},
	"DPUNode":                {"provisioning.dpu.nvidia.com", "dpunodes"},
	"ServiceAccount":         {"", "serviceaccounts"},
	"Role":                   {rbacv1.GroupName, "roles"},
	"RoleBinding":            {rbacv1.GroupName, "rolebindings"},
	"SimInstance":            {"scale.dpu.nvidia.com", "siminstances"},
	"SimFleet":               {"scale.dpu.nvidia.com", "simfleets"},
}

// TestWatchedResourcesHaveListAndWatchRBAC catches a watch the manager cannot
// actually run. Owns and Watches both build a cache informer, which LISTs and
// WATCHes cluster-wide before the controller starts; without those verbs the
// cache never syncs and the manager hangs at startup without ever reconciling.
// envtest cannot catch it because its client is cluster-admin, so the only
// cheap guard is reading the manifest the deployment actually uses.
//
// Every reconciler's SetupWithManager is checked, since each one's watches
// run in the same manager under the same Role.
func TestWatchedResourcesHaveListAndWatchRBAC(t *testing.T) {
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal([]byte(readRepoFile(t,
		"config/rbac/role.yaml")), role); err != nil {
		t.Fatalf("Could not decode manager Role: %v", err)
	}

	for file, reconciler := range map[string]string{
		"simfleet_controller.go":    "SimFleetReconciler",
		"siminstance_controller.go": "SimInstanceReconciler",
	} {
		source := readRepoFile(t, "internal/controller/"+file)
		start := strings.Index(source, "func (r *"+reconciler+") SetupWithManager")
		if start < 0 {
			t.Fatalf("%s has no %s.SetupWithManager", file, reconciler)
		}
		matches := watchRegistration.FindAllStringSubmatch(source[start:], -1)
		if len(matches) == 0 {
			t.Fatalf("%s.SetupWithManager registers no watches; the regexp or the body changed", reconciler)
		}
		for _, match := range matches {
			typeName := match[1]
			rule, known := watchedTypeResources[typeName]
			if !known {
				t.Errorf("%s.SetupWithManager watches %s, which has no known RBAC rule; "+
					"add it to watchedTypeResources after granting list and watch", reconciler, typeName)
				continue
			}
			for _, verb := range []string{"list", "watch"} {
				if !clusterRoleAllows(role.Rules, rule.group, rule.resource, verb) {
					t.Errorf("%s.SetupWithManager watches %s but the manager Role does not grant "+
						"%s %s/%s; the cache would never sync", reconciler, typeName, verb, rule.group, rule.resource)
				}
			}
		}
	}
}
