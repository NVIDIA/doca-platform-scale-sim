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
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// kubernetesVersion is the single source of truth for this test. Bumping
	// it makes every consumer below fail until it has been updated too.
	kubernetesVersion = "v1.36.4"
	// kubeAptVersion is the pkgs.k8s.io package version for the same release.
	kubeAptVersion = "1.36.4-1.1"
	// e2eNodeImageTag is local-only; it is never published.
	e2eNodeImageTag        = "simfleet-node:e2e"
	kubernetesVersionLabel = "com.nvidia.simfleet.kubernetes.version"
)

// aptRepoMinor is the pkgs.k8s.io release channel, for example v1.36.
func aptRepoMinor() string {
	GinkgoHelper()
	parts := strings.Split(kubernetesVersion, ".")
	if len(parts) != 3 {
		Fail(fmt.Sprintf("kubernetesVersion %q is not vMAJOR.MINOR.PATCH", kubernetesVersion))
	}
	return parts[0] + "." + parts[1]
}

var _ = Describe("Kubernetes version", func() {
	// This checks every place that spells out the Kubernetes version the node
	// image ships. The patterns are built from the constants above, so a
	// partial bump fails here rather than in a guest.
	It("is updated in every consumer together", func() {
		minor := aptRepoMinor()
		aptRepo := "https://pkgs.k8s.io/core:/stable:/" + minor + "/deb/"
		var problems []string

		consumers := map[string][]string{
			"images/simfleet-node/Makefile": {
				`(?m)^NODE_IMG \?= simfleet-node:local$`,
			},
			"images/simfleet-node/build.sh": {
				`(?m)^NODE_IMG="\$\{NODE_IMG:-simfleet-node:local\}"$`,
				`SIMFLEET_KUBERNETES_VERSION_LABEL`,
				`SIMFLEET_OCI_VERSION_LABEL`,
			},
			"images/simfleet-node/lib.sh": {
				`(?m)^SIMFLEET_KUBE_APT_VERSION="` + regexp.QuoteMeta(kubeAptVersion) + `"$`,
				regexp.QuoteMeta(kubernetesVersionLabel),
				regexp.QuoteMeta("org.opencontainers.image.version"),
			},
			"images/simfleet-node/build-cached.sh": {
				`(?m)^NODE_IMG="\$\{NODE_IMG:-simfleet-node:local\}"$`,
			},
			"images/simfleet-node/guest/install.sh": {
				`(?m)^KUBE_APT_VERSION="\$\{KUBE_APT_VERSION:-` + regexp.QuoteMeta(kubeAptVersion) + `\}"$`,
				regexp.QuoteMeta(aptRepo + "Release.key"),
				regexp.QuoteMeta("signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] " + aptRepo),
			},
			"images/simfleet-node/README.md": {
				regexp.QuoteMeta("Kubernetes " + kubernetesVersion),
				regexp.QuoteMeta(kubernetesVersionLabel),
			},
			"hack/e2e/setup-kind.sh": {
				`(?m)^KIND_KUBERNETES_VERSION="\$\{KIND_KUBERNETES_VERSION:-` + regexp.QuoteMeta(kubernetesVersion) + `\}"$`,
			},
			"Makefile": {
				`(?m)^KIND_KUBERNETES_VERSION \?= ` + regexp.QuoteMeta(kubernetesVersion) + `$`,
				`(?m)^KIND_NODE_IMAGE \?= kindest/node:` + regexp.QuoteMeta(kubernetesVersion) + `@sha256:[0-9a-f]{64}$`,
				`(?m)^override E2E_NODE_IMG := ` + regexp.QuoteMeta(e2eNodeImageTag) + `$`,
			},
			".github/workflows/e2e.yml": {
				`(?m)^      KUBECTL_VERSION: ` + regexp.QuoteMeta(kubernetesVersion) + `$`,
			},
			"test/e2e/testdata/simfleet.yaml": {
				`(?m)^    image: ` + regexp.QuoteMeta(e2eNodeImageTag) + `$`,
			},
			"test/e2e/e2e_test.go": {
				regexp.QuoteMeta(`Equal("` + e2eNodeImageTag + `")`),
			},
		}

		for path, patterns := range consumers {
			content := readRepoFile(path)
			for _, pattern := range patterns {
				if !regexp.MustCompile(pattern).MatchString(content) {
					problems = append(problems, fmt.Sprintf(
						"%s does not contain a match for %q; every consumer of %s must be updated together",
						path, pattern, kubernetesVersion))
				}
			}
		}
		Expect(problems).To(BeEmpty())
	})

	// This catches the other half of a partial bump: a file that still
	// mentions an older 1.3x release.
	It("has no stray older releases", func() {
		paths := []string{
			".github/workflows/e2e.yml",
			"Makefile",
			"hack/e2e/setup-kind.sh",
			"test/e2e/e2e_test.go",
			"test/e2e/testdata/simfleet.yaml",
			"images/simfleet-node/Makefile",
			"images/simfleet-node/README.md",
			"images/simfleet-node/build.sh",
			"images/simfleet-node/build-cached.sh",
			"images/simfleet-node/lib.sh",
			"images/simfleet-node/guest/install.sh",
			"images/simfleet-node/test.sh",
		}
		// Kubernetes releases only; tool versions such as v1.9.0 or v3.18.3
		// are unrelated and must not be swept up here.
		tagged := regexp.MustCompile(`v1\.3[0-9]\.[0-9]+`)
		packaged := regexp.MustCompile(`[^0-9.]1\.3[0-9]\.[0-9]+-[0-9]+\.[0-9]+`)
		var problems []string

		for _, path := range paths {
			content := readRepoFile(path)
			for _, found := range tagged.FindAllString(content, -1) {
				if found != kubernetesVersion {
					problems = append(problems, fmt.Sprintf("%s mentions Kubernetes %s; expected only %s",
						path, found, kubernetesVersion))
				}
			}
			for _, found := range packaged.FindAllString(content, -1) {
				if version := found[1:]; version != kubeAptVersion {
					problems = append(problems, fmt.Sprintf("%s mentions package version %s; expected only %s",
						path, version, kubeAptVersion))
				}
			}
		}
		Expect(problems).To(BeEmpty())
	})
})
