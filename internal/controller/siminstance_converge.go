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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/cloudinit"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpf"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/ownership"
)

// instanceChildren are the objects one converge pass found in place. A nil
// field is a child that does not exist, or does not exist yet.
type instanceChildren struct {
	dpu    *provisioningv1.DPU
	device *provisioningv1.DPUDevice
	vm     *virtv1.VirtualMachine
	vmi    *virtv1.VirtualMachineInstance
}

func (c instanceChildren) observedUIDs() scalev1alpha1.ObservedUIDs {
	var uids scalev1alpha1.ObservedUIDs
	if c.dpu != nil {
		uids.DPU = string(c.dpu.UID)
	}
	if c.device != nil {
		uids.DPUDevice = string(c.device.UID)
	}
	if c.vm != nil {
		uids.VirtualMachine = string(c.vm.UID)
	}
	if c.vmi != nil {
		uids.VirtualMachineInstance = string(c.vmi.UID)
	}
	return uids
}

// converge ensures one ordinal's children in the order the guest needs them:
// credentials and cloud-init first, then the DPUDevice, then the
// VirtualMachine that boots from both, and only then the BMC address that
// lets DPF claim the device.
func (r *SimInstanceReconciler) converge(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (instanceChildren, error) {
	var children instanceChildren
	cloudInitReady, err := r.ensureInstanceCredentials(ctx, fleet, instance)
	if err != nil {
		return children, failInstanceCondition(instance, scalev1alpha1.SimInstanceCredentialsReady, err)
	}
	if children.device, err = r.ensureInstanceDevice(ctx, fleet, instance); err != nil {
		return children, failInstanceCondition(instance, scalev1alpha1.SimInstanceDeviceCreated, err)
	}
	children.vm, err = r.ensureInstanceVirtualMachine(ctx, fleet, instance, cloudInitReady, children.device)
	if err != nil {
		return children, failInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, err)
	}
	if children.vmi, err = r.getInstanceVMI(ctx, children.vm); err != nil {
		return children, err
	}
	if err := r.ensureInstanceBMCIP(ctx, fleet, instance, children); err != nil {
		return children, failInstanceCondition(instance, scalev1alpha1.SimInstanceSelectable, err)
	}
	observeInstanceVM(fleet, instance, children)
	children.dpu, err = r.observeInstanceDPU(ctx, fleet, instance)
	return children, err
}

// ensureInstanceCredentials ensures the DPU agent credentials and the
// cloud-init Secret, and reports whether that Secret is in place for the
// VirtualMachine to mount. A fleet that boots guests needs the Secret even
// with the agent disabled; only the token in it is conditional.
func (r *SimInstanceReconciler) ensureInstanceCredentials(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (bool, error) {
	tmpl := fleet.Spec.VirtualMachineTemplate
	if tmpl == nil || kubevirt.ValidateTemplate(tmpl) != nil {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceCredentialsReady, true,
			"NotRequired", "No guest boots from this instance")
		return false, nil
	}
	eligible := credentialsEligible(fleet)
	var serviceAccount *corev1.ServiceAccount
	accessReady := true
	if eligible {
		var err error
		if serviceAccount, accessReady, err = r.ensureInstanceAgentAccess(ctx, fleet, instance); err != nil {
			return false, err
		}
	}
	secretReady := false
	if accessReady {
		var err error
		if secretReady, err = r.ensureInstanceCloudInitSecret(ctx, fleet, instance, serviceAccount); err != nil {
			return false, err
		}
	}
	switch {
	case !eligible:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceCredentialsReady, true,
			"NotRequired", "The DPU agent is disabled")
	case secretReady:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceCredentialsReady, true,
			"Ready", "DPU agent credentials and cloud-init Secret are in place")
	default:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceCredentialsReady, false,
			"Progressing", "DPU agent credentials are being created")
	}
	return secretReady, nil
}

