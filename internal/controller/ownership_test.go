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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

func ownedSecret(ownerRefs ...metav1.OwnerReference) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:            "child",
		Namespace:       "fleet-ns",
		OwnerReferences: ownerRefs,
	}}
}

func TestValidateControlledBy(t *testing.T) {
	owner := &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{
		Name:      "scale-3000-000000",
		Namespace: "fleet-ns",
		UID:       "instance-uid",
	}}
	ref := func(uid types.UID, kind string, controller bool) metav1.OwnerReference {
		return metav1.OwnerReference{
			APIVersion: scalev1alpha1.GroupVersion.String(),
			Kind:       kind,
			Name:       owner.Name,
			UID:        uid,
			Controller: ptr.To(controller),
		}
	}
	for _, tc := range []struct {
		name    string
		obj     *corev1.Secret
		wantErr string
	}{
		{name: "controlled by owner", obj: ownedSecret(ref("instance-uid", "SimInstance", true))},
		{
			name:    "no owner",
			obj:     ownedSecret(),
			wantErr: `Secret "child" is missing owner reference`,
		},
		{
			name:    "same UID different kind",
			obj:     ownedSecret(ref("instance-uid", "SimFleet", true)),
			wantErr: `Secret "child" is controlled by another owner`,
		},
		{
			name:    "different UID",
			obj:     ownedSecret(ref("other-uid", "SimInstance", true)),
			wantErr: `Secret "child" is controlled by another owner`,
		},
		{
			name:    "non-controller owner with matching UID",
			obj:     ownedSecret(ref("instance-uid", "SimInstance", false)),
			wantErr: `Secret "child" is missing owner reference`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateControlledBy(tc.obj, owner, "SimInstance", "Secret")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateControlledBy() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateControlledBy() = nil, want %q", tc.wantErr)
			}
			if !errors.Is(err, errOwnershipConflict) {
				t.Fatalf("validateControlledBy() = %v, want it to wrap errOwnershipConflict", err)
			}
			want := fmt.Sprintf("%s: %s", tc.wantErr, errOwnershipConflict)
			if err.Error() != want {
				t.Fatalf("validateControlledBy() = %q, want %q", err, want)
			}
		})
	}
}

func TestControllerOwnerKey(t *testing.T) {
	apiVersion := scalev1alpha1.GroupVersion.String()
	ref := func(apiVersion, kind string, controller bool) metav1.OwnerReference {
		return metav1.OwnerReference{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       "scale-3000-000000",
			UID:        "instance-uid",
			Controller: ptr.To(controller),
		}
	}
	for _, tc := range []struct {
		name   string
		obj    *corev1.Secret
		want   types.NamespacedName
		wantOK bool
	}{
		{
			name:   "controller ref",
			obj:    ownedSecret(ref(apiVersion, "SimInstance", true)),
			want:   types.NamespacedName{Namespace: "fleet-ns", Name: "scale-3000-000000"},
			wantOK: true,
		},
		{name: "wrong apiVersion", obj: ownedSecret(ref("scale.dpu.nvidia.com/v1", "SimInstance", true))},
		{name: "wrong kind", obj: ownedSecret(ref(apiVersion, "SimFleet", true))},
		{name: "no controller ref", obj: ownedSecret(ref(apiVersion, "SimInstance", false))},
		{name: "no owner refs", obj: ownedSecret()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := controllerOwnerKey(tc.obj, apiVersion, "SimInstance")
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("controllerOwnerKey() = (%v, %t), want (%v, %t)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestStripFinalizersAndForceDeleteRemovesObjectAndRecordsEvent(t *testing.T) {
	ctx := context.Background()
	scheme := instanceTestScheme(t)
	instance := &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{Name: "scale-3000-000000", Namespace: "fleet-ns"}}
	secret := ownedSecret()
	secret.Finalizers = []string{"x/y"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	recorder := events.NewFakeRecorder(10)

	err := stripFinalizersAndForceDelete(
		ctx, c, recorder, instance, secret.DeepCopy(), "Secret", 0, lifecycleForceTimeout+time.Second,
	)
	if err != nil {
		t.Fatalf("stripFinalizersAndForceDelete() = %v, want nil", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret GET error = %v, want NotFound", err)
	}
	want := fmt.Sprintf(
		"Warning LifecycleForceDelete stripped finalizers from Secret %s ordinal %d after %s",
		secret.Name, 0, lifecycleForceTimeout,
	)
	select {
	case got := <-recorder.Events:
		if got != want {
			t.Fatalf("Event = %q, want %q", got, want)
		}
	default:
		t.Fatal("expected LifecycleForceDelete Event")
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected second Event %q", event)
	default:
	}
}

func TestStripFinalizersAndForceDeleteToleratesGoneObject(t *testing.T) {
	ctx := context.Background()
	scheme := instanceTestScheme(t)
	instance := &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{Name: "scale-3000-000000", Namespace: "fleet-ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := events.NewFakeRecorder(10)

	err := stripFinalizersAndForceDelete(
		ctx, c, recorder, instance, ownedSecret(), "Secret", 0, lifecycleForceTimeout,
	)
	if err != nil {
		t.Fatalf("stripFinalizersAndForceDelete() = %v, want nil", err)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected Event %q for an already-gone object", event)
	default:
	}
}

func TestStripFinalizersAndForceDeleteWithoutRecorder(t *testing.T) {
	ctx := context.Background()
	scheme := instanceTestScheme(t)
	instance := &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{Name: "scale-3000-000000", Namespace: "fleet-ns"}}
	secret := ownedSecret()
	secret.Finalizers = []string{"x/y"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	if err := stripFinalizersAndForceDelete(
		ctx, c, nil, instance, secret.DeepCopy(), "Secret", 0, lifecycleForceTimeout,
	); err != nil {
		t.Fatalf("stripFinalizersAndForceDelete() = %v, want nil", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret GET error = %v, want NotFound", err)
	}
}
