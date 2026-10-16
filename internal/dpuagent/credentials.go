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
	"fmt"
	"os"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

const (
	managementServiceURL = "https://kubernetes.default.svc"
	managementConfigName = "management"
)

// ServiceAccountTokenIssuer issues a bearer token for one namespaced
// ServiceAccount.
type ServiceAccountTokenIssuer interface {
	Issue(ctx context.Context, namespace, name string) ([]byte, error)
}

type serviceAccountTokenIssuer struct {
	client client.Client
}

// NewServiceAccountTokenIssuer creates an issuer backed by the TokenRequest
// subresource.
func NewServiceAccountTokenIssuer(c client.Client) ServiceAccountTokenIssuer {
	return &serviceAccountTokenIssuer{client: c}
}

func (i *serviceAccountTokenIssuer) Issue(
	ctx context.Context,
	namespace, name string,
) ([]byte, error) {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	// 86400 seconds, and deliberately not renewed. This is an initial-join and
	// e2e credential: a guest uses it once, to read its own DPU and that
	// DPU's join Secret, and cloud-init data is create-only so nothing
	// rewrites it afterwards.
	//
	// Two consequences follow, and both are accepted rather than fixed here.
	// The Linux e2e timeout has to stay below this lifetime, because a run
	// that outlives the token has guests that can no longer read their join
	// Secret. And a fleet large enough that some ordinal is still waiting to
	// join a day after its Secret was created will find the token expired;
	// that is recorded as a scale-duration limitation. A day covers a
	// thousands-of-DPUs rollout queued behind DPF's parallel-install limits.
	result := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: ptr.To[int64](86400),
		},
	}
	if err := i.client.SubResource("token").Create(ctx, serviceAccount, result); err != nil {
		return nil, errors.New("could not issue management ServiceAccount token")
	}
	if result.Status.Token == "" {
		return nil, errors.New("management ServiceAccount TokenRequest returned an empty token")
	}
	return []byte(result.Status.Token), nil
}

// CredentialName is the identity every per-ordinal credential resource
// carries: the name DPF will give the DPU. The guest is issued this identity
// while the VM is created, which is before the DPU exists, so it cannot be read
// off a DPU object.
func CredentialName(fleet *scalev1alpha1.SimFleet, ordinal int) string {
	return naming.DPUName(fleet.Name, fleet.Spec.DevicesPerDPUNode, ordinal)
}

// DesiredServiceAccount is the identity the guest for ordinal authenticates
// to the management cluster as.
func DesiredServiceAccount(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CredentialName(fleet, ordinal),
			Namespace: fleet.Namespace,
			Labels:    dpuAgentCredentialLabels(fleet, ordinal),
		},
	}
}

// DesiredRole grants the guest for ordinal read access to its own DPU and
// that DPU's join Secret, and nothing else.
func DesiredRole(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
) *rbacv1.Role {
	name := CredentialName(fleet, ordinal)
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fleet.Namespace,
			Labels:    dpuAgentCredentialLabels(fleet, ordinal),
		},
		// The guest's join helper is the only consumer of this credential, and
		// it does exactly two reads: the DPU, to prove the join Secret really
		// belongs to it, and that one Secret.
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{"provisioning.dpu.nvidia.com"},
				Resources:     []string{"dpus"},
				ResourceNames: []string{name},
				Verbs:         []string{"get"},
			},
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{naming.KubeadmJoinSecretName(name)},
				Verbs:         []string{"get"},
			},
		},
	}
}

// DesiredRoleBinding binds DesiredRole to DesiredServiceAccount.
func DesiredRoleBinding(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
) *rbacv1.RoleBinding {
	name := CredentialName(fleet, ordinal)
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fleet.Namespace,
			Labels:    dpuAgentCredentialLabels(fleet, ordinal),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      name,
			Namespace: fleet.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
	}
}

func dpuAgentCredentialLabels(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
) map[string]string {
	return naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal))
}

// ManagementKubeconfig encodes a kubeconfig that reaches the management
// cluster's in-cluster Service with token and nothing else.
func ManagementKubeconfig(token, ca []byte) ([]byte, error) {
	if len(token) == 0 {
		return nil, errors.New("management ServiceAccount token is empty")
	}
	if len(ca) == 0 {
		return nil, errors.New("management cluster CA is empty")
	}
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			managementConfigName: {
				Server:                   managementServiceURL,
				CertificateAuthorityData: append([]byte(nil), ca...),
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			managementConfigName: {Token: string(token)},
		},
		Contexts: map[string]*clientcmdapi.Context{
			managementConfigName: {
				Cluster:  managementConfigName,
				AuthInfo: managementConfigName,
			},
		},
		CurrentContext: managementConfigName,
	}
	data, err := clientcmd.Write(config)
	if err != nil {
		return nil, errors.New("could not encode management kubeconfig")
	}
	return data, nil
}

// ManagementClusterCA returns the CA used by the manager's REST config. It
// supports both in-memory and file-backed client configuration.
func ManagementClusterCA(config *rest.Config) ([]byte, error) {
	if config == nil {
		return nil, errors.New("management REST config is missing")
	}
	if len(config.CAData) > 0 {
		return append([]byte(nil), config.CAData...), nil
	}
	if config.CAFile == "" {
		return nil, errors.New("management cluster CA is missing")
	}
	ca, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, errors.New("could not load management cluster CA")
	}
	if len(ca) == 0 {
		return nil, errors.New("management cluster CA is empty")
	}
	return ca, nil
}

func credentialKind(object client.Object) (string, error) {
	switch object.(type) {
	case *corev1.ServiceAccount:
		return "ServiceAccount", nil
	case *rbacv1.Role:
		return "Role", nil
	case *rbacv1.RoleBinding:
		return "RoleBinding", nil
	default:
		return "", fmt.Errorf("unsupported credential resource type %T", object)
	}
}

// EmptyCredentialResource returns a zero object of the same type as object,
// to read the live resource into, and its kind.
func EmptyCredentialResource(object client.Object) (client.Object, string, error) {
	kind, err := credentialKind(object)
	if err != nil {
		return nil, "", err
	}
	switch object.(type) {
	case *corev1.ServiceAccount:
		return &corev1.ServiceAccount{}, kind, nil
	case *rbacv1.Role:
		return &rbacv1.Role{}, kind, nil
	case *rbacv1.RoleBinding:
		return &rbacv1.RoleBinding{}, kind, nil
	default:
		return nil, "", fmt.Errorf("unsupported credential resource type %T", object)
	}
}
