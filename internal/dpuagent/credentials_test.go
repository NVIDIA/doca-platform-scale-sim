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

package dpuagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// credentialOrdinal is the fleet ordinal every credential test issues for. It
// sits on the second host group so a DPU name that ignored host grouping would
// be visible in the assertions.
const credentialOrdinal = 12

func credentialFleet() *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000",
			Namespace: "dpf-operator-system",
			UID:       types.UID("fleet-uid"),
		},
		Spec: scalev1alpha1.SimFleetSpec{Replicas: 3000, DevicesPerDPUNode: 10},
	}
}

// credentialDPUKey is the identity the credentials carry: the name DPF will
// give the DPU for credentialOrdinal, in the fleet namespace. No DPU object
// exists in any of these tests.
func credentialDPUKey(fleet *scalev1alpha1.SimFleet) client.ObjectKey {
	return client.ObjectKey{
		Name:      naming.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, credentialOrdinal),
		Namespace: fleet.Namespace,
	}
}

func assertFleetLabels(
	labels map[string]string,
	fleet *scalev1alpha1.SimFleet,
	ordinal string,
) {
	GinkgoHelper()
	Expect(labels).To(Equal(naming.InventoryLabels(fleet, ordinal)))
}

// tokenIssuerClient returns a client whose token subresource create is handled
// by create.
func tokenIssuerClient(
	create func(subResourceName string, obj, subResource client.Object) error,
) client.Client {
	GinkgoHelper()
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(authenticationv1.AddToScheme(scheme)).To(Succeed())
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourceCreate: func(
			_ context.Context,
			_ client.Client,
			subResourceName string,
			obj client.Object,
			subResource client.Object,
			_ ...client.SubResourceCreateOption,
		) error {
			return create(subResourceName, obj, subResource)
		},
	})
}

