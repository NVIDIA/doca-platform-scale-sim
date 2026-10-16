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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

var _ = Describe("Ownership helpers", func() {
	Describe("validateControlledBy", func() {
		const ownerName = "scale-3000-000000"
		ref := func(uid types.UID, kind string, controller bool) metav1.OwnerReference {
			return metav1.OwnerReference{
				APIVersion: scalev1alpha1.GroupVersion.String(),
				Kind:       kind,
				Name:       ownerName,
				UID:        uid,
				Controller: ptr.To(controller),
			}
		}

		DescribeTable("checks the controller reference",
			func(obj *corev1.Secret, wantErr string) {
				owner := &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{
					Name:      ownerName,
					Namespace: "fleet-ns",
					UID:       "instance-uid",
				}}
				err := validateControlledBy(obj, owner, "SimInstance", "Secret")
				if wantErr == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(errOwnershipConflict))
				Expect(err).To(MatchError(fmt.Sprintf("%s: %s", wantErr, errOwnershipConflict)))
			},
			Entry("controlled by owner", ownedSecret(ref("instance-uid", "SimInstance", true)), ""),
			Entry("no owner", ownedSecret(), `Secret "child" is missing owner reference`),
			Entry("same UID different kind",
				ownedSecret(ref("instance-uid", "SimFleet", true)),
				`Secret "child" is controlled by another owner`),
			Entry("different UID",
				ownedSecret(ref("other-uid", "SimInstance", true)),
				`Secret "child" is controlled by another owner`),
			Entry("non-controller owner with matching UID",
				ownedSecret(ref("instance-uid", "SimInstance", false)),
				`Secret "child" is missing owner reference`),
		)
	})

	Describe("controllerOwnerKey", func() {
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

		DescribeTable("resolves the controlling owner",
			func(obj *corev1.Secret, want types.NamespacedName, wantOK bool) {
				got, ok := controllerOwnerKey(obj, apiVersion, "SimInstance")
				Expect(ok).To(Equal(wantOK))
				Expect(got).To(Equal(want))
			},
			Entry("controller ref",
				ownedSecret(ref(apiVersion, "SimInstance", true)),
				types.NamespacedName{Namespace: "fleet-ns", Name: "scale-3000-000000"}, true),
			Entry("wrong apiVersion",
				ownedSecret(ref("scale.dpu.nvidia.com/v1", "SimInstance", true)), types.NamespacedName{}, false),
			Entry("wrong kind", ownedSecret(ref(apiVersion, "SimFleet", true)), types.NamespacedName{}, false),
			Entry("no controller ref", ownedSecret(ref(apiVersion, "SimInstance", false)), types.NamespacedName{}, false),
			Entry("no owner refs", ownedSecret(), types.NamespacedName{}, false),
		)
	})

	Describe("stripFinalizersAndForceDelete", func() {
		var instance *scalev1alpha1.SimInstance

		BeforeEach(func() {
			instance = &scalev1alpha1.SimInstance{ObjectMeta: metav1.ObjectMeta{Name: "scale-3000-000000", Namespace: "fleet-ns"}}
		})

		It("removes the object and records an Event", func(ctx SpecContext) {
			secret := ownedSecret()
			secret.Finalizers = []string{"x/y"}
			c := fake.NewClientBuilder().WithScheme(instanceTestScheme()).WithObjects(secret).Build()
			recorder := events.NewFakeRecorder(10)

			Expect(stripFinalizersAndForceDelete(
				ctx, c, recorder, instance, secret.DeepCopy(), "Secret", 0, lifecycleForceTimeout+time.Second,
			)).To(Succeed())
			err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Secret GET error = %v, want NotFound", err)
			want := fmt.Sprintf(
				"Warning LifecycleForceDelete stripped finalizers from Secret %s ordinal %d after %s",
				secret.Name, 0, lifecycleForceTimeout,
			)
			Expect(recorder.Events).To(Receive(Equal(want)))
			Expect(recorder.Events).NotTo(Receive(), "unexpected second Event")
		})

		It("tolerates an already-gone object", func(ctx SpecContext) {
			c := fake.NewClientBuilder().WithScheme(instanceTestScheme()).Build()
			recorder := events.NewFakeRecorder(10)

			Expect(stripFinalizersAndForceDelete(
				ctx, c, recorder, instance, ownedSecret(), "Secret", 0, lifecycleForceTimeout,
			)).To(Succeed())
			Expect(recorder.Events).NotTo(Receive(), "unexpected Event for an already-gone object")
		})

		It("works without a recorder", func(ctx SpecContext) {
			secret := ownedSecret()
			secret.Finalizers = []string{"x/y"}
			c := fake.NewClientBuilder().WithScheme(instanceTestScheme()).WithObjects(secret).Build()

			Expect(stripFinalizersAndForceDelete(
				ctx, c, nil, instance, secret.DeepCopy(), "Secret", 0, lifecycleForceTimeout,
			)).To(Succeed())
			err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Secret GET error = %v, want NotFound", err)
		})
	})
})
