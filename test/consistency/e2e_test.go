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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func makeTarget(makefile, name string) string {
	GinkgoHelper()
	target := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `:[^\n]*(?:\n\t[^\n]*)*`)
	matches := target.FindAllString(makefile, -1)
	if len(matches) != 1 {
		Fail(fmt.Sprintf("Makefile has %d %s target definitions, want exactly 1", len(matches), name))
	}
	return matches[0]
}

func makeTargetParts(target string) ([]string, []string) {
	GinkgoHelper()
	lines := strings.Split(target, "\n")
	header, _, _ := strings.Cut(lines[0], "##")
	_, prerequisiteText, found := strings.Cut(header, ":")
	if !found {
		Fail(fmt.Sprintf("Make target has no colon: %q", lines[0]))
	}
	recipes := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		recipe := strings.TrimSpace(line)
		if strings.HasPrefix(recipe, "#") {
			continue
		}
		recipes = append(recipes, recipe)
	}
	return strings.Fields(prerequisiteText), recipes
}

// mockDPUAgentArtifact is the one path hack/dpuagent/build.sh writes. Every
// consumer has to name that exact path: a Makefile pointing somewhere else
// fails a clean checkout, and on a machine that still holds the artifact
// under an older name it does something worse, which is succeed with bytes
// nobody built this round.
const mockDPUAgentArtifact = "bin/mock-dpuagent"

// kubevirtPatchFormat matches the single-quoted printf format string each
// KubeVirt installer uses to build its `kubectl patch -p` payload. Keeping the
// payload in one printf is what makes it extractable, and extracting it is
// what lets this test parse it as JSON instead of grepping for substrings.
var kubevirtPatchFormat = regexp.MustCompile(`'(\{"spec":\{"configuration":.*?)'`)

// kubevirtPatch renders an installer's patch payload the way bash would, with
// a representative developerConfiguration substituted for the script's
// ${dev_cfg}. No shell or interpreter is involved, so the test is hermetic.
func kubevirtPatch(path, devCfg string) string {
	GinkgoHelper()
	content := readRepoFile(path)
	matches := kubevirtPatchFormat.FindAllStringSubmatch(content, -1)
	if len(matches) != 1 {
		Fail(fmt.Sprintf("%s has %d KubeVirt patch payloads, want exactly 1; "+
			"build it with a single printf so it can be validated here", path, len(matches)))
	}
	format := matches[0][1]
	// A stray verb would make the rendered payload depend on argument order,
	// and printf would reuse the format for leftover arguments.
	if got := strings.Count(format, "%"); got != 1 || !strings.Contains(format, "%s") {
		Fail(fmt.Sprintf("%s patch format %q must contain exactly one verb, %%s", path, format))
	}
	return strings.Replace(format, "%s", devCfg, 1)
}

