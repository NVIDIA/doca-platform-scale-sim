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

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
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
		Name:      utils.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, credentialOrdinal),
		Namespace: fleet.Namespace,
	}
}

func TestDesiredDPUAgentRoleUsesLeastPrivilege(t *testing.T) {
	fleet := credentialFleet()
	key := credentialDPUKey(fleet)
	role := desiredDPUAgentRole(fleet, credentialOrdinal)

	if key.Name != "scale-3000-host-0001-scale-3000-device-000012" {
		t.Fatalf("test fixture DPU name = %q", key.Name)
	}
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
			ResourceNames: []string{utils.KubeadmJoinSecretName(key.Name)},
			Verbs:         []string{"get"},
		},
	}
	if role.Name != key.Name || role.Namespace != key.Namespace {
		t.Fatalf("Role identity = %s/%s, want %s/%s", role.Namespace, role.Name, key.Namespace, key.Name)
	}
	if !equality.Semantic.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("Role rules = %#v, want %#v", role.Rules, wantRules)
	}
	assertFleetLabels(t, role.Labels, fleet, "000012")
}

func TestDesiredDPUAgentServiceAccountAndRoleBinding(t *testing.T) {
	fleet := credentialFleet()
	key := credentialDPUKey(fleet)
	serviceAccount := desiredDPUAgentServiceAccount(fleet, credentialOrdinal)
	binding := desiredDPUAgentRoleBinding(fleet, credentialOrdinal)

	if serviceAccount.Name != key.Name || serviceAccount.Namespace != key.Namespace {
		t.Fatalf("ServiceAccount identity = %s/%s", serviceAccount.Namespace, serviceAccount.Name)
	}
	assertFleetLabels(t, serviceAccount.Labels, fleet, "000012")
	if binding.Name != key.Name || binding.Namespace != key.Namespace {
		t.Fatalf("RoleBinding identity = %s/%s", binding.Namespace, binding.Name)
	}
	wantSubjects := []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      key.Name,
		Namespace: key.Namespace,
	}}
	wantRoleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: key.Name}
	if !equality.Semantic.DeepEqual(binding.Subjects, wantSubjects) {
		t.Fatalf("RoleBinding subjects = %#v, want %#v", binding.Subjects, wantSubjects)
	}
	if binding.RoleRef != wantRoleRef {
		t.Fatalf("RoleBinding roleRef = %#v, want %#v", binding.RoleRef, wantRoleRef)
	}
	assertFleetLabels(t, binding.Labels, fleet, "000012")
}

func assertFleetLabels(
	t *testing.T,
	labels map[string]string,
	fleet *scalev1alpha1.SimFleet,
	ordinal string,
) {
	t.Helper()
	want := inventoryLabels(fleet, ordinal)
	if !equality.Semantic.DeepEqual(labels, want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
}

func TestManagementKubeconfigIsLimited(t *testing.T) {
	token := []byte("service-account-token")
	ca := []byte("management-ca")
	data, err := managementKubeconfig(token, ca)
	if err != nil {
		t.Fatalf("managementKubeconfig: %v", err)
	}
	config, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("decode kubeconfig: %v", err)
	}
	if len(config.Clusters) != 1 || len(config.AuthInfos) != 1 || len(config.Contexts) != 1 {
		t.Fatalf("kubeconfig entries = clusters:%d authInfos:%d contexts:%d",
			len(config.Clusters), len(config.AuthInfos), len(config.Contexts))
	}
	cluster := config.Clusters[config.Contexts[config.CurrentContext].Cluster]
	authInfo := config.AuthInfos[config.Contexts[config.CurrentContext].AuthInfo]
	if cluster.Server != managementServiceURL {
		t.Fatalf("server = %q, want %q", cluster.Server, managementServiceURL)
	}
	if !equality.Semantic.DeepEqual(cluster.CertificateAuthorityData, ca) {
		t.Fatalf("CA = %q, want %q", cluster.CertificateAuthorityData, ca)
	}
	if authInfo.Token != string(token) {
		t.Fatalf("token = %q, want supplied service-account token", authInfo.Token)
	}
	if authInfo.ClientCertificateData != nil || authInfo.ClientKeyData != nil ||
		authInfo.Username != "" || authInfo.Password != "" || authInfo.Exec != nil ||
		authInfo.AuthProvider != nil {
		t.Fatalf("kubeconfig contains additional authentication data: %#v", authInfo)
	}
}

