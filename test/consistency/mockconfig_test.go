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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	mockconfig "github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/config"
	"github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

const dpfModule = "github.com/nvidia/doca-platform"

var mockRouteLine = regexp.MustCompile(`\{"([^"]+)",\s*http\.Method(\w+),`)

// upstreamRoutes reads the route table the mock keeps unexported, from the
// DPF module source this package is built against.
func upstreamRoutes() map[string][]string {
	GinkgoHelper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", dpfModule).Output()
	Expect(err).NotTo(HaveOccurred(), "go list -m %s", dpfModule)
	path := filepath.Join(strings.TrimSpace(string(out)), "test", "mock", "dpuagent", "pkg", "redfish", "routes.go")
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), "read upstream routes.go")
	routes := map[string][]string{}
	for _, m := range mockRouteLine.FindAllStringSubmatch(string(data), -1) {
		method := strings.ToUpper(m[2])
		if !slices.Contains(routes[m[1]], method) {
			routes[m[1]] = append(routes[m[1]], method)
		}
	}
	Expect(routes).NotTo(BeEmpty(), "parsed no routes from upstream routes.go; the route table format changed")
	return routes
}

var _ = Describe("Mock dpuagent", func() {
	// The controller's tests import the mock from the DPF module go.mod
	// requires; the guest runs the binary hack/dpuagent/build.sh builds from
	// mock.ref. Both must be the same commit, or the tests vouch for code the
	// guest never runs.
	It("pins mock.ref to the DPF module go.mod requires", func() {
		gomod, err := modfile.Parse("go.mod", []byte(readRepoFile("go.mod")), nil)
		Expect(err).NotTo(HaveOccurred(), "parse go.mod")
		var version string
		for _, r := range gomod.Require {
			if r.Mod.Path == dpfModule {
				version = r.Mod.Version
			}
		}
		rev, err := module.PseudoVersionRev(version)
		Expect(err).NotTo(HaveOccurred(),
			"go.mod requires %s %q, which is not a pseudo-version of a commit", dpfModule, version)
		ref := strings.TrimSpace(readRepoFile("hack/dpuagent/mock.ref"))
		Expect(ref).To(HavePrefix(rev),
			"hack/dpuagent/mock.ref is %s but go.mod requires %s at %s; bump both together", ref, dpfModule, rev)
	})

	It("lists exactly the Redfish operations upstream routes", func() {
		upstream := upstreamRoutes()
		names := slices.Sorted(maps.Keys(upstream))
		Expect(slices.Sorted(maps.Keys(scalev1alpha1.MockRedfishOperations))).To(Equal(names),
			"MockRedfishOperations names")
		var problems []string
		for _, name := range names {
			got := slices.Sorted(slices.Values(scalev1alpha1.MockRedfishOperations[name]))
			want := slices.Sorted(slices.Values(upstream[name]))
			if !slices.Equal(got, want) {
				problems = append(problems, fmt.Sprintf("MockRedfishOperations[%q] = %v, upstream serves %v", name, got, want))
			}
		}
		Expect(problems).To(BeEmpty())
	})

	// The source scan above is only as good as its regexp, so hold the table
	// to what the mock's own startup check accepts as well.
	It("accepts exactly the MockRedfishOperations overrides", func() {
		accepts := func(o mockconfig.ResponseDelayOverride) bool {
			_, err := redfish.NewServer(nil, nil, redfish.WithResponseDelayOverrides([]mockconfig.ResponseDelayOverride{o}))
			return err == nil
		}
		var problems []string
		for name, served := range scalev1alpha1.MockRedfishOperations {
			for _, method := range []string{"GET", "PATCH", "POST", "DELETE"} {
				want := slices.Contains(served, method)
				if got := accepts(mockconfig.ResponseDelayOverride{Name: name, Methods: []string{method}}); got != want {
					problems = append(problems, fmt.Sprintf("mock accepts %s %s = %v, MockRedfishOperations says %v",
						name, method, got, want))
				}
			}
		}
		if accepts(mockconfig.ResponseDelayOverride{Name: "NotAnOperation"}) {
			problems = append(problems, "mock accepts an unknown operation name; the probe no longer tells anything")
		}
		Expect(problems).To(BeEmpty())
	})
})
