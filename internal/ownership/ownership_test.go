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

package ownership

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

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
	Describe("ValidateControlledBy", func() {
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
				err := ValidateControlledBy(obj, owner, "SimInstance", "Secret")
				if wantErr == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(ErrConflict))
				Expect(err).To(MatchError(fmt.Sprintf("%s: %s", wantErr, ErrConflict)))
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

	Describe("ControllerOwnerKey", func() {
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
				got, ok := ControllerOwnerKey(obj, apiVersion, "SimInstance")
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
})