// ensureInstanceAgentAccess ensures the ServiceAccount, Role, and RoleBinding
// the guest's token is issued against.
func (r *SimInstanceReconciler) ensureInstanceAgentAccess(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (*corev1.ServiceAccount, bool, error) {
	ordinal := instance.Spec.Ordinal
	var serviceAccount *corev1.ServiceAccount
	ready := true
	for _, desired := range []client.Object{
		dpuagent.DesiredServiceAccount(fleet, ordinal),
		dpuagent.DesiredRole(fleet, ordinal),
		dpuagent.DesiredRoleBinding(fleet, ordinal),
	} {
		current, kind, err := dpuagent.EmptyCredentialResource(desired)
		if err != nil {
			return nil, false, err
		}
		live, err := r.ensureOwnedChild(ctx, instance, desired, current, kind)
		if err != nil {
			return nil, false, err
		}
		if live == nil {
			ready = false
			continue
		}
		if account, ok := live.(*corev1.ServiceAccount); ok {
			serviceAccount = account
		}
	}
	return serviceAccount, ready, nil
}

// ensureInstanceCloudInitSecret creates the create-only cloud-init Secret.
// Its Get precedes token issuance because the Secret is the durable record
// that this ordinal already holds a credential; an existing one only has its
// identity labels repaired. A cache miss is confirmed against the API server,
// since the cache lags a fresh create and drops a Secret that lost its labels,
// and either would otherwise mint a second token.
func (r *SimInstanceReconciler) ensureInstanceCloudInitSecret(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
	serviceAccount *corev1.ServiceAccount,
) (bool, error) {
	ordinal := instance.Spec.Ordinal
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: naming.CloudInitSecretName(fleet.Name, ordinal), Namespace: fleet.Namespace}
	found, err := r.getChild(ctx, key, secret, "Secret")
	if err == nil && !found {
		found, err = r.getLiveChild(ctx, key, secret, "Secret")
	}
	if err != nil {
		return false, err
	}
	if found {
		return true, r.syncChildLabels(ctx, instance, secret, cloudinit.SecretLabels(fleet, ordinal), "Secret")
	}
	desired, err := r.renderInstanceCloudInitSecret(ctx, fleet, ordinal, serviceAccount)
	if err != nil || desired == nil {
		return false, err
	}
	live, err := r.createChild(ctx, instance, desired, &corev1.Secret{}, "Secret")
	return live != nil, err
}

// renderInstanceCloudInitSecret returns nil while the ServiceAccount the
// token is issued for is not in place yet.
func (r *SimInstanceReconciler) renderInstanceCloudInitSecret(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	serviceAccount *corev1.ServiceAccount,
) (*corev1.Secret, error) {
	if !dpuagent.Enabled(fleet) {
		return cloudinit.DesiredSecret(fleet, ordinal, nil)
	}
	if serviceAccount == nil || serviceAccount.UID == "" {
		return nil, nil
	}
	// Everything but the token is validated first, so a payload that can
	// never be written costs no credential.
	preflight, err := dpuagent.ManagementKubeconfig([]byte("preflight"), r.ManagementCA)
	if err != nil {
		return nil, err
	}
	if _, err := cloudinit.DesiredSecret(fleet, ordinal, preflight); err != nil {
		return nil, err
	}
	if r.ServiceAccountTokenIssuer == nil {
		return nil, errors.New("management ServiceAccount token issuer is not configured")
	}
	token, err := r.ServiceAccountTokenIssuer.Issue(ctx, fleet.Namespace, dpuagent.CredentialName(fleet, ordinal))
	if err != nil {
		return nil, fmt.Errorf("could not issue per-DPU management credentials: %w", err)
	}
	kubeconfig, err := dpuagent.ManagementKubeconfig(token, r.ManagementCA)
	if err != nil {
		return nil, err
	}
	return cloudinit.DesiredSecret(fleet, ordinal, kubeconfig)
}

func (r *SimInstanceReconciler) ensureInstanceDevice(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (*provisioningv1.DPUDevice, error) {
	live, err := r.ensureOwnedChild(ctx, instance,
		dpf.DesiredDevice(fleet, instance.Spec.Ordinal), &provisioningv1.DPUDevice{}, "DPUDevice")
	if err != nil {
		return nil, err
	}
	device, _ := live.(*provisioningv1.DPUDevice)
	if device == nil {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceDeviceCreated, false,
			"Progressing", "DPUDevice is being created")
		return nil, nil
	}
	setInstanceCondition(instance, scalev1alpha1.SimInstanceDeviceCreated, true,
		"Created", "DPUDevice is in place")
	return device, nil
}

