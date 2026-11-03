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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

var _ = Describe("Manager cache selector", func() {
	It("scopes Pods to the fleet label, as it does Secrets", func() {
		byObject := ManagerCacheOptions().ByObject
		var selectors []string
		for obj, options := range byObject {
			if _, ok := obj.(*corev1.Pod); ok {
				Expect(options.Label).NotTo(BeNil())
				selectors = append(selectors, options.Label.String())
			}
		}
		Expect(selectors).To(Equal([]string{naming.LabelFleetName}))
	})
})

var _ = Describe("Manager cache", func() {
	It("caches only fleet-labelled Secrets", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cache-scope-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

		labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      "labelled",
			Namespace: namespace.Name,
			Labels:    map[string]string{naming.LabelFleetName: "scale-3000"},
		}}
		unlabelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      "unlabelled",
			Namespace: namespace.Name,
		}}
		Expect(k8sClient.Create(ctx, labelled)).To(Succeed())
		Expect(k8sClient.Create(ctx, unlabelled)).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Cache:                  ManagerCacheOptions(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())
		mgrCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			stop()
			Eventually(done, time.Minute).Should(BeClosed())
		})

		cached := mgr.GetClient()
		Eventually(func() error {
			return cached.Get(ctx, client.ObjectKeyFromObject(labelled), &corev1.Secret{})
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		err = cached.Get(ctx, client.ObjectKeyFromObject(unlabelled), &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Get(unlabelled) error = %v, want NotFound", err)
	})
})
