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

package consistency

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	// bmcCredentialFixture is the BMC password the Kind e2e gives DPF. The
	// fleet's DPUDevices set no bmcCredentialSecretName, which sends DPF to
	// this shared Secret in the device's own namespace.
	bmcCredentialFixture = "test/e2e/testdata/bmc-shared-password.yaml"
	// bmcCredentialSecretName and bmcCredentialKey are DPF's, not ours:
	// BMCPasswordSecret and BMCSharedPasswordKey in the provisioning Redfish
	// client. Neither is configurable, so a rename here is a silent break.
	bmcCredentialSecretName = "bmc-shared-password"
	bmcCredentialKey        = "password"
	// bmcCredentialNamespace is the namespace the e2e fleet and its DPUDevices
	// live in, which is where DPF looks the Secret up. It has to be the DPF
	// operator namespace: Redfish mTLS then finds the CA bundle and
	// namespaced Issuer DPF already created there. A dedicated fleet
	// namespace leaves DPUDevices Initialized=False with
	// dpf-ca-trust-bundle not found.
	bmcCredentialNamespace = "dpf-operator-system"
)

var _ = Describe("E2E BMC credential", func() {
	// This keeps the shared BMC password in the e2e fixtures. Without it DPF
	// cannot open a Redfish session against a guest mock BMC, so it never
	// reaches the chassis read that stamps the DPU type, and every DPU waits
	// in Initializing until the suite times out fifteen minutes later with
	// nothing pointing at the missing Secret.
	It("is shipped in the e2e fixtures", func() {
		secret := &corev1.Secret{}
		Expect(yaml.Unmarshal([]byte(readRepoFile(bmcCredentialFixture)), secret)).To(Succeed(),
			"parse %s", bmcCredentialFixture)
		var problems []string

		if secret.Kind != "Secret" {
			problems = append(problems, fmt.Sprintf("%s is a %q, not a Secret", bmcCredentialFixture, secret.Kind))
		}
		if secret.Name != bmcCredentialSecretName {
			problems = append(problems, fmt.Sprintf("Secret is named %q; DPF only reads %q",
				secret.Name, bmcCredentialSecretName))
		}
		if secret.Namespace != bmcCredentialNamespace {
			problems = append(problems, fmt.Sprintf("Secret is in namespace %q; the DPUDevices are in %q",
				secret.Namespace, bmcCredentialNamespace))
		}
		if secret.StringData[bmcCredentialKey] == "" {
			problems = append(problems, fmt.Sprintf(
				"Secret has no non-empty %q; DPF rejects an empty password before contacting the BMC", bmcCredentialKey))
		}
		Expect(problems).To(BeEmpty())
	})

	// This keeps the Kind fleet in the namespace DPF already issued its
	// provisioning CA into. Kubernetes forbids a SimFleet in one namespace
	// from owning a DPUDevice in another, and DPF's Redfish client looks up
	// dpf-ca-trust-bundle and dpf-provisioning-issuer in the DPUDevice's own
	// namespace, so the whole fleet has to live there. AfterAll must not
	// delete that namespace: it is the operator's.
	It("shares the DPF operator namespace with the fleet", func() {
		suite := readRepoFile("test/e2e/e2e_test.go")
		var problems []string
		if !strings.Contains(suite, `fleetNamespace = dpfOperatorNamespace`) &&
			!strings.Contains(suite, `fleetNamespace = "`+bmcCredentialNamespace+`"`) {
			problems = append(problems, fmt.Sprintf("e2e fleetNamespace is not %s; DPF's CA bundle and Issuer live only there",
				bmcCredentialNamespace))
		}
		if strings.Contains(suite, `delete", "ns", fleetNamespace`) {
			problems = append(problems, "AfterAll deletes fleetNamespace, which is the DPF operator namespace")
		}
		if !strings.Contains(suite, `fleetLabel`) || !strings.Contains(suite, "cleanupFleetInventory") {
			problems = append(problems, "e2e cleanup is missing; a shared operator namespace cannot be wiped wholesale")
		}

		root := filepath.Join(repoRoot(), "test/e2e/testdata")
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "tenant" {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".yaml", ".tmpl":
			default:
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(body)
			if strings.Contains(text, "namespace: simfleet-e2e") {
				problems = append(problems, fmt.Sprintf(
					"%s still names namespace simfleet-e2e; DPUDevices there cannot complete mTLS", path))
			}
			if !strings.Contains(text, "namespace: "+bmcCredentialNamespace) {
				problems = append(problems, fmt.Sprintf("%s does not place objects in %s", path, bmcCredentialNamespace))
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(problems).To(BeEmpty())
	})
})
