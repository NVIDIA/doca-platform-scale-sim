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

package kubevirt

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

func discoveryWith(resources ...*metav1.APIResourceList) discovery.DiscoveryInterface {
	return &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
}

var _ = Describe("Installed", func() {
	It("reports KubeVirt installed when the cluster serves kubevirt.io/v1", func() {
		installed, err := Installed(discoveryWith(&metav1.APIResourceList{
			GroupVersion: "kubevirt.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "virtualmachines", Kind: "VirtualMachine"},
				{Name: "virtualmachineinstances", Kind: "VirtualMachineInstance"},
			},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(installed).To(BeTrue())
	})

	It("reports KubeVirt absent when the API group lacks the kinds the controller watches", func() {
		installed, err := Installed(discoveryWith(&metav1.APIResourceList{
			GroupVersion: "kubevirt.io/v1",
			APIResources: []metav1.APIResource{{Name: "virtualmachines", Kind: "VirtualMachine"}},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(installed).To(BeFalse())
	})

	It("reports KubeVirt absent when the cluster does not serve kubevirt.io/v1", func() {
		installed, err := Installed(discoveryWith(&metav1.APIResourceList{GroupVersion: "apps/v1"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(installed).To(BeFalse())
	})

	It("returns a discovery failure other than not-found instead of guessing", func() {
		failing := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
		failing.AddReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		_, err := Installed(failing)
		Expect(err).To(MatchError(ContainSubstring("boom")))
	})
})
