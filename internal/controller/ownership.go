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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var errOwnershipConflict = errors.New("ownership conflict")

func validateControlledBy(obj client.Object, owner client.Object, ownerKind string, kind string) error {
	ref := metav1.GetControllerOf(obj)
	if ref == nil {
		return fmt.Errorf("%s %q is missing owner reference: %w", kind, obj.GetName(), errOwnershipConflict)
	}
	if ref.UID != owner.GetUID() || ref.Kind != ownerKind {
		return fmt.Errorf("%s %q is controlled by another owner: %w", kind, obj.GetName(), errOwnershipConflict)
	}
	return nil
}

// controllerOwnerKey is the owner of kind that controls obj, if any.
func controllerOwnerKey(obj client.Object, apiVersion, kind string) (types.NamespacedName, bool) {
	ref := metav1.GetControllerOf(obj)
	if ref == nil || ref.APIVersion != apiVersion || ref.Kind != kind {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Name: ref.Name, Namespace: obj.GetNamespace()}, true
}

// controlledBy reports whether object's controller reference names exactly
// this API version, kind, name, and UID.
func controlledBy(object client.Object, apiVersion, kind, name string, uid types.UID) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil &&
		owner.APIVersion == apiVersion &&
		owner.Kind == kind &&
		owner.Name == name &&
		owner.UID == uid
}

// stripFinalizersAndForceDelete assumes the caller has already proved obj is
// eventObject's and that the phase deadline has passed.
func stripFinalizersAndForceDelete(
	ctx context.Context,
	c client.Client,
	recorder events.EventRecorder,
	eventObject client.Object,
	obj client.Object,
	kind string,
	ordinal int,
	elapsed time.Duration,
) error {
	base := obj.DeepCopyObject().(client.Object)
	obj.SetFinalizers(nil)
	if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("strip %s %q finalizers: %w", kind, obj.GetName(), err)
	}
	message := fmt.Sprintf(
		"stripped finalizers from %s %s ordinal %d after %s",
		kind, obj.GetName(), ordinal, lifecycleForceTimeout,
	)
	if recorder != nil {
		recorder.Eventf(eventObject, nil, corev1.EventTypeWarning, "LifecycleForceDelete", "ForceDelete", "%s", message)
	}
	logf.FromContext(ctx).Error(nil, "Stripped finalizers from blocked lifecycle object",
		"ordinal", ordinal,
		"kind", kind,
		"name", obj.GetName(),
		"elapsed", elapsed,
	)
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %q after stripping finalizers: %w", kind, obj.GetName(), err)
	}
	return nil
}