// ensureInstanceVirtualMachine creates the guest once its device and
// cloud-init Secret exist; a VirtualMachine without its Secret would boot
// with no identity.
func (r *SimInstanceReconciler) ensureInstanceVirtualMachine(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
	cloudInitReady bool,
	device *provisioningv1.DPUDevice,
) (*virtv1.VirtualMachine, error) {
	tmpl := fleet.Spec.VirtualMachineTemplate
	if tmpl != nil {
		if err := kubevirt.ValidateTemplate(tmpl); err != nil {
			return nil, err
		}
	}
	switch {
	case !instanceConditionTrue(instance, scalev1alpha1.SimInstanceCredentialsReady):
		setInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, false,
			"WaitingForCredentials", "DPU agent credentials are not in place")
		return nil, nil
	case device == nil:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, false,
			"WaitingForDevice", "DPUDevice is not in place")
		return nil, nil
	case tmpl == nil:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, true,
			"Created", "Every child is in place")
		return nil, nil
	case !cloudInitReady:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, false,
			"WaitingForCloudInit", "cloud-init Secret is not in place")
		return nil, nil
	}
	live, err := r.ensureOwnedChild(ctx, instance,
		kubevirt.DesiredVirtualMachine(fleet, instance.Spec.Ordinal), &virtv1.VirtualMachine{}, "VirtualMachine")
	if err != nil {
		return nil, err
	}
	vm, _ := live.(*virtv1.VirtualMachine)
	if vm == nil {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, false,
			"Progressing", "VirtualMachine is being created")
		return nil, nil
	}
	setInstanceCondition(instance, scalev1alpha1.SimInstanceChildrenCreated, true,
		"Created", "Every child is in place")
	return vm, nil
}

// getInstanceVMI returns the VirtualMachineInstance KubeVirt runs for vm, or
// nil when there is none.
func (r *SimInstanceReconciler) getInstanceVMI(
	ctx context.Context,
	vm *virtv1.VirtualMachine,
) (*virtv1.VirtualMachineInstance, error) {
	if vm == nil {
		return nil, nil
	}
	vmi := &virtv1.VirtualMachineInstance{}
	found, err := r.getChild(ctx, client.ObjectKeyFromObject(vm), vmi, "VirtualMachineInstance")
	if err != nil || !found {
		return nil, err
	}
	return vmi, nil
}

// guestBelongsToInstance checks the whole ownership chain of a guest: the
// VMI must be controlled by this instance's VirtualMachine, by name and UID,
// and that VirtualMachine by this instance. Its address becomes the Redfish
// endpoint DPF authenticates against, so a copied name or label is not enough.
func guestBelongsToInstance(
	instance *scalev1alpha1.SimInstance,
	vm *virtv1.VirtualMachine,
	vmi *virtv1.VirtualMachineInstance,
) bool {
	return vm != nil && vmi != nil &&
		ownership.ControlledBy(vm, scalev1alpha1.GroupVersion.String(), "SimInstance", instance.Name, instance.UID) &&
		ownership.ControlledBy(vmi, virtv1.GroupVersion.String(), "VirtualMachine", vm.Name, vm.UID)
}