var _ = Describe("E2E Makefile", func() {
	// make expands an undefined variable to nothing, so a missing KIND turns
	// `$(KIND) delete cluster` into `delete cluster` and hands every hack script
	// an empty KIND= instead of failing.
	It("defines every variable and target the e2e and deploy paths use", func() {
		cmd := exec.Command("make", "--no-print-directory", "--warn-undefined-variables", "-n",
			"test-e2e", "deploy", "undeploy", "kind")
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "make -n failed:\n%s", out)
		var problems []string
		for line := range strings.SplitSeq(string(out), "\n") {
			if strings.Contains(line, "warning: undefined variable") {
				problems = append(problems, line)
			}
		}
		Expect(problems).To(BeEmpty())
	})

	// controller-gen, unlike the go tool, descends into dot directories, so
	// ./... picks up the doca-platform checkout in .cache/ and puts DPF's CRDs
	// and RBAC into the chart.
	It("keeps controller-gen out of the doca-platform checkout", func() {
		makefile := readRepoFile("Makefile")
		Expect(makefile).NotTo(ContainSubstring(`paths="./..."`))
		Expect(makefile).To(MatchRegexp(`(?m)^CONTROLLER_GEN_PATHS \?= \./api/\.\.\.;\./cmd/\.\.\.;\./internal/\.\.\.$`))
	})

	// This keeps `make test-e2e E2E_NODE_IMG=...` from satisfying the suite's
	// image assertion with a differently versioned guest.
	It("does not let the e2e node image be overridden", func() {
		makefile := readRepoFile("Makefile")
		var problems []string
		if strings.Contains(makefile, "E2E_NODE_IMG ?=") || strings.Contains(makefile, "E2E_NODE_IMG =") {
			problems = append(problems,
				"E2E_NODE_IMG must be declared with `override ... :=` so a command-line value cannot replace it")
		}
		if !strings.Contains(makefile, "override E2E_NODE_IMG := ") {
			problems = append(problems, "E2E_NODE_IMG is no longer declared with `override ... :=`")
		}
		Expect(problems).To(BeEmpty())
	})

	It("starts every e2e run fresh", func() {
		makefile := readRepoFile("Makefile")
		var problems []string
		orchestrator := makeTarget(makefile, "test-e2e")
		prerequisites, recipes := makeTargetParts(orchestrator)
		if want := []string{"check-e2e-host", "kind"}; !slices.Equal(prerequisites, want) {
			problems = append(problems, fmt.Sprintf(
				"test-e2e prerequisites = %v, want %v; setup belongs behind the recursive-make boundary",
				prerequisites, want))
		}
		if want := []string{"$(MAKE) cleanup-test-e2e", "$(MAKE) test-e2e-run"}; !slices.Equal(recipes, want) {
			problems = append(problems, fmt.Sprintf(
				"test-e2e recipes = %v, want %v; recursive cleanup must finish before the suite starts under -j",
				recipes, want))
		}

		suite := makeTarget(makefile, "test-e2e-run")
		suitePrerequisites, suiteRecipes := makeTargetParts(suite)
		for _, required := range []string{
			"check-e2e-host",
			"setup-test-e2e",
			"deploy-e2e-dpf",
			"deploy-e2e-kubevirt",
			"manifests",
			"generate",
			"fmt",
			"vet",
		} {
			if !slices.Contains(suitePrerequisites, required) {
				problems = append(problems, fmt.Sprintf(
					"test-e2e-run prerequisites %v do not contain %q; standalone setup and deploy targets must remain reusable",
					suitePrerequisites, required))
			}
		}
		if slices.Contains(suitePrerequisites, "cleanup-test-e2e") {
			problems = append(problems, fmt.Sprintf(
				"test-e2e-run prerequisites %v contain cleanup-test-e2e; cleanup must complete before setup starts",
				suitePrerequisites))
		}
		for _, recipe := range suiteRecipes {
			if strings.HasPrefix(recipe, "#") {
				problems = append(problems, fmt.Sprintf("test-e2e-run recipe parsing retained Make comment %q", recipe))
			}
		}
		suiteRecipe := strings.Join(suiteRecipes, "\n")
		for _, required := range []string{
			"dpuagent-build",
			"node-image-build-cached",
			"DPUAGENT_BIN=$(E2E_DPUAGENT_BIN)",
			"load docker-image $(E2E_NODE_IMG)",
			"go test -tags=e2e ./test/e2e/",
		} {
			if !strings.Contains(suiteRecipe, required) {
				problems = append(problems, fmt.Sprintf(
					"test-e2e-run recipes do not contain %q; the existing suite body must stay behind the reset",
					required))
			}
		}
		agentIdx := strings.Index(suiteRecipe, "dpuagent-build")
		imageIdx := strings.Index(suiteRecipe, "node-image-build-cached")
		if agentIdx < 0 || imageIdx < 0 || agentIdx > imageIdx {
			problems = append(problems, "test-e2e-run must build dpuagent before node-image-build-cached")
		}
		Expect(suiteRecipes).NotTo(BeEmpty(), "test-e2e-run has no recipes; the suite body was dropped")
		if got := suiteRecipes[len(suiteRecipes)-1]; got != "$(MAKE) cleanup-test-e2e" {
			problems = append(problems, fmt.Sprintf(
				"test-e2e-run final recipe = %q, want successful cleanup via $(MAKE) cleanup-test-e2e", got))
		}
		Expect(problems).To(BeEmpty())
	})

	// Pod Security admission only checks a pod when it is created. Labelling
	// the namespace after the chart has created the controller pod passes a
	// chart that violates the restricted profile.
	It("enforces the restricted pod security profile before deploying", func() {
		_, recipes := makeTargetParts(makeTarget(readRepoFile("Makefile"), "test-e2e-run"))
		label, deploy := -1, -1
		for i, recipe := range recipes {
			if strings.Contains(recipe, "pod-security.kubernetes.io/enforce=restricted") && label < 0 {
				label = i
			}
			if strings.HasPrefix(recipe, "$(MAKE) deploy ") && deploy < 0 {
				deploy = i
			}
		}
		Expect(label).To(BeNumerically(">=", 0), "test-e2e-run does not label the namespace restricted")
		Expect(deploy).To(BeNumerically(">=", 0), "test-e2e-run does not run $(MAKE) deploy")
		Expect(label).To(BeNumerically("<", deploy),
			"test-e2e-run labels the namespace restricted after deploying the controller")
	})

	It("names one mock-dpuagent artifact path everywhere", func() {
		var problems []string
		// Each pattern captures the repo-relative path its file resolves to,
		// so this compares the value a build would use rather than merely
		// checking that some variable is threaded through.
		sources := []struct {
			path    string
			pattern string
		}{
			{
				path:    "hack/dpuagent/build.sh",
				pattern: `(?m)^OUT="\$\{DPUAGENT_OUT:-\$ROOT/([^"}]+)\}"`,
			},
			{
				path:    "Makefile",
				pattern: `(?m)^E2E_DPUAGENT_BIN\s*:?=\s*\$\(CURDIR\)/(\S+)`,
			},
			{
				path:    "images/simfleet-node/Makefile",
				pattern: `(?m)^DPUAGENT_BIN\s*\?=\s*\$\(CURDIR\)/\.\./\.\./(\S+)`,
			},
			{
				path:    "images/simfleet-node/lib.sh",
				pattern: `\$\{SIMFLEET_RECIPE_DIR:-\$\{ROOT\}\}/\.\./\.\./([^"}]+)\}`,
			},
		}
		for _, source := range sources {
			matches := regexp.MustCompile(source.pattern).FindStringSubmatch(readRepoFile(source.path))
			if matches == nil {
				problems = append(problems, fmt.Sprintf("%s does not declare a mock-dpuagent artifact path", source.path))
				continue
			}
			if matches[1] != mockDPUAgentArtifact {
				problems = append(problems, fmt.Sprintf("%s resolves the mock-dpuagent artifact to %q, want %q",
					source.path, matches[1], mockDPUAgentArtifact))
			}
		}

		// The built artifact is a build output, so it stays out of the tree.
		// An ignore entry left on the old name would also leave the real one
		// showing up as untracked noise on every build.
		gitignore := readRepoFile(".gitignore")
		if !slices.Contains(strings.Split(gitignore, "\n"), "/"+mockDPUAgentArtifact) {
			problems = append(problems, fmt.Sprintf(".gitignore does not ignore /%s:\n%s", mockDPUAgentArtifact, gitignore))
		}
		Expect(problems).To(BeEmpty())
	})

	// The management token a guest joins with lasts 86400 seconds and nothing
	// renews it: cloud-init data is create-only, so a guest that has not read
	// its join Secret by then never will. A suite allowed to run past that
	// would fail on expired credentials and look like a provisioning bug.
	It("keeps the e2e timeout inside the management token lifetime", func() {
		const tokenLifetime = 86400 * time.Second

		issuer := readRepoFile("internal/dpuagent/credentials.go")
		lifetime := regexp.MustCompile(`ExpirationSeconds: ptr\.To\[int64\]\((\d+)\)`).
			FindStringSubmatch(issuer)
		Expect(lifetime).NotTo(BeNil(), "credentials.go does not set a token ExpirationSeconds")
		seconds, err := strconv.Atoi(lifetime[1])
		Expect(err).NotTo(HaveOccurred(), "parse token ExpirationSeconds %q", lifetime[1])
		Expect(time.Duration(seconds)*time.Second).To(Equal(tokenLifetime),
			"management token lifetime changed; Task 5 locks this value")

		makefile := readRepoFile("Makefile")
		declared := regexp.MustCompile(`(?m)^E2E_TEST_TIMEOUT\s*\?=\s*(\S+)`).
			FindStringSubmatch(makefile)
		Expect(declared).NotTo(BeNil(), "Makefile does not declare E2E_TEST_TIMEOUT")
		timeout, err := time.ParseDuration(declared[1])
		Expect(err).NotTo(HaveOccurred(), "parse E2E_TEST_TIMEOUT %q", declared[1])
		Expect(timeout).To(BeNumerically("<", tokenLifetime),
			"E2E_TEST_TIMEOUT = %s, which is not inside the %s management token lifetime", timeout, tokenLifetime)
	})
})

