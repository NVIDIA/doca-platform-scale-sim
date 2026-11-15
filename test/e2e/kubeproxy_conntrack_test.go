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

package e2e

import (
	"strings"
	"testing"
)

func TestDisableKubeProxyConntrack(t *testing.T) {
	in := "" +
		"apiVersion: kubeproxy.config.k8s.io/v1alpha1\n" +
		"kind: KubeProxyConfiguration\n" +
		"conntrack:\n" +
		"  maxPerCore: null\n" +
		"  min: null\n" +
		"  tcpBeLiberal: false\n" +
		"  tcpCloseWaitTimeout: null\n" +
		"mode: iptables\n"
	got, err := disableKubeProxyConntrack(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  maxPerCore: 0\n", "  min: 0\n", "  tcpCloseWaitTimeout: null\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "maxPerCore: null") || strings.Contains(got, "\n  min: null\n") {
		t.Fatalf("conntrack limits were left null:\n%s", got)
	}

	if _, err := disableKubeProxyConntrack("mode: iptables\n"); err == nil {
		t.Fatal("a config without a conntrack block succeeded")
	}
}
