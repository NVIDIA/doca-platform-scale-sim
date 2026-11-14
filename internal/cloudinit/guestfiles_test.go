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

package cloudinit

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

var _ = Describe("GuestFiles", func() {
	It("returns the identity, configuration and management kubeconfig for an enabled guest", func() {
		fleet := fleetWith(nil)
		fleet.Spec.Bootstrap = &scalev1alpha1.BootstrapSpec{
			DPUClusterRef: scalev1alpha1.DPUClusterReference{Name: "tenant1", Namespace: "dpf-ns"},
		}
		const ordinal = 12
		kubeconfig := []byte("kubeconfig-body")

		files, err := GuestFiles(fleet, ordinal, kubeconfig)
		Expect(err).NotTo(HaveOccurred())

		wantConf, err := dpuagent.RenderConfig(fleet, naming.SerialNumber(fleet.Name, ordinal))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(Equal([]GuestFile{
			{Name: IdentityFileName, Path: dpuIdentityPath, Content: dpuIdentityContent(fleet, ordinal)},
			{Name: ConfigFileName, Path: dpuagent.ConfPath, Content: wantConf},
			{Name: KubeconfigFileName, Path: dpuagent.KubeconfigPath, Content: "kubeconfig-body"},
		}))
		Expect([]string{files[0].Name, files[1].Name, files[2].Name}).
			To(Equal([]string{"identity.env", "config.yaml", "mgmt.kubeconfig"}))
	})

	It("returns only the identity when the agent is disabled and ignores the kubeconfig", func() {
		fleet := fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: boolPtr(false)})

		files, err := GuestFiles(fleet, 3, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(HaveLen(1))
		Expect(files[0].Name).To(Equal(IdentityFileName))
		Expect(files[0].Content).To(Equal(dpuIdentityContent(fleet, 3)))
	})

	It("rejects a missing fleet and an empty kubeconfig for an enabled guest", func() {
		_, err := GuestFiles(nil, 0, []byte("kubeconfig"))
		Expect(err).To(HaveOccurred())
		_, err = GuestFiles(fleetWith(nil), 0, nil)
		Expect(err).To(MatchError(ContainSubstring("management kubeconfig for ordinal 0 is empty")))
	})
})

func boolPtr(b bool) *bool { return &b }