var _ = Describe("E2E suite", func() {
	// This keeps the suite from racing the second guest. DPF only creates a
	// DPU after selection labels are on the device, and those labels are
	// published with spec.bmcIP, so expecting replicas DPUs after only
	// ordinal 0 is selectable either flakes or, with an unscoped list, passes
	// on leftover objects from a reused Kind cluster.
	It("waits for every device before the DPUSet", func() {
		e2e := readRepoFile("test/e2e/e2e_test.go")
		agent := readRepoFile("test/e2e/e2e_agent.go")
		var problems []string

		if !regexp.MustCompile(
			`By\("waiting for every guest BMC address to make its device selectable"\)[\s\S]+?for ordinal := range replicas[\s\S]+?guestAgentTimeout\(\)`,
		).MatchString(e2e) {
			problems = append(problems, "e2e does not wait for every replica's BMC address before the DPUSet")
		}

		section := func(start, end string) string {
			i := strings.Index(e2e, start)
			if i < 0 {
				problems = append(problems, fmt.Sprintf("e2e is missing %q", start))
				return ""
			}
			rest := e2e[i:]
			if end == "" {
				return rest
			}
			j := strings.Index(rest[len(start):], end)
			if j < 0 {
				problems = append(problems, fmt.Sprintf("e2e is missing %q after %q", end, start))
				return rest
			}
			return rest[:len(start)+j]
		}
		if !strings.Contains(section(
			`By("waiting for DPUSet to create DPUs")`,
			`By("waiting for DPUsObserved")`,
		), "guestAgentTimeout()") {
			problems = append(problems, "e2e waits for DPUSet to create DPUs with the default Eventually timeout")
		}
		if !strings.Contains(section(
			`By("waiting for DPUsObserved")`,
			`By("waiting for the Kamaji DPUCluster")`,
		), "guestAgentTimeout()") {
			problems = append(problems, "e2e waits for DPUsObserved with the default Eventually timeout")
		}

		if strings.Contains(e2e, `listedNames("dpu", fleetNamespace, "")`) ||
			strings.Contains(agent, `listedNames("dpu", fleetNamespace, "")`) {
			problems = append(problems, "e2e lists every DPU in the operator namespace; "+
				"leftover objects on a reused Kind cluster would satisfy HaveLen(replicas)")
		}
		if !strings.Contains(e2e, "fleetDPUName(") {
			problems = append(problems, "e2e does not address DPUs by the names DPF derives from this fleet")
		}
		Expect(problems).To(BeEmpty())
	})

	// This keeps a TCG host from being given the KVM budget. kubectl failing
	// to read useEmulation used to return false, which is the KVM answer, and
	// the suite then looked like a product failure 15 minutes early.
	It("does not assume KVM when the KubeVirt query fails", func() {
		e2e := readRepoFile("test/e2e/e2e_test.go")
		fn := regexp.MustCompile(`(?ms)func softwareEmulationEnabled\(\) bool \{.*?^\}`)
		body := fn.FindString(e2e)
		Expect(body).NotTo(BeEmpty(), "e2e_test.go has no softwareEmulationEnabled")
		var problems []string
		if regexp.MustCompile(`if err != nil \{\s*return false\s*\}`).MatchString(body) {
			problems = append(problems, "softwareEmulationEnabled treats a kubectl error as KVM")
		}
		if !strings.Contains(body, "NotTo(HaveOccurred()") {
			problems = append(problems, "softwareEmulationEnabled does not fail the spec when it cannot read useEmulation")
		}
		Expect(problems).To(BeEmpty())
	})
})

