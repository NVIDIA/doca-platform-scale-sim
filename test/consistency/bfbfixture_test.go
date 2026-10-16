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
	"os/exec"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	// bfbFixture is the trimmed BF3 BFB the Kind e2e installs. It is a binary
	// fixture, so nothing in a review shows what it is; these tests are the
	// only place that says so.
	bfbFixture = "bf3-trimmed.bfb"
	// bfbFixtureURL is what testdata/bfb.yaml asks DPF to download. The
	// provisioning controller resolves it with cluster DNS, so it has to be
	// the in-cluster Service of the fixture server.
	bfbFixtureURL = "http://bfb-fixtures.dpf-operator-system.svc.cluster.local/" + bfbFixture

	bfbFixturesManifest = "hack/e2e/bfb-fixtures.yaml"
	bfbFixturesDeploy   = "hack/e2e/deploy-bfb-fixtures.sh"
	// bfbFixtureSync stages the fixture out of the doca-platform revision
	// mock.ref pins; the fixture itself is not committed.
	bfbFixtureSync = "BFB_FIXTURE_OUT=$(BFB_FIXTURE_FILE) ./hack/dpuagent/sync-bfb-fixture.sh"
)

// fixtureServiceFromURL splits bfbFixtureURL back into the Service and
// namespace cluster DNS resolves it from.
func fixtureServiceFromURL() (string, string) {
	GinkgoHelper()
	host, _, _ := strings.Cut(strings.TrimPrefix(bfbFixtureURL, "http://"), "/")
	service, namespace, found := strings.Cut(strings.TrimSuffix(host, ".svc.cluster.local"), ".")
	if !found {
		Fail(fmt.Sprintf("bfbFixtureURL host %q is not <service>.<namespace>.svc.cluster.local", host))
	}
	return service, namespace
}

// fixtureServiceProblems decodes the manifest and checks the three things
// cluster DNS and kube-proxy need to agree on for bfbFixtureURL to reach the
// httpd: the Service name and namespace the host resolves to, a selector that
// matches the Deployment's pods, and a port that lands on the container.
func fixtureServiceProblems(manifest string) []string {
	GinkgoHelper()
	var problems []string

	host, _, _ := strings.Cut(strings.TrimPrefix(bfbFixtureURL, "http://"), "/")
	var service corev1.Service
	var deployment appsv1.Deployment
	for _, document := range strings.Split(manifest, "\n---\n") {
		var typeMeta metav1.TypeMeta
		Expect(yaml.Unmarshal([]byte(document), &typeMeta)).To(Succeed(),
			"%s has a document that is not valid YAML", bfbFixturesManifest)
		switch typeMeta.Kind {
		case "Service":
			Expect(yaml.Unmarshal([]byte(document), &service)).To(Succeed(), "Could not decode the fixture Service")
		case "Deployment":
			Expect(yaml.Unmarshal([]byte(document), &deployment)).To(Succeed(), "Could not decode the fixture Deployment")
		}
	}

	if service.Name == "" || deployment.Name == "" {
		Fail(fmt.Sprintf("%s declares Service %q and Deployment %q; both are required",
			bfbFixturesManifest, service.Name, deployment.Name))
	}
	if got := service.Name + "." + service.Namespace + ".svc.cluster.local"; got != host {
		problems = append(problems, fmt.Sprintf("the fixture Service resolves as %q, but the BFB downloads from %q", got, host))
	}

	pods := deployment.Spec.Template.Labels
	for key, value := range service.Spec.Selector {
		if pods[key] != value {
			problems = append(problems, fmt.Sprintf(
				"the fixture Service selects %s=%s, which its Deployment's pods do not carry", key, value))
		}
	}
	Expect(service.Spec.Ports).To(HaveLen(1), "the fixture Service publishes the wrong number of ports")
	// A BFB URL without a port is fetched on 80, so the Service has to be
	// there even though the container listens higher up.
	port := service.Spec.Ports[0]
	if port.Port != 80 {
		problems = append(problems, fmt.Sprintf("the fixture Service listens on %d, but the BFB URL has no port", port.Port))
	}
	containers := deployment.Spec.Template.Spec.Containers
	Expect(containers).To(HaveLen(1), "the fixture Deployment has the wrong number of containers")
	named := false
	for _, containerPort := range containers[0].Ports {
		if containerPort.Name == port.TargetPort.StrVal {
			named = true
		}
	}
	if !named {
		problems = append(problems, fmt.Sprintf(
			"the fixture Service targets port %q, which its container does not declare", port.TargetPort.StrVal))
	}
	return problems
}