// ensureInstanceBMCIP publishes the guest's BMC address and the selection
// labels in one patch: a device DPF can select before it has an address sends
// DPF at nothing. The guest's address is the
// source of truth, so a different address already on the device is replaced.
func (r *SimInstanceReconciler) ensureInstanceBMCIP(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
	children instanceChildren,
) error {
	device := children.device
	switch {
	case fleet.Spec.VirtualMachineTemplate == nil:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceSelectable, false,
			"NoVirtualMachine", "No guest serves this device's BMC")
		return nil
	case device == nil:
		setInstanceCondition(instance, scalev1alpha1.SimInstanceSelectable, false,
			"WaitingForDevice", "DPUDevice is not in place")
		return nil
	}
	log := logf.FromContext(ctx)
	address := ""
	if guestBelongsToInstance(instance, children.vm, children.vmi) {
		address = kubevirt.GuestBridgedIPv4(children.vmi)
	} else if children.vmi != nil {
		log.V(1).Info("Ignored a VirtualMachineInstance this SimInstance does not own",
			"virtualMachineInstance", children.vmi.Name)
	}
	if address != "" {
		base := device.DeepCopy()
		device.Spec.BMCIP = &address
		mergeLabels(device, dpf.DeviceSelectionLabels(fleet))
		mergeLabels(device, dpf.DeviceLabels(fleet, instance.Spec.Ordinal))
		previous := ptr.Deref(base.Spec.BMCIP, "")
		if !inventoryMetadataUnchanged(base, device) || previous != address {
			if err := r.Patch(ctx, device, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("patch DPUDevice %q BMC address: %w", device.Name, err)
			}
			if previous != "" && previous != address {
				log.Info("Replaced DPUDevice BMC address",
					"dpuDevice", device.Name, "previousBMCIP", previous, "bmcIP", address)
			} else {
				log.V(1).Info("Published DPUDevice BMC address", "dpuDevice", device.Name, "bmcIP", address)
			}
		}
	}
	if !deviceSelectable(fleet, device) {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceSelectable, false,
			"WaitingForGuestAddress", "The guest has not reported a bridged IPv4 address")
		return nil
	}
	setInstanceCondition(instance, scalev1alpha1.SimInstanceSelectable, true,
		"Published", "DPUDevice has a BMC address and the selection labels")
	return nil
}

func deviceSelectable(fleet *scalev1alpha1.SimFleet, device *provisioningv1.DPUDevice) bool {
	if ptr.Deref(device.Spec.BMCIP, "") == "" {
		return false
	}
	for key, value := range dpf.DeviceSelectionLabels(fleet) {
		if device.Labels[key] != value {
			return false
		}
	}
	return true
}

func observeInstanceVM(fleet *scalev1alpha1.SimFleet, instance *scalev1alpha1.SimInstance, children instanceChildren) {
	if fleet.Spec.VirtualMachineTemplate == nil {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceGuestReady, true,
			"NotRequired", "No guest boots from this instance")
		return
	}
	ready := children.vm != nil && children.vm.Status.Ready
	if !ready && guestBelongsToInstance(instance, children.vm, children.vmi) {
		status, _ := vmiReadyStatus(children.vmi)
		ready = status == corev1.ConditionTrue
	}
	if !ready {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceGuestReady, false,
			"WaitingForVM", "VirtualMachine is not ready")
		return
	}
	setInstanceCondition(instance, scalev1alpha1.SimInstanceGuestReady, true, "Ready", "VirtualMachine is ready")
}

// observeInstanceDPU finds the DPU DPF created for this instance's device.
func (r *SimInstanceReconciler) observeInstanceDPU(
	ctx context.Context,
	fleet *scalev1alpha1.SimFleet,
	instance *scalev1alpha1.SimInstance,
) (*provisioningv1.DPU, error) {
	deviceName := naming.DeviceName(fleet.Name, instance.Spec.Ordinal)
	list := &provisioningv1.DPUList{}
	if err := r.List(ctx, list,
		client.InNamespace(instance.Namespace),
		client.MatchingFields{dpuDeviceField: deviceName},
	); err != nil {
		return nil, fmt.Errorf("list DPUs for DPUDevice %q: %w", deviceName, err)
	}
	switch len(list.Items) {
	case 0:
		instance.Status.DPUName = ""
		setInstanceCondition(instance, scalev1alpha1.SimInstanceDPUProvisioned, false,
			"DPUMissing", "DPF has not created a DPU for this device")
		return nil, nil
	case 1:
	default:
		instance.Status.DPUName = ""
		setInstanceCondition(instance, scalev1alpha1.SimInstanceDPUProvisioned, false,
			"DuplicateDPU", fmt.Sprintf("%d DPUs reference DPUDevice %q", len(list.Items), deviceName))
		return nil, nil
	}
	dpu := &list.Items[0]
	instance.Status.DPUName = dpu.Name
	if !kubeletConfigured(dpu) {
		setInstanceCondition(instance, scalev1alpha1.SimInstanceDPUProvisioned, false,
			"WaitingForKubelet", "DPU kubelet is not configured")
		return dpu, nil
	}
	setInstanceCondition(instance, scalev1alpha1.SimInstanceDPUProvisioned, true,
		"KubeletConfigured", "DPU kubelet is configured")
	return dpu, nil
}

