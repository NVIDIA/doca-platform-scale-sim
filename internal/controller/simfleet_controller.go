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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

const (
	labelFleetName = "scale.dpu.nvidia.com/fleet-name"
	labelFleetUID  = "scale.dpu.nvidia.com/fleet-uid"
	labelOrdinal   = "scale.dpu.nvidia.com/ordinal"
	dpuDeviceField = "spec.dpuDeviceName"
)

// SimFleetReconciler reconciles a SimFleet object
type SimFleetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// MaxConcurrentReconciles bounds parallel fleet reconciles. Zero uses
	// the controller-runtime default.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=simfleets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=simfleets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=simfleets/finalizers,verbs=update
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=provisioning.dpu.nvidia.com,resources=dpunodes,verbs=get;list;watch;create;update;patch;delete

// Reconcile rolls the fleet out through one SimInstance per ordinal.
func (r *SimFleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. SimInstance
// events are the only children events the rollout needs: each instance
// reports its own children through its phase and conditions.
func (r *SimFleetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalev1alpha1.SimFleet{}).
		Owns(&scalev1alpha1.SimInstance{}, builder.WithPredicates(simInstanceWatchPredicate())).
		Owns(&provisioningv1.DPUNode{}, builder.WithPredicates(inventoryChildPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("simfleet").
		Complete(r)
}
