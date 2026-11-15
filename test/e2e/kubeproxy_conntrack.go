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
	"fmt"
	"strings"
)

// disableKubeProxyConntrack sets the conntrack limits kube-proxy would otherwise
// write into nf_conntrack_max. That sysctl is read-only in a DPU Pod network
// namespace, and a failed write makes kube-proxy exit. Zero leaves the kernel
// value alone. Other null fields in the config stay as they are.
func disableKubeProxyConntrack(config string) (string, error) {
	lines := strings.Split(config, "\n")
	inBlock := false
	sawMax, sawMin := false, false
	for i, line := range lines {
		if line == "conntrack:" {
			inBlock = true
			continue
		}
		if inBlock && (line == "" || !strings.HasPrefix(line, " ")) {
			inBlock = false
		}
		if !inBlock {
			continue
		}
		switch {
		case strings.HasPrefix(line, "  maxPerCore:"):
			lines[i] = "  maxPerCore: 0"
			sawMax = true
		case strings.HasPrefix(line, "  min:"):
			lines[i] = "  min: 0"
			sawMin = true
		}
	}
	if !sawMax || !sawMin {
		return "", fmt.Errorf("kube-proxy conntrack block is missing maxPerCore or min")
	}
	return strings.Join(lines, "\n"), nil
}
