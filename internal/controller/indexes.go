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
	"fmt"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SetupIndexes registers the cache indexes the reconcilers read. A field can
// be indexed only once per manager, so this runs once, before any reconciler
// is set up.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &provisioningv1.DPU{}, dpuDeviceField, dpuDeviceIndex); err != nil {
		return fmt.Errorf("index DPU field %q: %w", dpuDeviceField, err)
	}
	return nil
}

func dpuDeviceIndex(object client.Object) []string {
	dpu, ok := object.(*provisioningv1.DPU)
	if !ok || dpu.Spec.DPUDeviceName == "" {
		return nil
	}
	return []string{dpu.Spec.DPUDeviceName}
}