var _ = Describe("Node image", func() {
	// A disabled spec.dpuAgent stops the two baked units by masking them, and
	// whether that works at all depends on two files in different trees
	// agreeing: where the image recipe installs the units, and which mask
	// form the controller renders into cloud-init. systemd searches /etc,
	// then /run, then /usr/lib, and picks the first hit.
	//
	// Neither suite alone can see the mismatch. The image tests do not know
	// what cloud-init will run, and the controller tests do not know where
	// the units live, so both passed while a plain mask could not be created
	// (a real unit file already occupied /etc) and a --runtime mask was
	// created but shadowed (the unit file in /etc outranked /run). Either way
	// the mock started without a config and the join helper looped without a
	// credential.
	It("puts disabled agent units where they can be masked", func() {
		units := []string{"mock-dpuagent.service", "wait-kubelet-join.service"}
		var problems []string

		// The image must ship them as vendor units, leaving /etc free for the
		// mask symlink.
		//
		// This reads the --copy-in destination itself rather than looking for
		// the vendor path anywhere in the file. The chmod that follows also
		// names that path, so a looser check passes while the copy-in alone
		// is redirected to /etc, which is exactly the edit that would
		// reintroduce the bug.
		recipe := readRepoFile("images/simfleet-node/customize-disk.sh")
		for _, unit := range units {
			copyIn := regexp.MustCompile(
				`--copy-in\s+"[^"]*/guest/` + regexp.QuoteMeta(unit) + `:([^"]+)"`,
			).FindStringSubmatch(recipe)
			if copyIn == nil {
				problems = append(problems, fmt.Sprintf("customize-disk.sh does not copy %s into the guest at all", unit))
				continue
			}
			if got := copyIn[1]; got != "/usr/lib/systemd/system" {
				problems = append(problems, fmt.Sprintf(
					"customize-disk.sh installs %s into %s, want /usr/lib/systemd/system;"+
						" a unit outside the vendor path cannot be masked, and one in"+
						" /etc/systemd/system occupies the path the mask symlink needs",
					unit, got))
			}
			// The permissions step has to follow the unit to the vendor path,
			// or the shipped unit is left unreadable at the location systemd
			// uses.
			if !strings.Contains(recipe, "/usr/lib/systemd/system/"+unit) {
				problems = append(problems, fmt.Sprintf(
					"customize-disk.sh never references /usr/lib/systemd/system/%s after copying it there", unit))
			}
		}

		// And the controller must render a mask that persists, because /etc
		// is now the only location that outranks the vendor copy on every
		// boot.
		renderer := readRepoFile("internal/cloudinit/secret.go")
		mask := regexp.MustCompile(`"\s*-\s*(systemctl mask[^"]*)"`).FindStringSubmatch(renderer)
		Expect(mask).NotTo(BeNil(), "secret.go renders no systemctl mask for a disabled dpuAgent")
		if strings.Contains(mask[1], "--runtime") {
			problems = append(problems, fmt.Sprintf("secret.go renders %q, which masks only until reboot", mask[1]))
		}
		if !strings.Contains(mask[1], "--now") {
			problems = append(problems, fmt.Sprintf("secret.go renders %q, which leaves an already-started unit running", mask[1]))
		}
		Expect(problems).To(BeEmpty())
	})

	It("ships the Flannel CNI configuration", func() {
		bridge := filepath.Join(repoRoot(), "images/simfleet-node/guest/10-simfleet-bridge.conf")
		// A nil error means the file is there; only ErrNotExist is
		// acceptable, and any other stat error is itself worth failing on.
		_, err := os.Stat(bridge)
		Expect(err).To(HaveOccurred(), "fixed host-local CNI configuration still exists at %s", bridge)
		Expect(os.IsNotExist(err)).To(BeTrue(), "Could not stat %s: %v", bridge, err)

		flannel := filepath.Join(repoRoot(), "images/simfleet-node/guest/10-flannel.conflist")
		contents, err := os.ReadFile(flannel)
		Expect(err).NotTo(HaveOccurred(), "guest must ship kube-flannel's default conflist")
		var problems []string
		if !strings.Contains(string(contents), `"type": "flannel"`) {
			problems = append(problems, "10-flannel.conflist does not delegate to the flannel CNI plugin")
		}

		customize := readRepoFile("images/simfleet-node/customize-disk.sh")
		if strings.Contains(customize, "10-simfleet-bridge.conf") {
			problems = append(problems, "customize-disk.sh still installs the fixed host-local CNI configuration")
		}
		if !strings.Contains(customize, "10-flannel.conflist") {
			problems = append(problems, "customize-disk.sh does not install 10-flannel.conflist")
		}

		readme := readRepoFile("images/simfleet-node/README.md")
		if !strings.Contains(readme, "Flannel owns") {
			problems = append(problems, "node image README does not state that Flannel owns guest CNI configuration")
		}
		if !strings.Contains(readme, "10-flannel.conflist") {
			problems = append(problems, "node image README does not mention the baked 10-flannel.conflist")
		}
		Expect(problems).To(BeEmpty())
	})

	// This keeps the guest paths the image bakes in identical to the ones the
	// controller's cloud-init writes. The controller and the image are
	// versioned and reviewed apart, so a renamed path would only show up as a
	// guest that boots and never joins.
	It("ships the guest paths the agent configuration uses", func() {
		const (
			agentBinary    = "/opt/dpf/mock-dpuagent"
			joinHelper     = "/opt/dpf/wait-kubelet-join.sh"
			configPath     = "/etc/mock-dpuagent/config.yaml"
			kubeconfigPath = "/etc/dpf-sim/mgmt.kubeconfig"
			identityPath   = "/etc/dpf-sim/identity.env"
		)
		var problems []string
		for _, source := range []struct {
			path  string
			wants []string
		}{
			{
				path:  "images/simfleet-node/guest/mock-dpuagent.service",
				wants: []string{agentBinary, configPath},
			},
			{
				path:  "images/simfleet-node/guest/wait-kubelet-join.service",
				wants: []string{joinHelper},
			},
			{
				// The helper's only inputs are the two files cloud-init writes.
				path:  "images/simfleet-node/guest/wait-kubelet-join.sh",
				wants: []string{kubeconfigPath, identityPath},
			},
			{
				path:  "images/simfleet-node/verify-guest.sh",
				wants: []string{agentBinary, joinHelper},
			},
			{
				path: "internal/dpuagent/config.go",
				wants: []string{
					configPath, kubeconfigPath,
					"mock-dpuagent.service", "wait-kubelet-join.service",
				},
			},
			{
				path:  "internal/cloudinit/secret.go",
				wants: []string{identityPath},
			},
		} {
			content := readRepoFile(source.path)
			for _, want := range source.wants {
				if !strings.Contains(content, want) {
					problems = append(problems, fmt.Sprintf("%s does not reference the guest path %s", source.path, want))
				}
			}
		}
		Expect(problems).To(BeEmpty())
	})

	// This keeps the shell that builds and publishes the image free of an
	// interpreter that only happens to be present in the CI images.
	It("does not depend on python", func() {
		paths := []string{
			".github/workflows/e2e.yml",
			"images/simfleet-node/build.sh",
			"images/simfleet-node/build-cached.sh",
			"images/simfleet-node/check-host.sh",
			"images/simfleet-node/customize-disk.sh",
			"images/simfleet-node/lib.sh",
			"images/simfleet-node/source-hash.sh",
			"images/simfleet-node/test.sh",
			"images/simfleet-node/verify-guest.sh",
			"images/simfleet-node/guest/install.sh",
			"images/simfleet-node/guest/wait-kubelet-join.sh",
			"images/simfleet-node/guest/wait-kubelet-join.service",
		}
		var problems []string
		for _, path := range paths {
			if strings.Contains(readRepoFile(path), "python") {
				problems = append(problems, fmt.Sprintf("%s depends on python; use tooling the job image guarantees", path))
			}
		}
		Expect(problems).To(BeEmpty())
	})
})