// gitIndexMode returns the mode git records for path, or "" when path is not
// in the index.
func gitIndexMode(path string) string {
	GinkgoHelper()
	out, err := exec.Command("git", "-C", repoRoot(), "ls-files", "--stage", "--", path).Output()
	Expect(err).NotTo(HaveOccurred(), "git ls-files --stage %s", path)
	fields := strings.Fields(string(out))
	if len(fields) < 1 {
		return ""
	}
	return fields[0]
}

var _ = Describe("BFB fixture", func() {
	// This keeps the BFB object, the fixture server and the file on disk
	// pointing at each other. Any one of them drifting leaves the suite
	// waiting on a download that can never finish.
	It("is served in cluster", func() {
		var problems []string
		bfb := readRepoFile("test/e2e/testdata/bfb.yaml")
		if !strings.Contains(bfb, "url: "+bfbFixtureURL) {
			problems = append(problems, fmt.Sprintf("testdata/bfb.yaml does not download %s", bfbFixtureURL))
		}

		manifest := readRepoFile(bfbFixturesManifest)
		for _, want := range []string{
			"name: bfb-fixtures",
			"namespace: dpf-operator-system",
			"kind: Service",
			"kind: Deployment",
		} {
			if !strings.Contains(manifest, want) {
				problems = append(problems, fmt.Sprintf("%s does not contain %q", bfbFixturesManifest, want))
			}
		}
		// The readiness probe is the manifest's only mention of the file
		// name, and it is deliberate: a Pod that goes Ready with an unmounted
		// or renamed fixture turns into an unexplained BFB download failure
		// minutes later.
		if !strings.Contains(manifest, "path: /"+bfbFixture) {
			problems = append(problems, fmt.Sprintf("%s does not probe /%s", bfbFixturesManifest, bfbFixture))
		}
		problems = append(problems, fixtureServiceProblems(manifest)...)

		deploy := readRepoFile(bfbFixturesDeploy)
		for _, want := range []string{"configmap", "rollout status", "load docker-image"} {
			if !strings.Contains(deploy, want) {
				problems = append(problems, fmt.Sprintf("%s does not mention %q", bfbFixturesDeploy, want))
			}
		}
		// Where the server lands is not a knob. spec.url, the manifest and
		// the suite's diagnostics all name this one Service, so an override
		// would put the server somewhere nothing looks; the script has to fix
		// the same pair the URL resolves.
		service, namespace := fixtureServiceFromURL()
		for _, want := range []string{`NAMESPACE="` + namespace + `"`, `NAME="` + service + `"`} {
			if !strings.Contains(deploy, want) {
				problems = append(problems, fmt.Sprintf("%s does not fix %s; it has to match %s",
					bfbFixturesDeploy, want, bfbFixtureURL))
			}
		}
		// "BFB_FIXTURES_NAME:" is the `${VAR:-default}` form and does not
		// match BFB_FIXTURES_NAMESPACE, whose next character is S.
		for _, knob := range []string{"BFB_FIXTURES_NAMESPACE", "BFB_FIXTURES_NAME:"} {
			if strings.Contains(deploy, knob) {
				problems = append(problems, fmt.Sprintf(
					"%s still takes %s from the environment, which nothing else would follow", bfbFixturesDeploy, knob))
			}
		}
		// A tag would let the host pull a different busybox than the one this
		// harness was verified against.
		digestPin := regexp.MustCompile(`(?m)^BFB_FIXTURES_SOURCE_IMAGE \?= \S+@sha256:[0-9a-f]{64}$`)
		if !digestPin.MatchString(readRepoFile("Makefile")) {
			problems = append(problems, "the Makefile does not pin BFB_FIXTURES_SOURCE_IMAGE by digest")
		}
		Expect(problems).To(BeEmpty())
	})

	// This keeps the fixture server in front of the suite. The BFB object is
	// applied in the first seconds of the run, so the server has to be up
	// before `go test` starts, not alongside it.
	It("is deployed before the e2e run", func() {
		makefile := readRepoFile("Makefile")
		prerequisites, _ := makeTargetParts(makeTarget(makefile, "test-e2e-run"))
		target := "deploy-e2e-bfb-fixtures"
		var problems []string

		// Being a prerequisite is the ordering guarantee: Make finishes every
		// prerequisite before it runs the first recipe line, so there is no
		// position within the recipe left to check.
		if !slices.Contains(prerequisites, target) {
			problems = append(problems, fmt.Sprintf("test-e2e-run prerequisites %v do not contain %q; the fixture server "+
				"has to be serving before the suite applies the BFB", prerequisites, target))
		}

		// The prerequisite is only worth anything if the target behind it
		// still runs the script.
		_, fixtureRecipes := makeTargetParts(makeTarget(makefile, target))
		fixtureRecipe := strings.Join(fixtureRecipes, "\n")
		deployIdx := strings.Index(fixtureRecipe, "./hack/e2e/deploy-bfb-fixtures.sh")
		if deployIdx < 0 {
			problems = append(problems, fmt.Sprintf("%s recipes %v do not run ./hack/e2e/deploy-bfb-fixtures.sh",
				target, fixtureRecipes))
		}
		// The fixture is not committed, so the same target has to stage it
		// before the deploy script reads it.
		syncIdx := strings.Index(fixtureRecipe, bfbFixtureSync)
		if syncIdx < 0 {
			problems = append(problems, fmt.Sprintf("%s recipes %v do not run %s", target, fixtureRecipes, bfbFixtureSync))
		} else if deployIdx >= 0 && syncIdx > deployIdx {
			problems = append(problems, fmt.Sprintf("%s runs ./hack/e2e/deploy-bfb-fixtures.sh before %s",
				target, bfbFixtureSync))
		}
		if !regexp.MustCompile(`(?m)^BFB_FIXTURE_FILE \?= \$\(LOCALBIN\)/` + regexp.QuoteMeta(bfbFixture) + `$`).
			MatchString(makefile) {
			problems = append(problems, fmt.Sprintf("the Makefile does not declare BFB_FIXTURE_FILE ?= $(LOCALBIN)/%s",
				bfbFixture))
		}

		// The index is what CI checks out, whatever the local worktree mode is.
		if mode := gitIndexMode(bfbFixturesDeploy); mode != "100755" {
			problems = append(problems, fmt.Sprintf("%s is indexed as %s, not 100755; test-e2e-run invokes it as "+
				"./hack/e2e/deploy-bfb-fixtures.sh and CI then fails with Permission denied", bfbFixturesDeploy, mode))
		}
		Expect(problems).To(BeEmpty())
	})

	It("shares mock.ref with the mock-dpuagent build", func() {
		// Both the binary that runs in the guest and the BFB it installs come
		// out of the same doca-platform revision. Two pins would let the mock
		// BMC parse a fixture its own parser never saw.
		ref := strings.TrimSpace(readRepoFile("hack/dpuagent/mock.ref"))
		Expect(ref).To(HaveLen(40), "hack/dpuagent/mock.ref is not a full SHA")
		Expect(readRepoFile("hack/dpuagent/sync-bfb-fixture.sh")).NotTo(ContainSubstring(ref),
			"sync-bfb-fixture.sh hardcodes the revision instead of reading mock.ref")
	})
})
