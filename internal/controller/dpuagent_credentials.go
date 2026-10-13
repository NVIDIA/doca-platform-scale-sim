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

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