func TestManagementKubeconfigRejectsEmptyInputs(t *testing.T) {
	cases := []struct {
		name  string
		token []byte
		ca    []byte
	}{
		{name: "empty token", ca: []byte("management-ca")},
		{name: "empty CA", token: []byte("service-account-token")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := managementKubeconfig(testCase.token, testCase.ca); err == nil {
				t.Fatal("managementKubeconfig() = nil error")
			}
		})
	}
}

func TestManagementClusterCAUsesDataOrFile(t *testing.T) {
	t.Run("data", func(t *testing.T) {
		got, err := ManagementClusterCA(&rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CAData: []byte("inline-ca")},
		})
		if err != nil || string(got) != "inline-ca" {
			t.Fatalf("ManagementClusterCA() = %q, %v", got, err)
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(path, []byte("file-ca"), 0o600); err != nil {
			t.Fatalf("write CA file: %v", err)
		}
		got, err := ManagementClusterCA(&rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CAFile: path},
		})
		if err != nil || string(got) != "file-ca" {
			t.Fatalf("ManagementClusterCA() = %q, %v", got, err)
		}
	})

	t.Run("does not expose filesystem details on error", func(t *testing.T) {
		secretPath := filepath.Join(t.TempDir(), "credential-secret")
		_, err := ManagementClusterCA(&rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CAFile: secretPath},
		})
		if err == nil {
			t.Fatal("ManagementClusterCA() = nil error")
		}
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), secretPath) {
			t.Fatalf("error exposes filesystem details: %v", err)
		}
	})

	t.Run("nil config", func(t *testing.T) {
		if _, err := ManagementClusterCA(nil); err == nil {
			t.Fatal("ManagementClusterCA(nil) = nil error")
		}
	})

	t.Run("no CA data or file", func(t *testing.T) {
		if _, err := ManagementClusterCA(&rest.Config{}); err == nil {
			t.Fatal("ManagementClusterCA() = nil error")
		}
	})

	t.Run("empty CA file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write CA file: %v", err)
		}
		_, err := ManagementClusterCA(&rest.Config{
			TLSClientConfig: rest.TLSClientConfig{CAFile: path},
		})
		if err == nil {
			t.Fatal("ManagementClusterCA() = nil error")
		}
	})
}

// tokenIssuerClient returns a client whose token subresource create is handled
// by create.
func tokenIssuerClient(
	t *testing.T,
	create func(subResourceName string, obj, subResource client.Object) error,
) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	if err := authenticationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add authentication types: %v", err)
	}
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

func TestTokenIssuerRequestsTokenForServiceAccount(t *testing.T) {
	var gotSubResource string
	var gotServiceAccount *corev1.ServiceAccount
	var gotRequest *authenticationv1.TokenRequest
	c := tokenIssuerClient(t, func(subResourceName string, obj, subResource client.Object) error {
		gotSubResource = subResourceName
		gotServiceAccount, _ = obj.(*corev1.ServiceAccount)
		gotRequest, _ = subResource.(*authenticationv1.TokenRequest)
		if gotRequest != nil {
			gotRequest.Status.Token = "api-token"
		}
		return nil
	})

	token, err := NewServiceAccountTokenIssuer(c).Issue(context.Background(), "dpu-ns", "dpu-a")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if string(token) != "api-token" {
		t.Fatalf("token = %q, want api-token", token)
	}
	if gotSubResource != "token" {
		t.Fatalf("subresource = %q, want token", gotSubResource)
	}
	if gotServiceAccount == nil {
		t.Fatal("token was not requested on a ServiceAccount")
	}
	if gotServiceAccount.Namespace != "dpu-ns" || gotServiceAccount.Name != "dpu-a" {
		t.Fatalf("ServiceAccount = %s/%s, want dpu-ns/dpu-a", gotServiceAccount.Namespace, gotServiceAccount.Name)
	}
	if gotRequest == nil {
		t.Fatal("subresource was not a TokenRequest")
	}
	if gotRequest.Spec.Audiences != nil {
		t.Fatalf("audiences = %#v, want nil so the API server chooses its default", gotRequest.Spec.Audiences)
	}
	if gotRequest.Spec.ExpirationSeconds == nil || *gotRequest.Spec.ExpirationSeconds != 86400 {
		t.Fatalf("expirationSeconds = %v, want 86400", gotRequest.Spec.ExpirationSeconds)
	}
}

