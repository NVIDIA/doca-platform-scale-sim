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

// Package ownership checks controller owner references.
package ownership

import (
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrConflict marks an object that exists but is not controlled by the owner
// expected to manage it.
var ErrConflict = errors.New("ownership conflict")

// ValidateControlledBy returns an error wrapping ErrConflict unless owner,
// of ownerKind, is obj's controller.
func ValidateControlledBy(obj client.Object, owner client.Object, ownerKind string, kind string) error {
	ref := metav1.GetControllerOf(obj)
	if ref == nil {
		return fmt.Errorf("%s %q is missing owner reference: %w", kind, obj.GetName(), ErrConflict)
	}
	if ref.UID != owner.GetUID() || ref.Kind != ownerKind {
		return fmt.Errorf("%s %q is controlled by another owner: %w", kind, obj.GetName(), ErrConflict)
	}
	return nil
}

// ControllerOwnerKey is the owner of kind that controls obj, if any.
func ControllerOwnerKey(obj client.Object, apiVersion, kind string) (types.NamespacedName, bool) {
	ref := metav1.GetControllerOf(obj)
	if ref == nil || ref.APIVersion != apiVersion || ref.Kind != kind {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Name: ref.Name, Namespace: obj.GetNamespace()}, true
}

// ControlledBy reports whether object's controller reference names exactly
// this API version, kind, name, and UID.
func ControlledBy(object client.Object, apiVersion, kind, name string, uid types.UID) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil &&
		owner.APIVersion == apiVersion &&
		owner.Kind == kind &&
		owner.Name == name &&
		owner.UID == uid
}