var _ = Describe("Bridge networking", func() {
	It("is configured in KubeVirt, the fleets, and the e2e selectors", func() {
		// Representative of both installer branches: the emulation flag
		// differs between them, the shape does not.
		const devCfg = `{"disabledFeatureGates":["ImageVolume"],"useEmulation":true}`
		var problems []string

		for _, path := range []string{
			"hack/e2e/deploy-kubevirt.sh",
		} {
			patch := kubevirtPatch(path, devCfg)

			var rendered struct {
				Spec struct {
					Configuration struct {
						DeveloperConfiguration struct {
							DisabledFeatureGates []string `json:"disabledFeatureGates"`
							UseEmulation         *bool    `json:"useEmulation"`
						} `json:"developerConfiguration"`
						Network struct {
							PermitBridgeInterfaceOnPodNetwork *bool `json:"permitBridgeInterfaceOnPodNetwork"`
						} `json:"network"`
					} `json:"configuration"`
				} `json:"spec"`
			}
			if err := json.Unmarshal([]byte(patch), &rendered); err != nil {
				problems = append(problems, fmt.Sprintf("%s renders an invalid KubeVirt patch %q: %v", path, patch, err))
				continue
			}

			configuration := rendered.Spec.Configuration
			if configuration.Network.PermitBridgeInterfaceOnPodNetwork == nil ||
				!*configuration.Network.PermitBridgeInterfaceOnPodNetwork {
				problems = append(problems, fmt.Sprintf(
					"%s patch %q does not set network.permitBridgeInterfaceOnPodNetwork=true", path, patch))
			}
			// The bridge setting is merged into the same object as the
			// ImageVolume and emulation settings, so it must not displace
			// them.
			if !slices.Contains(configuration.DeveloperConfiguration.DisabledFeatureGates, "ImageVolume") {
				problems = append(problems, fmt.Sprintf(
					"%s patch %q drops developerConfiguration.disabledFeatureGates=[ImageVolume]", path, patch))
			}
			if configuration.DeveloperConfiguration.UseEmulation == nil {
				problems = append(problems, fmt.Sprintf("%s patch %q drops developerConfiguration.useEmulation", path, patch))
			}
		}

		for _, path := range []string{
			"config/samples/scale_v1alpha1_simfleet.yaml",
			"test/e2e/testdata/simfleet.yaml",
		} {
			content := readRepoFile(path)
			if !strings.Contains(content, "bridge: true") {
				problems = append(problems, fmt.Sprintf("%s does not request bridge pod networking", path))
			}
			if strings.Contains(content, "masquerade:") {
				problems = append(problems, fmt.Sprintf("%s still requests masquerade pod networking", path))
			}
		}

		// KubeVirt 1.9 puts the VM name on virt-launcher as
		// vm.kubevirt.io/name. kubevirt.io/domain is a pod annotation, so a
		// label selector never matches.
		e2e := readRepoFile("test/e2e/e2e_test.go")
		if strings.Contains(e2e, "kubevirt.io/domain=") {
			problems = append(problems,
				"e2e selects virt-launcher pods with kubevirt.io/domain, which is an annotation on KubeVirt 1.9")
		}
		if !strings.Contains(e2e, "vm.kubevirt.io/name=") {
			problems = append(problems, "e2e does not select virt-launcher pods with vm.kubevirt.io/name")
		}
		Expect(problems).To(BeEmpty())
	})
})