var _ = Describe("DPU agent credentials", func() {
	Describe("RBAC objects", func() {
		It("grants the Role least privilege", func() {
			fleet := credentialFleet()
			key := credentialDPUKey(fleet)
			role := DesiredRole(fleet, credentialOrdinal)

			Expect(key.Name).To(Equal("scale-3000-host-0001-scale-3000-device-000012"), "test fixture DPU name")
			// The credential sits in a guest for hours, so it grants exactly the two
			// reads the join helper performs and nothing more.
			wantRules := []rbacv1.PolicyRule{
				{
					APIGroups:     []string{"provisioning.dpu.nvidia.com"},
					Resources:     []string{"dpus"},
					ResourceNames: []string{key.Name},
					Verbs:         []string{"get"},
				},
				{
					APIGroups:     []string{""},
					Resources:     []string{"secrets"},
					ResourceNames: []string{naming.KubeadmJoinSecretName(key.Name)},
					Verbs:         []string{"get"},
				},
			}
			Expect(client.ObjectKeyFromObject(role)).To(Equal(key), "Role identity")
			Expect(role.Rules).To(Equal(wantRules))
			assertFleetLabels(role.Labels, fleet, "000012")
		})

		It("binds the Role to the DPU's ServiceAccount", func() {
			fleet := credentialFleet()
			key := credentialDPUKey(fleet)
			serviceAccount := DesiredServiceAccount(fleet, credentialOrdinal)
			binding := DesiredRoleBinding(fleet, credentialOrdinal)

			Expect(client.ObjectKeyFromObject(serviceAccount)).To(Equal(key), "ServiceAccount identity")
			assertFleetLabels(serviceAccount.Labels, fleet, "000012")
			Expect(client.ObjectKeyFromObject(binding)).To(Equal(key), "RoleBinding identity")
			Expect(binding.Subjects).To(Equal([]rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      key.Name,
				Namespace: key.Namespace,
			}}))
			Expect(binding.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: key.Name}))
			assertFleetLabels(binding.Labels, fleet, "000012")
		})
	})

	Describe("Management kubeconfig", func() {
		It("contains only the server, CA and service-account token", func() {
			token := []byte("service-account-token")
			ca := []byte("management-ca")
			data, err := ManagementKubeconfig(token, ca)
			Expect(err).NotTo(HaveOccurred())
			config, err := clientcmd.Load(data)
			Expect(err).NotTo(HaveOccurred(), "decode kubeconfig")
			Expect(config.Clusters).To(HaveLen(1))
			Expect(config.AuthInfos).To(HaveLen(1))
			Expect(config.Contexts).To(HaveLen(1))

			cluster := config.Clusters[config.Contexts[config.CurrentContext].Cluster]
			authInfo := config.AuthInfos[config.Contexts[config.CurrentContext].AuthInfo]
			Expect(cluster.Server).To(Equal(managementServiceURL))
			Expect(cluster.CertificateAuthorityData).To(Equal(ca))
			Expect(authInfo.Token).To(Equal(string(token)), "want supplied service-account token")
			Expect(authInfo.ClientCertificateData).To(BeNil())
			Expect(authInfo.ClientKeyData).To(BeNil())
			Expect(authInfo.Username).To(BeEmpty())
			Expect(authInfo.Password).To(BeEmpty())
			Expect(authInfo.Exec).To(BeNil())
			Expect(authInfo.AuthProvider).To(BeNil())
		})

		DescribeTable("rejects empty inputs",
			func(token, ca []byte) {
				_, err := ManagementKubeconfig(token, ca)
				Expect(err).To(HaveOccurred())
			},
			Entry("empty token", nil, []byte("management-ca")),
			Entry("empty CA", []byte("service-account-token"), nil),
		)
	})

	Describe("Management cluster CA", func() {
		It("uses inline CA data", func() {
			got, err := ManagementClusterCA(&rest.Config{
				TLSClientConfig: rest.TLSClientConfig{CAData: []byte("inline-ca")},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal("inline-ca"))
		})

		It("reads the CA file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "ca.crt")
			Expect(os.WriteFile(path, []byte("file-ca"), 0o600)).To(Succeed())
			got, err := ManagementClusterCA(&rest.Config{
				TLSClientConfig: rest.TLSClientConfig{CAFile: path},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal("file-ca"))
		})

		It("does not expose filesystem details on error", func() {
			secretPath := filepath.Join(GinkgoT().TempDir(), "credential-secret")
			_, err := ManagementClusterCA(&rest.Config{
				TLSClientConfig: rest.TLSClientConfig{CAFile: secretPath},
			})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, os.ErrNotExist)).To(BeFalse(), "error exposes filesystem details: %v", err)
			Expect(err.Error()).NotTo(ContainSubstring(secretPath))
		})

		It("rejects a nil config", func() {
			_, err := ManagementClusterCA(nil)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a config with no CA data or file", func() {
			_, err := ManagementClusterCA(&rest.Config{})
			Expect(err).To(HaveOccurred())
		})

		It("rejects an empty CA file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "ca.crt")
			Expect(os.WriteFile(path, nil, 0o600)).To(Succeed())
			_, err := ManagementClusterCA(&rest.Config{
				TLSClientConfig: rest.TLSClientConfig{CAFile: path},
			})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ServiceAccount token issuer", func() {
		It("requests a token for the ServiceAccount", func(ctx SpecContext) {
			var gotSubResource string
			var gotServiceAccount *corev1.ServiceAccount
			var gotRequest *authenticationv1.TokenRequest
			c := tokenIssuerClient(func(subResourceName string, obj, subResource client.Object) error {
				gotSubResource = subResourceName
				gotServiceAccount, _ = obj.(*corev1.ServiceAccount)
				gotRequest, _ = subResource.(*authenticationv1.TokenRequest)
				if gotRequest != nil {
					gotRequest.Status.Token = "api-token"
				}
				return nil
			})

			token, err := NewServiceAccountTokenIssuer(c).Issue(ctx, "dpu-ns", "dpu-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(token)).To(Equal("api-token"))
			Expect(gotSubResource).To(Equal("token"))
			Expect(gotServiceAccount).NotTo(BeNil(), "token was not requested on a ServiceAccount")
			Expect(client.ObjectKeyFromObject(gotServiceAccount)).To(Equal(client.ObjectKey{Namespace: "dpu-ns", Name: "dpu-a"}))
			Expect(gotRequest).NotTo(BeNil(), "subresource was not a TokenRequest")
			Expect(gotRequest.Spec.Audiences).To(BeNil(), "want nil so the API server chooses its default")
			Expect(gotRequest.Spec.ExpirationSeconds).To(Equal(ptr.To[int64](86400)))
		})

		It("rejects an empty token", func(ctx SpecContext) {
			c := tokenIssuerClient(func(string, client.Object, client.Object) error {
				return nil
			})

			token, err := NewServiceAccountTokenIssuer(c).Issue(ctx, "dpu-ns", "dpu-a")
			Expect(err).To(HaveOccurred(), "Issue() = %q; want an error for an empty token", token)
		})

		It("sanitizes upstream errors", func(ctx SpecContext) {
			const sensitive = "upstream-secret-detail"
			c := tokenIssuerClient(func(string, client.Object, client.Object) error {
				return errors.New(sensitive)
			})

			_, err := NewServiceAccountTokenIssuer(c).Issue(ctx, "dpu-ns", "dpu-a")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring(sensitive))
		})
	})

})
