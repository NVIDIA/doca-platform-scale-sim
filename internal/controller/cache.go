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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// ManagerCacheOptions scopes the credential-bearing kinds and the guest Pods
// to objects this controller labels, so watching them does not cache every
// Secret, ServiceAccount, Role, RoleBinding, and Pod in the cluster.
func ManagerCacheOptions() cache.Options {
	fleetLabelled := cache.ByObject{Label: fleetLabelSelector()}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}:         fleetLabelled,
		&corev1.ServiceAccount{}: fleetLabelled,
		&rbacv1.Role{}:           fleetLabelled,
		&rbacv1.RoleBinding{}:    fleetLabelled,
		&corev1.Pod{}:            fleetLabelled,
	}}
}

func fleetLabelSelector() labels.Selector {
	requirement, err := labels.NewRequirement(naming.LabelFleetName, selection.Exists, nil)
	if err != nil {
		panic(fmt.Sprintf("build %s selector: %v", naming.LabelFleetName, err))
	}
	return labels.NewSelector().Add(*requirement)
}