func TestTokenIssuerRejectsEmptyToken(t *testing.T) {
	c := tokenIssuerClient(t, func(string, client.Object, client.Object) error {
		return nil
	})

	token, err := NewServiceAccountTokenIssuer(c).Issue(context.Background(), "dpu-ns", "dpu-a")
	if err == nil {
		t.Fatalf("Issue() = %q, nil error; want an error for an empty token", token)
	}
}

func TestTokenIssuerSanitizesErrors(t *testing.T) {
	const sensitive = "upstream-secret-detail"
	c := tokenIssuerClient(t, func(string, client.Object, client.Object) error {
		return errors.New(sensitive)
	})

	_, err := NewServiceAccountTokenIssuer(c).Issue(context.Background(), "dpu-ns", "dpu-a")
	if err == nil {
		t.Fatal("Issue() = nil error")
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("Issue() error exposes upstream detail: %v", err)
	}
}

// Only a fleet whose guests boot with an enabled agent gets per-DPU
// credentials. Creating a ServiceAccount, Role, and RoleBinding for any other
// fleet grants management API access nothing will ever use.
func TestCredentialsEligibleOnlyForAFleetThatCanIssueCredentials(t *testing.T) {
	enabled, disabled := true, false
	template := &scalev1alpha1.VirtualMachineTemplate{
		Image: "example.com/node@sha256:test",
	}
	cases := []struct {
		name     string
		prepare  func(*scalev1alpha1.SimFleet)
		eligible bool
	}{
		{
			name: "a VM template and an enabled agent",
			prepare: func(fleet *scalev1alpha1.SimFleet) {
				fleet.Spec.VirtualMachineTemplate = template
				fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: &enabled}
			},
			eligible: true,
		},
		{
			// No VirtualMachine means no guest, and the cloud-init pass that
			// issues credentials returns before it reaches them.
			name: "no VM template",
			prepare: func(fleet *scalev1alpha1.SimFleet) {
				fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: &enabled}
			},
		},
		{
			// A guest with the agent off gets a payload with no credential in
			// it, and both units runtime-masked.
			name: "a disabled agent",
			prepare: func(fleet *scalev1alpha1.SimFleet) {
				fleet.Spec.VirtualMachineTemplate = template
				fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: &disabled}
			},
		},
		{
			// A template this controller refuses never produces a guest, so
			// the pass fails before issuing anything.
			name: "a template this controller refuses",
			prepare: func(fleet *scalev1alpha1.SimFleet) {
				fleet.Spec.VirtualMachineTemplate = &scalev1alpha1.VirtualMachineTemplate{
					Image:   "example.com/node@sha256:test",
					Storage: &scalev1alpha1.VirtualMachineStorage{Ephemeral: &disabled},
				}
				fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: &enabled}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fleet := credentialFleet()
			testCase.prepare(fleet)
			if got := credentialsEligible(fleet); got != testCase.eligible {
				t.Fatalf("credentialsEligible = %t, want %t", got, testCase.eligible)
			}
		})
	}
}