func kubeletConfigured(dpu *provisioningv1.DPU) bool {
	return dpu != nil && dpu.Status.AgentStatus != nil &&
		meta.IsStatusConditionTrue(dpu.Status.AgentStatus.Conditions, "KubeletConfigured")
}

// ensureOwnedChild creates desired owned by instance, or repairs the identity
// labels of the existing object after checking it is this instance's. It
// returns the live object, or nil when a create raced a cache that has not
// seen the object yet; that object's create event requeues the instance.
func (r *SimInstanceReconciler) ensureOwnedChild(
	ctx context.Context,
	instance *scalev1alpha1.SimInstance,
	desired, current client.Object,
	kind string,
) (client.Object, error) {
	found, err := r.getChild(ctx, client.ObjectKeyFromObject(desired), current, kind)
	if err != nil {
		return nil, err
	}
	if found {
		return current, r.syncChildLabels(ctx, instance, current, desired.GetLabels(), kind)
	}
	return r.createChild(ctx, instance, desired, current, kind)
}

func (r *SimInstanceReconciler) getChild(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	kind string,
) (bool, error) {
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get %s %q: %w", kind, key.Name, err)
	}
	return true, nil
}

// getLiveChild is getChild against the API server rather than the cache.
func (r *SimInstanceReconciler) getLiveChild(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	kind string,
) (bool, error) {
	if err := r.APIReader.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get %s %q from the API server: %w", kind, key.Name, err)
	}
	return true, nil
}

// labelScopedCacheKind reports whether ManagerCacheOptions caches obj's kind
// only while it carries the fleet label.
func labelScopedCacheKind(obj client.Object) bool {
	switch obj.(type) {
	case *corev1.Secret, *corev1.ServiceAccount, *rbacv1.Role, *rbacv1.RoleBinding:
		return true
	}
	return false
}

// createChild treats AlreadyExists as the child already being in place, once
// it is proven to be this instance's. One of the label-scoped kinds is read from
// the API server, because the cache never holds it without its labels, and
// the label repair is what brings it back into the cache.
func (r *SimInstanceReconciler) createChild(
	ctx context.Context,
	instance *scalev1alpha1.SimInstance,
	desired, current client.Object,
	kind string,
) (client.Object, error) {
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return nil, fmt.Errorf("set %s %q owner: %w", kind, desired.GetName(), err)
	}
	if err := r.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create %s %q: %w", kind, desired.GetName(), err)
		}
		get := r.getChild
		if labelScopedCacheKind(current) {
			get = r.getLiveChild
		}
		found, err := get(ctx, client.ObjectKeyFromObject(desired), current, kind)
		if err != nil || !found {
			return nil, err
		}
		return current, r.syncChildLabels(ctx, instance, current, desired.GetLabels(), kind)
	}
	logf.FromContext(ctx).V(1).Info("Created SimInstance child", "kind", kind, "name", desired.GetName())
	return desired, nil
}

// syncChildLabels never touches spec or data: DPF fills in DPUDevice spec and
// the cloud-init payload is create-only.
func (r *SimInstanceReconciler) syncChildLabels(
	ctx context.Context,
	instance *scalev1alpha1.SimInstance,
	obj client.Object,
	labels map[string]string,
	kind string,
) error {
	if err := ownership.ValidateControlledBy(obj, instance, "SimInstance", kind); err != nil {
		return err
	}
	base := obj.DeepCopyObject().(client.Object)
	mergeLabels(obj, labels)
	if inventoryMetadataUnchanged(base, obj) {
		return nil
	}
	if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch %s %q: %w", kind, obj.GetName(), err)
	}
	return nil
}
