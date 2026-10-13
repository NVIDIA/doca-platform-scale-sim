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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

// SimInstanceReconciler converges the children of one SimFleet ordinal.
type SimInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the API server directly. The cache holds only
	// fleet-labelled Secrets, ServiceAccounts, Roles, and RoleBindings, so a
	// child that lost its labels, or that the cache has not seen yet, is
	// found only through it.
	APIReader client.Reader

	// ServiceAccountTokenIssuer mints per-DPU management API credentials.
	ServiceAccountTokenIssuer ServiceAccountTokenIssuer
	// ManagementCA authenticates the management API from simulated DPUs.
	ManagementCA []byte

	// Recorder emits instance Events. A nil value skips Events.
	Recorder events.EventRecorder
	// Now is the teardown clock. A nil value uses time.Now.
	Now func() time.Time
	// MaxConcurrentReconciles bounds parallel instance reconciles. Zero uses
	// the controller-runtime default.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scale.dpu.nvidia.com,resources=siminstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=provisioning.dpu.nvidia.com,resources=dpudevices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=provisioning.dpu.nvidia.com,resources=dpus,verbs=get;list;watch;delete;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch;delete;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile converges one ordinal's children in order and reports progress
// through the instance conditions and phase.
func (r *SimInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *SimInstanceReconciler) mapDPUToInstance(ctx context.Context, object client.Object) []ctrl.Request {
	dpu, ok := object.(*provisioningv1.DPU)
	if !ok || dpu.Spec.DPUDeviceName == "" {
		return nil
	}
	device := &provisioningv1.DPUDevice{}
	key := client.ObjectKey{Name: dpu.Spec.DPUDeviceName, Namespace: dpu.Namespace}
	if err := r.Get(ctx, key, device); err != nil {
		return nil
	}
	instanceKey, ok := controllerOwnerKey(device, scalev1alpha1.GroupVersion.String(), "SimInstance")
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: instanceKey}}
}

// mapFleetToInstances enqueues every instance the fleet controls, so a spec
// edit reaches the children each instance converges from it.
func (r *SimInstanceReconciler) mapFleetToInstances(ctx context.Context, object client.Object) []ctrl.Request {
	fleet, ok := object.(*scalev1alpha1.SimFleet)
	if !ok {
		return nil
	}
	list := &scalev1alpha1.SimInstanceList{}
	if err := r.List(ctx, list,
		client.InNamespace(fleet.Namespace),
		client.MatchingLabels{labelFleetName: fleet.Name},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list SimInstances of SimFleet", "simFleet", fleet.Name)
		return nil
	}
	var requests []ctrl.Request
	for i := range list.Items {
		instance := &list.Items[i]
		if owner := metav1.GetControllerOf(instance); owner == nil || owner.UID != fleet.UID {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	}
	return requests
}

// mapVMIToInstance resolves only KubeVirt-owned VMIs through their VM owner;
// malformed or unrelated controller references use KubeVirt's same-name VM.
func (r *SimInstanceReconciler) mapVMIToInstance(ctx context.Context, object client.Object) []ctrl.Request {
	vmi, ok := object.(*virtv1.VirtualMachineInstance)
	if !ok {
		return nil
	}
	vmName := vmi.Name
	if owner := metav1.GetControllerOf(vmi); owner != nil &&
		owner.APIVersion == virtv1.GroupVersion.String() &&
		owner.Kind == "VirtualMachine" {
		vmName = owner.Name
	}
	vm := &virtv1.VirtualMachine{}
	if err := r.Get(ctx, client.ObjectKey{Name: vmName, Namespace: vmi.Namespace}, vm); err != nil {
		return nil
	}
	instanceKey, ok := controllerOwnerKey(vm, scalev1alpha1.GroupVersion.String(), "SimInstance")
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: instanceKey}}
}

// SetupWithManager sets up the controller with the Manager. The DPU
// spec.dpuDeviceName index it reads is registered by SetupIndexes.
func (r *SimInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalev1alpha1.SimInstance{}).
		Owns(&provisioningv1.DPUDevice{}, builder.WithPredicates(inventoryChildPredicate())).
		Owns(&virtv1.VirtualMachine{}, builder.WithPredicates(virtualMachineWatchPredicate())).
		Owns(&corev1.Secret{}, builder.WithPredicates(secretWatchPredicate())).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(
			&provisioningv1.DPU{},
			handler.EnqueueRequestsFromMapFunc(r.mapDPUToInstance),
			builder.WithPredicates(dpuWatchPredicate()),
		).
		Watches(
			&virtv1.VirtualMachineInstance{},
			handler.EnqueueRequestsFromMapFunc(r.mapVMIToInstance),
			builder.WithPredicates(vmiWatchPredicate()),
		).
		Watches(
			&scalev1alpha1.SimFleet{},
			handler.EnqueueRequestsFromMapFunc(r.mapFleetToInstances),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("siminstance").
		Complete(r)
}

func instanceConditionTrue(instance *scalev1alpha1.SimInstance, conditionType string) bool {
	return meta.IsStatusConditionTrue(instance.Status.Conditions, conditionType)
}

const lifecycleForceTimeout = 5 * time.Minute
