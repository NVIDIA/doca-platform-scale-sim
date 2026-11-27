//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

const (
	namespace = "simfleet-controller-system"
	// dpfOperatorNamespace matches DPF_OPERATOR_NAMESPACE in hack/e2e/deploy-dpf.sh.
	dpfOperatorNamespace = "dpf-operator-system"
	// fleetNamespace is the DPF operator namespace. DPF looks up the BMC CA
	// bundle and namespaced Issuer in the DPUDevice's namespace, and a
	// SimFleet cannot own inventory across namespaces, so the fleet has to
	// live where DPF already issued that PKI.
	fleetNamespace = dpfOperatorNamespace
	// bfbFixturesImage matches BFB_FIXTURES_IMAGE in the Makefile. It is the
	// busybox the fixture server runs, already loaded on the Kind node, and
	// the diagnostics reuse it rather than pull anything of their own.
	bfbFixturesImage = "simfleet-e2e/bfb-fixtures:busybox"
	fleetName        = "e2e-fleet"
	fleetLabel       = "scale.dpu.nvidia.com/fleet-name=" + fleetName
	// replicas is the size the fleet is created at. Diagnostics keep ranging
	// over it after the scale-down: an ordinal that was scaled away but not
	// torn down is exactly what a failed scale-down has to show.
	replicas = 2
	// scaledReplicas is the size the scale-down spec shrinks the fleet to.
	scaledReplicas = 1
	// replacedOrdinal is the ordinal whose VirtualMachine the replacement
	// spec deletes, and the one the scale-down then removes.
	replacedOrdinal = 1
	// devicesPerDPUNode matches testdata/simfleet.yaml. DPF names each DPU
	// {host}-{device} from those two values, and the suite has to use the
	// same names: a namespace-wide DPU list would pass on leftovers from a
	// reused Kind cluster.
	devicesPerDPUNode = 1
	nodes             = 2

	// delegatedIPTimeout bounds the wait for a bridged guest address. It is
	// short on purpose: by the time it is used the guest agent has already
	// reported in, which means the guest is booted and its DHCP lease is
	// either taken or seconds away. A boot-sized budget here would only turn
	// a broken delegation into a very slow failure.
	delegatedIPTimeout = 2 * time.Minute

	// bfbReadyTimeout bounds the BFB download. The bundle is the few-kilobyte
	// fixture served by hack/e2e/bfb-fixtures.yaml from inside the cluster,
	// so anything beyond a DPF reconcile or two means the download is not
	// happening at all rather than that it is slow.
	bfbReadyTimeout = 5 * time.Minute

	// instanceTeardownTimeout bounds one SimInstance going from Failed or
	// deleted to gone. A step blocked on another controller's finalizer
	// strips it after the controller's 5m lifecycleForceTimeout; the extra
	// minute is the fleet noticing and the requeues around that. It allows
	// for one forced step: two means DPF is not processing deletions at all.
	instanceTeardownTimeout = 6 * time.Minute

	// fleetDeletionTraceInterval is how often the fleet deletion spec writes
	// what is still left, so a slow deletion shows which step it sat in.
	fleetDeletionTraceInterval = 30 * time.Second

	tenantControlPlanePatch = `{"spec":{"controlPlane":{"deployment":{"replicas":1,"resources":{` +
		`"apiServer":{"requests":{"cpu":"100m","memory":"256Mi"}},` +
		`"controllerManager":{"requests":{"cpu":"50m","memory":"128Mi"}},` +
		`"scheduler":{"requests":{"cpu":"50m","memory":"64Mi"}}}}}}}`
)

var _ = Describe("SimFleet", Ordered, func() {
	AfterAll(func() {
		cleanupFleetInventory(fleetNamespace, fleetName)
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}

		podName := controllerPodName()

		By("Fetching controller manager pod logs")
		var cmd *exec.Cmd
		if podName != "" {
			cmd = exec.Command("kubectl", "logs", podName, "-n", namespace)
			controllerLogs, err := run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}
		}

		By("Fetching Kubernetes events")
		cmd = exec.Command("kubectl", "get", "events", "-n", fleetNamespace, "--sort-by=.lastTimestamp")
		eventsOutput, err := run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
		}

		for ordinal := range replicas {
			vmName := naming.VirtualMachineName(fleetName, ordinal)
			By("Fetching VirtualMachine description")
			cmd = exec.Command("kubectl", "describe", "virtualmachine", vmName, "-n", fleetNamespace)
			vmDescription, err := run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "VirtualMachine %s description:\n%s", vmName, vmDescription)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe VirtualMachine %s: %s", vmName, err)
			}
		}

		for _, resource := range []string{"dpucluster", "tenantcontrolplane"} {
			By("Fetching " + resource + " description")
			cmd = exec.Command("kubectl", "describe", resource, "e2e-tenant", "-n", fleetNamespace)
			description, err := run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "%s description:\n%s", resource, description)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe %s: %s", resource, err)
			}
		}

		By("Fetching controller manager pod description")
		if podName != "" {
			cmd = exec.Command("kubectl", "describe", "pod", podName, "-n", namespace)
			podDescription, err := run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Pod description:\n %s", podDescription)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe controller pod: %s", err)
			}
		}

		dumpBFBFixtureDiagnostics()
		dumpDPUAgentDiagnostics()
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("creates DPUDevices and DPUNodes from a SimFleet", func() {
		By("applying the SimFleet")
		// DPF's CRDs and admission webhooks for BFB, DPUFlavor and DPUSet can
		// lag the DPFOperatorConfig going Ready, so retry until every object
		// applies. The fleet lives in dpf-operator-system, which the DPF
		// install already created.
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "apply", "-f", "testdata")
			_, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to apply e2e testdata")
		}).Should(Succeed())

		By("applying the DPUCluster")
		dpuCluster, err := renderDPUCluster()
		Expect(err).NotTo(HaveOccurred(), "Failed to render the DPUCluster")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(dpuCluster)
		_, err = run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the DPUCluster")

		// DPF creates the TenantControlPlane with three replicas requesting
		// 500m and 1Gi each, which does not schedule next to the guests on a
		// 2-vCPU, 8 GB runner. It never reconciles replicas or resources after
		// creation, so the patch sticks.
		By("shrinking the tenant control plane to one replica")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "patch", "tenantcontrolplane", "e2e-tenant", "-n", fleetNamespace,
				"--type=merge", "-p", tenantControlPlanePatch)
			_, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to patch the TenantControlPlane")
		}).Should(Succeed())

		By("waiting for DPUDevices")
		Eventually(func(g Gomega) {
			names, err := listedNames("dpudevices", fleetNamespace, fleetLabel)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(names).To(HaveLen(replicas))
		}).Should(Succeed())

		By("waiting for DPUNodes")
		Eventually(func(g Gomega) {
			names, err := listedNames("dpunodes", fleetNamespace, fleetLabel)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(names).To(HaveLen(nodes))
		}).Should(Succeed())

		By("validating the first DPUDevice")
		deviceName := fleetName + "-device-000000"
		Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.spec.serialNumber}")).To(Equal(naming.SerialNumber(fleetName, 0)))
		// The DPUSet selector must not match yet. DPF opens Redfish against
		// spec.bmcIP as soon as it claims a device, and no guest has reported
		// an address at this point.
		Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace,
			`{.metadata.labels['scale\.dpu\.nvidia\.com/pool']}`)).To(BeEmpty())
		Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.spec.bmcIp}")).To(BeEmpty())
		Expect(objectLabel("dpudevice", deviceName, fleetNamespace, "provisioning.dpu.nvidia.com/dpunode-name")).To(Equal(fleetName + "-host-0000"))
		Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.metadata.ownerReferences[0].kind}")).To(Equal("SimInstance"))
		Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.metadata.ownerReferences[0].name}")).To(Equal(naming.InstanceName(fleetName, 0)))

		By("validating the first DPUNode")
		nodeName := fleetName + "-host-0000"
		Expect(listedJSONPath("dpunode", nodeName, fleetNamespace, "{.spec.dpus[*].name}")).To(HaveLen(1))
		Expect(objectJSONPath("dpunode", nodeName, fleetNamespace, "{.spec.nodeRebootMethod.external}")).NotTo(BeEmpty())
		Expect(objectJSONPath("dpunode", nodeName, fleetNamespace, "{.metadata.ownerReferences[0].kind}")).To(Equal("SimFleet"))

		By("waiting for InventoryReconciled status")
		Eventually(func(g Gomega) {
			admitted, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace, "{.status.admittedReplicas}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(admitted).To(Equal("2"))
			created, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace, "{.status.createdInstances}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(created).To(Equal("2"))
			nodeCount, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace, "{.status.dpuNodes}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(nodeCount).To(Equal("2"))
			ready, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace,
				`{.status.conditions[?(@.type=="InventoryReconciled")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ready).To(Equal("True"))
		}).Should(Succeed())

		By("waiting for the BFB to download from the in-cluster fixture server")
		Eventually(func(g Gomega) {
			phase, err := objectJSONPathErr("bfb", "e2e-bfb", fleetNamespace, "{.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Ready"))
		}, bfbReadyTimeout, 5*time.Second).Should(Succeed())

		// Only once a guest answers on its bridged address does the fleet
		// publish spec.bmcIP and the selection labels, which is what lets the
		// DPUSet claim that device and DPF reach its Redfish BMC. Both
		// ordinals share one timeout: they boot in parallel, and waiting for
		// only the first would give the second the default 2m Eventually
		// window, which is shorter than a guest boot.
		By("waiting for every guest BMC address to make its device selectable")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				expectDeviceSelectable(g, ordinal)
			}
		}, guestAgentTimeout(), 5*time.Second).Should(Succeed())

		By("waiting for DPUSet to create DPUs")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				name := fleetDPUName(ordinal)
				got, err := objectJSONPathErr("dpu", name, fleetNamespace, "{.spec.dpuDeviceName}")
				g.Expect(err).NotTo(HaveOccurred(), "DPU %s is missing", name)
				g.Expect(got).To(Equal(naming.DeviceName(fleetName, ordinal)))
			}
		}, guestAgentTimeout(), 5*time.Second).Should(Succeed())

		By("waiting for DPUsReconciled")
		Eventually(func(g Gomega) {
			// Every instance names its DPU once DPF has created it; kubelet
			// on it, which provisionedInstances counts, comes much later.
			cmd := exec.Command("kubectl", "get", "siminstances", "-n", fleetNamespace, "-l", fleetLabel,
				"-o", "jsonpath={.items[*].status.dpuName}")
			dpuNames, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Fields(dpuNames)).To(HaveLen(replicas))
			ready, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace,
				`{.status.conditions[?(@.type=="DPUsReconciled")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ready).To(Equal("True"))
		}, guestAgentTimeout(), 5*time.Second).Should(Succeed())

		By("waiting for the Kamaji DPUCluster")
		var kubeconfigSecret string
		Eventually(func(g Gomega) {
			phase, err := objectJSONPathErr(
				"dpucluster", "e2e-tenant", fleetNamespace, "{.status.phase}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Ready"))

			kubeconfigSecret, err = objectJSONPathErr(
				"dpucluster", "e2e-tenant", fleetNamespace, "{.spec.kubeconfig}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kubeconfigSecret).NotTo(BeEmpty())
			g.Expect(secretDataKeyExists(kubeconfigSecret, "super-admin.conf")).To(BeTrue())
		}, 10*time.Minute, time.Second).Should(Succeed())

		// One per ordinal, and nothing fleet-wide. The guest reads DPF's
		// per-DPU join Secret itself with the credential inside its own
		// payload, so no tenant bootstrap token is minted for the fleet.
		By("waiting for owned per-ordinal bootstrap Secrets")
		for _, secretName := range []string{
			fleetName + "-dpu-000000-cloud-init",
			fleetName + "-dpu-000001-cloud-init",
		} {
			Eventually(func(g Gomega) {
				ownerKind, err := objectJSONPathErr(
					"secret", secretName, fleetNamespace, "{.metadata.ownerReferences[0].kind}",
				)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ownerKind).To(Equal("SimInstance"))
				g.Expect(secretDataKeyExists(secretName, "userdata")).To(BeTrue())
			}).Should(Succeed())
		}
		Consistently(func(g Gomega) {
			_, err := objectJSONPathErr(
				"secret", fleetName+"-join-discovery", fleetNamespace, "{.metadata.name}",
			)
			g.Expect(err).To(HaveOccurred())
		}, 10*time.Second, time.Second).Should(Succeed())

		By("validating the VirtualMachines")
		vmNames, err := listedNames("virtualmachines", fleetNamespace, fleetLabel)
		Expect(err).NotTo(HaveOccurred())
		Expect(vmNames).To(HaveLen(replicas))

		vmName := fleetName + "-dpu-000000"
		Expect(objectJSONPath("virtualmachine", vmName, fleetNamespace,
			`{.spec.template.spec.volumes[?(@.name=="containerdisk")].containerDisk.image}`)).To(
			Equal("simfleet-node:e2e"))
		Expect(objectJSONPath("virtualmachine", vmName, fleetNamespace,
			"{.spec.template.spec.architecture}")).To(Equal("amd64"))
		Expect(objectJSONPath(
			"virtualmachine", vmName, fleetNamespace,
			`{.spec.template.spec.volumes[?(@.name=="cloud-init")].cloudInitNoCloud.networkData}`,
		)).To(ContainSubstring("dhcp4: true"))
		Expect(objectJSONPath("virtualmachine", vmName, fleetNamespace, "{.metadata.ownerReferences[0].kind}")).To(Equal("SimInstance"))

		By("waiting for VirtualMachines to become Ready")
		Eventually(func(g Gomega) {
			readyCount, err := objectJSONPathErr(
				"simfleet", fleetName, fleetNamespace, "{.status.guestReadyInstances}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyCount).To(Equal("2"))
			readyCondition, err := objectJSONPathErr(
				"simfleet", fleetName, fleetNamespace,
				`{.status.conditions[?(@.type=="GuestsReady")].status}`,
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyCondition).To(Equal("True"))
			firstVMReady, err := objectJSONPathErr(
				"virtualmachine", fleetName+"-dpu-000000", fleetNamespace, "{.status.ready}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(firstVMReady).To(Equal("true"))
			secondVMReady, err := objectJSONPathErr(
				"virtualmachine", fleetName+"-dpu-000001", fleetNamespace, "{.status.ready}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(secondVMReady).To(Equal("true"))
		}, virtualMachineReadyTimeout(), time.Second).Should(Succeed())

		// VirtualMachine Ready only means virt-launcher started the domain. The
		// AgentConnected condition on the VirtualMachineInstance is reported by
		// virt-handler once qemu-guest-agent inside the guest answers on the
		// org.qemu.guest_agent.0 virtio-serial channel, so it is the first
		// signal that the baked image actually boots.
		// Both instances share one timeout: they boot in parallel, and a
		// timeout per instance would let the suite outlive E2E_TEST_TIMEOUT.
		By("waiting for the guest agent in every VirtualMachineInstance")
		Eventually(func(g Gomega) {
			for _, vmiName := range []string{fleetName + "-dpu-000000", fleetName + "-dpu-000001"} {
				agentConnected, err := objectJSONPathErr(
					"virtualmachineinstance", vmiName, fleetNamespace,
					`{.status.conditions[?(@.type=="AgentConnected")].status}`,
				)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(agentConnected).To(Equal("True"), "%s has no connected guest agent", vmiName)
			}
		}, guestAgentTimeout(), 5*time.Second).Should(Succeed())

		dumpGuestVMDiagnostics()

		By("verifying every delegated pod address is unique and published as a BMC address")
		addresses := map[string]struct{}{}
		for ordinal := range replicas {
			vmiName := fmt.Sprintf("%s-dpu-%06d", fleetName, ordinal)
			// SimFleet numbers devices and VirtualMachines from the same
			// ordinal, so device N's BMC is VirtualMachine N's guest.
			device := fmt.Sprintf("%s-device-%06d", fleetName, ordinal)
			// Keep the address that satisfied the assertions rather than
			// reading it again afterwards, so uniqueness is checked against
			// the same snapshot that was validated.
			var delegatedIP string
			Eventually(func(g Gomega) {
				guestIP, launcherIP, err := delegatedIPv4(vmiName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(guestIP).NotTo(BeEmpty())
				g.Expect(guestIP).NotTo(Equal("10.0.2.2"))
				g.Expect(guestIP).To(Equal(launcherIP))

				bmcIP, err := objectJSONPathErr("dpudevice", device, fleetNamespace, "{.spec.bmcIp}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(bmcIP).To(Equal(guestIP),
					"%s publishes BMC address %q, but %s was delegated %q",
					device, bmcIP, vmiName, guestIP)

				delegatedIP = guestIP
			}, delegatedIPTimeout, 5*time.Second).Should(Succeed())

			Expect(addresses).NotTo(HaveKey(delegatedIP),
				"%s was delegated an address another instance already has", vmiName)
			addresses[delegatedIP] = struct{}{}
		}

		By("waiting for every DPU agent to join the tenant cluster")
		tenant, err := tenantClient()
		Expect(err).NotTo(HaveOccurred(), "Failed to build the tenant client")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				dpu, node := expectDPUAgentJoined(g, tenant, fleetDPUName(ordinal))
				expectNodeJoinedInConfigureKubelet(g, dpu, node)
			}
		}, dpuProvisioningTimeout(), 5*time.Second).Should(Succeed())

		// DPUProvisioned follows the same KubeletConfigured condition the
		// agent check just saw, so this is one instance reconcile away.
		By("waiting for every SimInstance to be Ready with every child recorded")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				expectInstanceReady(g, ordinal)
			}
			expectFleetCounts(g, replicas)
		}).Should(Succeed())

		By("verifying every guest's mock DPU agent applied the fleet's phase delays")
		phaseList := objectJSONPath("simfleet", fleetName, fleetNamespace,
			`{range .spec.dpuAgent.phaseDelays[*]}{.phase}{"\n"}{end}`)
		Expect(strings.TrimSpace(phaseList)).NotTo(BeEmpty(), "testdata/simfleet.yaml delays no phase")
		phases := strings.Split(strings.TrimSpace(phaseList), "\n")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				expectPhaseDelaysApplied(g, naming.VirtualMachineName(fleetName, ordinal), phases)
			}
		}, time.Minute, 5*time.Second).Should(Succeed())

		// Last on purpose: every check above is about what this controller
		// owns, and this one is about DPF finishing the DPUs after the join.
		By("waiting for every DPU to reach the Ready phase")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				dpu, err := getDPU(fleetDPUName(ordinal))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dpu.Status.Phase).To(Equal(provisioningv1.DPUReady), "DPU %s phase", dpu.Name)
			}
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("replaces an ordinal whose VirtualMachine was deleted", func() {
		instanceName := naming.InstanceName(fleetName, replacedOrdinal)
		vmName := naming.VirtualMachineName(fleetName, replacedOrdinal)
		dpuName := fleetDPUName(replacedOrdinal)

		By("recording the identities the replacement must change")
		kept, err := getInstanceIdentity(0)
		Expect(err).NotTo(HaveOccurred())
		replaced, err := getInstanceIdentity(replacedOrdinal)
		Expect(err).NotTo(HaveOccurred())
		staleDPUUID := objectJSONPath("dpu", dpuName, fleetNamespace, "{.metadata.uid}")
		Expect(staleDPUUID).To(Equal(replaced.dpu))
		staleServiceAccountUID := objectJSONPath("serviceaccount", dpuName, fleetNamespace, "{.metadata.uid}")
		Expect(staleServiceAccountUID).NotTo(BeEmpty())
		tenant, err := tenantClient()
		Expect(err).NotTo(HaveOccurred(), "Failed to build the tenant client")
		staleBootID, err := tenantNodeBootID(tenant, dpuName)
		Expect(err).NotTo(HaveOccurred(), "Failed to read tenant Node %s", dpuName)
		Expect(staleBootID).NotTo(BeEmpty())

		By("deleting the VirtualMachine of ordinal " + strconv.Itoa(replacedOrdinal))
		cmd := exec.Command("kubectl", "delete", "virtualmachine", vmName, "-n", fleetNamespace, "--wait=false")
		_, err = run(cmd)
		Expect(err).NotTo(HaveOccurred())

		// The Failed phase lasts only until the fleet deletes the instance,
		// so it is too short-lived to poll for. The event is what records it.
		By("waiting for the fleet to fail and recreate the SimInstance")
		Eventually(func(g Gomega) {
			uid, err := objectJSONPathErr("siminstance", instanceName, fleetNamespace, "{.metadata.uid}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(uid).NotTo(BeEmpty())
			g.Expect(uid).NotTo(Equal(replaced.uid), "%s has not been replaced", instanceName)

			cmd := exec.Command("kubectl", "get", "events", "-n", fleetNamespace,
				"--field-selector", "reason=SimInstanceFailed,involvedObject.uid="+replaced.uid,
				"-o", "jsonpath={.items[*].message}")
			messages, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			// KubeVirt holds the VirtualMachine until its VMI is gone, so
			// drift can be seen on either one first.
			g.Expect(messages).To(MatchRegexp(`VirtualMachine(Instance)?Replaced`),
				"no SimInstanceFailed event for the replaced %s", instanceName)
		}, instanceTeardownTimeout, 5*time.Second).Should(Succeed())

		// The new instance starts from nothing: a fresh guest boot, the
		// Redfish flow, and the join, which is what dpuProvisioningTimeout
		// allows for.
		By("waiting for the replacement to be Ready with a rotated credential and a rejoined agent")
		Eventually(func(g Gomega) {
			current := expectInstanceReady(g, replacedOrdinal)
			g.Expect(current.uid).NotTo(Equal(replaced.uid))
			g.Expect(current.virtualMachine).NotTo(Equal(replaced.virtualMachine))
			g.Expect(current.dpu).NotTo(Equal(replaced.dpu))

			vmUID, err := objectJSONPathErr("virtualmachine", vmName, fleetNamespace, "{.metadata.uid}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(vmUID).To(Equal(current.virtualMachine))
			dpuUID, err := objectJSONPathErr("dpu", dpuName, fleetNamespace, "{.metadata.uid}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dpuUID).To(Equal(current.dpu))
			serviceAccountUID, err := objectJSONPathErr("serviceaccount", dpuName, fleetNamespace, "{.metadata.uid}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(serviceAccountUID).NotTo(BeEmpty())
			g.Expect(serviceAccountUID).NotTo(Equal(staleServiceAccountUID), "ServiceAccount %s was not rotated", dpuName)

			expectDPUAgentRejoined(g, tenant, dpuName, staleDPUUID, staleBootID)
			expectFleetCounts(g, replicas)
		}, dpuProvisioningTimeout(), 5*time.Second).Should(Succeed())

		// UIDs are never reused, so ordinal 0 matching its snapshot now
		// means it was not replaced at any point in between.
		By("verifying ordinal 0 was left alone")
		Expect(getInstanceIdentity(0)).To(Equal(kept))

		By("verifying the replaced ordinal was torn down without forcing a finalizer")
		expectNoForcedTeardown()

		// Last on purpose: every check above is about what this controller
		// owns, and this one is about DPF finishing the DPUs after the join.
		By("waiting for the replacement DPU to reach the Ready phase")
		Eventually(func(g Gomega) {
			dpu, err := getDPU(dpuName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dpu.Status.Phase).To(Equal(provisioningv1.DPUReady), "DPU %s phase", dpu.Name)
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("scales a fleet down and removes the scaled-away ordinal", func() {
		kept, err := getInstanceIdentity(0)
		Expect(err).NotTo(HaveOccurred())

		By("scaling the SimFleet down to " + strconv.Itoa(scaledReplicas))
		cmd := exec.Command("kubectl", "patch", "simfleet", fleetName, "-n", fleetNamespace,
			"--type=merge", "-p", fmt.Sprintf(`{"spec":{"replicas":%d}}`, scaledReplicas))
		_, err = run(cmd)
		Expect(err).NotTo(HaveOccurred())

		// DPUNodes stay: the fleet does not remove them on a scale-down.
		By("waiting for the scaled-away ordinal and every child of it to be gone")
		Eventually(func(g Gomega) {
			expectOrdinalGone(g, replacedOrdinal)

			admitted, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace, "{.status.admittedReplicas}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(admitted).To(Equal(strconv.Itoa(scaledReplicas)))
			ready, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace,
				`{.status.conditions[?(@.type=="InventoryReconciled")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ready).To(Equal("True"))
		}, instanceTeardownTimeout, 5*time.Second).Should(Succeed())

		By("verifying ordinal 0 was left alone and is still Ready")
		Eventually(func(g Gomega) {
			g.Expect(expectInstanceReady(g, 0)).To(Equal(kept))
		}).Should(Succeed())

		By("verifying the scale-down was torn down without forcing a finalizer")
		expectNoForcedTeardown()
	})

	It("deletes a fleet through its SimInstance finalizers", func() {
		By("deleting the SimFleet without stripping any finalizer")
		started := time.Now()
		DeferCleanup(func() { dumpFleetDeletionDiagnostics(started) })
		cmd := exec.Command("kubectl", "delete", "simfleet", fleetName, "-n", fleetNamespace, "--wait=false")
		_, err := run(cmd)
		Expect(err).NotTo(HaveOccurred())

		// The fleet's finalizer holds it while the controller deletes its
		// SimInstances, each of which tears its ordinal down before it goes.
		// Garbage collection deletes the DPUNodes once the fleet is gone.
		By("waiting for every SimInstance, DPUNode, and ordinal child to be gone")
		var lastTrace time.Time
		Eventually(func(g Gomega) {
			if time.Since(lastTrace) >= fleetDeletionTraceInterval {
				traceFleetDeletion(started)
				lastTrace = time.Now()
			}
			// The fleet is read first, so instances listed after it was
			// found gone really did outlive it.
			fleet, err := run(exec.Command("kubectl", "get", "simfleet", fleetName, "-n", fleetNamespace,
				"--ignore-not-found", "-o", "jsonpath={.metadata.name} {.metadata.deletionTimestamp}"))
			g.Expect(err).NotTo(HaveOccurred())
			instances, err := listedNames("siminstances", fleetNamespace, fleetLabel)
			g.Expect(err).NotTo(HaveOccurred())
			if len(instances) > 0 {
				if strings.TrimSpace(fleet) == "" {
					StopTrying(fmt.Sprintf("SimFleet %s is gone while SimInstances %v remain", fleetName, instances)).Now()
				}
				g.Expect(strings.Fields(fleet)).To(HaveLen(2), "SimFleet %s has no deletionTimestamp", fleetName)
			}

			expectGone(g, "simfleet", fleetName)
			for _, resource := range []string{"siminstances", "dpunodes", "dpudevices", "virtualmachines"} {
				names, err := listedNames(resource, fleetNamespace, fleetLabel)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(names).To(BeEmpty(), "%s left behind by the SimFleet deletion", resource)
			}
			for ordinal := range scaledReplicas {
				expectOrdinalGone(g, ordinal)
			}
		}, instanceTeardownTimeout, 5*time.Second).Should(Succeed())

		By("verifying the fleet was torn down without forcing a finalizer")
		expectNoForcedTeardown()
	})
})

var (
	emulationOnce sync.Once
	useEmulation  bool
)

// softwareEmulationEnabled reports whether KubeVirt runs guests under QEMU TCG.
// deploy-kubevirt.sh sets this exactly when the Kind node has no /dev/kvm.
func softwareEmulationEnabled() bool {
	emulationOnce.Do(func() {
		cmd := exec.Command("kubectl", "get", "kubevirt", "kubevirt",
			"-n", "kubevirt",
			"-o", "jsonpath={.spec.configuration.developerConfiguration.useEmulation}",
		)
		output, err := run(cmd)
		Expect(err).NotTo(HaveOccurred(),
			"could not read KubeVirt useEmulation; treating that as KVM would under-budget a TCG host")
		useEmulation = strings.TrimSpace(output) == "true"
	})
	return useEmulation
}

func fleetDPUName(ordinal int) string {
	return naming.DPUName(fleetName, devicesPerDPUNode, ordinal)
}

func expectDeviceSelectable(g Gomega, ordinal int) {
	vmiName := naming.VirtualMachineName(fleetName, ordinal)
	device := naming.DeviceName(fleetName, ordinal)
	// The delegated address, not just whatever the VMI reports: DPF
	// opens Redfish against spec.bmcIP from a pod, so an address the
	// virt-launcher pod does not own is unreachable there.
	guestIP, launcherIP, err := delegatedIPv4(vmiName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(guestIP).NotTo(BeEmpty())
	g.Expect(guestIP).To(Equal(launcherIP))

	bmcIP, err := objectJSONPathErr("dpudevice", device, fleetNamespace, "{.spec.bmcIp}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(bmcIP).To(Equal(guestIP))

	pool, err := objectJSONPathErr("dpudevice", device, fleetNamespace,
		`{.metadata.labels['scale\.dpu\.nvidia\.com/pool']}`)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pool).To(Equal("e2e"))
}

// instanceIdentity is what a SimInstance nothing happened to keeps: its own
// UID and the UIDs of the children it recorded.
type instanceIdentity struct {
	uid, dpu, dpuDevice, virtualMachine, virtualMachineInstance string
}

// getInstanceIdentity fails unless every child UID has been recorded, so a
// match against an earlier snapshot cannot be two empty values.
func getInstanceIdentity(ordinal int) (instanceIdentity, error) {
	name := naming.InstanceName(fleetName, ordinal)
	output, err := objectJSONPathErr("siminstance", name, fleetNamespace,
		"{.metadata.uid} {.status.observedUIDs.dpu} {.status.observedUIDs.dpuDevice} "+
			"{.status.observedUIDs.virtualMachine} {.status.observedUIDs.virtualMachineInstance}")
	if err != nil {
		return instanceIdentity{}, err
	}
	fields := strings.Fields(output)
	if len(fields) != 5 {
		return instanceIdentity{}, fmt.Errorf("SimInstance %s has not recorded every child UID: %q", name, output)
	}
	return instanceIdentity{
		uid:                    fields[0],
		dpu:                    fields[1],
		dpuDevice:              fields[2],
		virtualMachine:         fields[3],
		virtualMachineInstance: fields[4],
	}, nil
}

func expectInstanceReady(g Gomega, ordinal int) instanceIdentity {
	name := naming.InstanceName(fleetName, ordinal)
	phase, err := objectJSONPathErr("siminstance", name, fleetNamespace, "{.status.phase}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(phase).To(Equal("Ready"), "SimInstance %s is not Ready", name)
	provisioned, err := objectJSONPathErr("siminstance", name, fleetNamespace,
		`{.status.conditions[?(@.type=="DPUProvisioned")].status}`)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(provisioned).To(Equal("True"), "SimInstance %s DPUProvisioned is not True", name)
	identity, err := getInstanceIdentity(ordinal)
	g.Expect(err).NotTo(HaveOccurred())
	return identity
}

// expectFleetCounts reads failedInstances as unset or zero: the field is
// omitted when it is zero.
func expectFleetCounts(g Gomega, provisioned int) {
	got, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace, "{.status.provisionedInstances}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(strconv.Itoa(provisioned)))
	failed, err := objectJSONPathErr("simfleet", fleetName, fleetNamespace, "{.status.failedInstances}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(failed).To(BeElementOf("", "0"))
}

// expectNoForcedTeardown fails when any teardown step had to strip another
// controller's finalizer. A deadline-forced step still ends with the object
// gone, so the waits above cannot tell it from a clean teardown.
func expectNoForcedTeardown() {
	cmd := exec.Command("kubectl", "get", "events", "-n", fleetNamespace,
		"--field-selector", "reason=LifecycleForceDelete",
		"-o", "go-template={{ range .items }}{{ .message }}{{ \"\\n\" }}{{ end }}")
	output, err := run(cmd)
	Expect(err).NotTo(HaveOccurred())
	Expect(nonEmptyLines(output)).To(BeEmpty(), "teardown stripped finalizers instead of completing")
}

// controllerPodName is the running controller manager pod, or empty when it
// cannot be found.
func controllerPodName() string {
	cmd := exec.Command("kubectl", "get",
		"pods", "-l", "control-plane=controller-manager",
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}{{ \"\\n\" }}{{ end }}{{ end }}",
		"-n", namespace,
	)
	output, err := run(cmd)
	if err != nil {
		return ""
	}
	if names := nonEmptyLines(output); len(names) > 0 {
		return names[0]
	}
	return ""
}

// traceFleetDeletion writes which of the fleet's objects still exist, with
// what holds each one, in five kubectl calls.
func traceFleetDeletion(started time.Time) {
	const meta = " deletionTimestamp={{ .metadata.deletionTimestamp }} finalizers={{ .metadata.finalizers }}"
	dpus := []string{"get", "dpu", "-n", fleetNamespace, "--ignore-not-found",
		"-o", "go-template={{ range .items }}{{ .metadata.name }} phase={{ .status.phase }}" + meta + "\n{{ end }}"}
	for ordinal := range replicas {
		dpus = append(dpus, fleetDPUName(ordinal))
	}
	queries := []struct {
		label string
		args  []string
	}{
		{"SimFleet", []string{"get", "simfleet", fleetName, "-n", fleetNamespace, "--ignore-not-found",
			"-o", "go-template={{ .metadata.name }}" + meta + "\n"}},
		{"SimInstances", []string{"get", "siminstances", "-n", fleetNamespace, "-l", fleetLabel,
			"-o", "go-template={{ range .items }}{{ .metadata.name }} phase={{ .status.phase }} teardown=" +
				`{{ range .status.conditions }}{{ if eq .type "Teardown" }}{{ .reason }}{{ end }}{{ end }}` +
				meta + "\n{{ end }}"}},
		{"DPUNodes and DPUDevices", []string{"get", "dpunodes,dpudevices", "-n", fleetNamespace, "-l", fleetLabel,
			"-o", "go-template={{ range .items }}{{ .kind }}/{{ .metadata.name }}" +
				` pool={{ index .metadata.labels "scale.dpu.nvidia.com/pool" }}` + meta + "\n{{ end }}"}},
		{"DPUs", dpus},
		{"VMs and VMIs", []string{"get", "virtualmachines,virtualmachineinstances", "-n", fleetNamespace,
			"-l", fleetLabel,
			"-o", "go-template={{ range .items }}{{ .kind }}/{{ .metadata.name }}" + meta + "\n{{ end }}"}},
	}
	var trace strings.Builder
	fmt.Fprintf(&trace, "Fleet deletion at %s (+%s):\n",
		time.Now().UTC().Format(time.RFC3339), time.Since(started).Round(time.Second))
	for _, query := range queries {
		output, err := exec.Command("kubectl", query.args...).CombinedOutput()
		if err != nil {
			fmt.Fprintf(&trace, "  %s: %v: %s\n", query.label, err, strings.TrimSpace(string(output)))
			continue
		}
		lines := nonEmptyLines(string(output))
		if len(lines) == 0 {
			lines = []string{"none"}
		}
		fmt.Fprintf(&trace, "  %s:\n    %s\n", query.label, strings.Join(lines, "\n    "))
	}
	_, _ = fmt.Fprint(GinkgoWriter, trace.String())
}

// dumpFleetDeletionDiagnostics writes the controller log lines and Events
// about the fleet's teardown since started, whether or not the spec passed.
func dumpFleetDeletionDiagnostics(started time.Time) {
	traceFleetDeletion(started)
	if podName := controllerPodName(); podName != "" {
		since := started.Add(-time.Minute).UTC().Format(time.RFC3339)
		output, err := exec.Command("kubectl", "logs", podName, "-n", namespace, "--since-time="+since).CombinedOutput()
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get controller logs: %v: %s\n", err, output)
		} else {
			var kept []string
			for _, line := range nonEmptyLines(string(output)) {
				lower := strings.ToLower(line)
				for _, word := range []string{"siminstance", "simfleet", "teardown", "deleted", "stripped"} {
					if strings.Contains(lower, word) {
						kept = append(kept, line)
						break
					}
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Controller teardown log lines since %s:\n%s\n", since, strings.Join(kept, "\n"))
		}
	}
	output, err := exec.Command("kubectl", "get", "events", "-n", fleetNamespace,
		"--sort-by=.lastTimestamp").CombinedOutput()
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Events: %v: %s\n", err, output)
		return
	}
	lines := nonEmptyLines(string(output))
	kept := lines[:min(1, len(lines))]
	for _, line := range lines[len(kept):] {
		if strings.Contains(line, fleetName) {
			kept = append(kept, line)
		}
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Events about %s:\n%s\n", fleetName, strings.Join(kept, "\n"))
}

// expectOrdinalGone checks every object one ordinal's teardown removes,
// including DPF's DPU and join Secret, which carry no fleet label.
func expectOrdinalGone(g Gomega, ordinal int) {
	dpuName := fleetDPUName(ordinal)
	for _, child := range []struct{ resource, name string }{
		{"siminstance", naming.InstanceName(fleetName, ordinal)},
		{"dpu", dpuName},
		{"secret", naming.KubeadmJoinSecretName(dpuName)},
		{"dpudevice", naming.DeviceName(fleetName, ordinal)},
		{"virtualmachine", naming.VirtualMachineName(fleetName, ordinal)},
		{"virtualmachineinstance", naming.VirtualMachineName(fleetName, ordinal)},
		{"secret", naming.CloudInitSecretName(fleetName, ordinal)},
		{"serviceaccount", dpuName},
		{"role", dpuName},
		{"rolebinding", dpuName},
	} {
		expectGone(g, child.resource, child.name)
	}
}

// expectGone tells a deleted object from a failed read: kubectl prints
// nothing for a missing object only with --ignore-not-found.
func expectGone(g Gomega, resource, name string) {
	cmd := exec.Command("kubectl", "get", resource, name, "-n", fleetNamespace,
		"--ignore-not-found", "-o", "name")
	output, err := run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "%s %s still exists", resource, name)
}

// virtualMachineReadyTimeout allows more time without KVM, where the guest
// boots under emulation.
func virtualMachineReadyTimeout() time.Duration {
	if softwareEmulationEnabled() {
		return 20 * time.Minute
	}
	return 5 * time.Minute
}

// guestAgentTimeout allows for the full guest boot, which under emulation is
// considerably slower than reaching VirtualMachine Ready.
func guestAgentTimeout() time.Duration {
	if softwareEmulationEnabled() {
		return 20 * time.Minute
	}
	return 8 * time.Minute
}

// dpuProvisioningTimeout allows for everything that stands between a booted
// guest and a joined tenant Node: DPF claiming the device, the Redfish flow
// against the mock BMC on the guest, the BFB install, and then the agent's own
// operations. The fleet takes the short path (the BMC already reports the
// minimum firmware, no Secure Boot, no factory reset, rebootMethod NoAction),
// so this is the guest budget plus a handful of Redfish round trips rather
// than a second boot.
func dpuProvisioningTimeout() time.Duration {
	return guestAgentTimeout() + 7*time.Minute
}

func delegatedIPv4(vmiName string) (string, string, error) {
	guestIP, err := objectJSONPathErr(
		"virtualmachineinstance", vmiName, fleetNamespace,
		`{.status.interfaces[?(@.name=="default")].ipAddress}`,
	)
	if err != nil {
		return "", "", err
	}

	// KubeVirt 1.9 labels virt-launcher with vm.kubevirt.io/name. kubevirt.io/domain is a pod annotation.
	cmd := exec.Command(
		"kubectl", "get", "pod", "-n", fleetNamespace,
		"-l", "vm.kubevirt.io/name="+vmiName,
		"-o", "jsonpath={.items[0].status.podIP}",
	)
	output, err := run(cmd)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(guestIP), strings.TrimSpace(output), nil
}

func secretDataKeyExists(name, key string) bool {
	path := "{.data." + strings.ReplaceAll(key, ".", `\.`) + "}"
	output, err := objectJSONPathErr("secret", name, fleetNamespace, path)
	return err == nil && output != ""
}

func listedNames(resource, ns, label string) ([]string, error) {
	args := []string{"get", resource, "-n", ns,
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}"}
	if label != "" {
		args = append(args, "-l", label)
	}
	cmd := exec.Command("kubectl", args...)
	output, err := run(cmd)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(output), nil
}

func objectJSONPath(resource, name, ns, path string) string {
	output, err := objectJSONPathErr(resource, name, ns, path)
	Expect(err).NotTo(HaveOccurred(), "Failed to get %s/%s", resource, name)
	return output
}

func objectJSONPathErr(resource, name, ns, path string) (string, error) {
	cmd := exec.Command("kubectl", "get", resource, name, "-n", ns, "-o", "jsonpath="+path)
	return run(cmd)
}

// objectLabel reads one label, and reads an unset one as the empty string.
// It goes through jsonpath rather than a go-template because a go-template
// renders a missing map key as the literal "<no value>", which an assertion
// that a label is absent can never satisfy.
func objectLabel(resource, name, ns, key string) string {
	return objectJSONPath(resource, name, ns,
		fmt.Sprintf("{.metadata.labels['%s']}", strings.ReplaceAll(key, ".", `\.`)))
}

func listedJSONPath(resource, name, ns, path string) []string {
	return nonEmptyLines(strings.ReplaceAll(objectJSONPath(resource, name, ns, path), " ", "\n"))
}

func cleanupFleetInventory(ns, fleetName string) {
	deleteFleetGracefully(ns, fleetName)

	By("force-deleting whatever SimFleet inventory is left")
	// The fleet shares dpf-operator-system with the operator. Never delete
	// the namespace, and never list-and-wipe Secrets or other types: that
	// would take down DPF's CA, Issuer, and controller credentials.
	for _, item := range []struct{ resource, name string }{
		{"simfleet", fleetName},
		{"dpuset", "e2e-dpuset"},
		{"dpuflavor", "e2e-flavor"},
		{"bfb", "e2e-bfb"},
		{"dpucluster", "e2e-tenant"},
		{"tenantcontrolplane", "e2e-tenant"},
		{"secret", "bmc-shared-password"},
	} {
		stripFinalizersAndDelete(item.resource, item.name, ns)
	}

	// SimInstances go first: their teardown finalizer would otherwise hold
	// them, and their children, past the cleanup.
	for _, resource := range []string{
		"siminstances",
		"virtualmachines",
		"dpu",
		"dpudevices",
		"dpunodes",
		"secrets",
	} {
		cmd := exec.Command("kubectl", "get", resource, "-n", ns, "-l", fleetLabel,
			"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
		output, err := run(cmd)
		if err != nil {
			continue
		}
		for _, name := range nonEmptyLines(output) {
			stripFinalizersAndDelete(resource, name, ns)
		}
	}
}

// deleteFleetGracefully gives garbage collection and the SimInstance
// finalizers one teardown budget before the forced cleanup takes over. It
// never fails the suite: after an earlier failure the controller may be the
// thing that is broken, and the cleanup still has to run.
func deleteFleetGracefully(ns, fleetName string) {
	By("deleting the SimFleet through the normal deletion path")
	cmd := exec.Command("kubectl", "delete", "simfleet", fleetName, "-n", ns, "--ignore-not-found", "--wait=false")
	if _, err := run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Could not delete SimFleet %s: %s\n", fleetName, err)
		return
	}
	deadline := time.Now().Add(instanceTeardownTimeout)
	for {
		names, err := listedNames("siminstances", ns, fleetLabel)
		if err == nil && len(names) == 0 {
			return
		}
		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(GinkgoWriter, "SimInstances %v still exist after %s; forcing cleanup\n",
				names, instanceTeardownTimeout)
			return
		}
		time.Sleep(5 * time.Second)
	}
}

func stripFinalizersAndDelete(resource, name, ns string) {
	cmd := exec.Command("kubectl", "patch", resource, name, "-n", ns,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
	_, _ = run(cmd)
	cmd = exec.Command("kubectl", "delete", resource, name, "-n", ns,
		"--ignore-not-found", "--force", "--grace-period=0")
	_, _ = run(cmd)
}

// renderDPUCluster fills in the keepalived endpoint the DPUCluster needs. Its
// VIP is advertised over VRRP on the Kind bridge, which every Kind cluster on
// the host shares, so the address and the router id have to be unique per
// cluster. Set E2E_SLOT (0-50) to a distinct value for each e2e running at once
// on the same host.
func renderDPUCluster() (string, error) {
	slot := 0
	if raw := os.Getenv("E2E_SLOT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return "", fmt.Errorf("E2E_SLOT %q is not a number: %w", raw, err)
		}
		slot = parsed
	}
	// Router ids stop at 255. Starting at 200 also stays clear of the 192.0.2.199
	// VIP and router id 126 that the DPF suite's own Kind clusters take.
	if slot < 0 || slot > 50 {
		return "", fmt.Errorf("E2E_SLOT %d has no VIP reserved for it", slot)
	}
	template, err := os.ReadFile(filepath.Join("testdata", "dpucluster.yaml.tmpl"))
	if err != nil {
		return "", fmt.Errorf("failed to read the DPUCluster template: %w", err)
	}
	return strings.NewReplacer(
		"${KEEPALIVED_VIP}", fmt.Sprintf("192.0.2.%d", 200+slot),
		"${KEEPALIVED_VIRTUAL_ROUTER_ID}", strconv.Itoa(200+slot),
	).Replace(string(template)), nil
}

func run(cmd *exec.Cmd) (string, error) {
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}
	return string(output), nil
}

func nonEmptyLines(output string) []string {
	var res []string
	for line := range strings.SplitSeq(output, "\n") {
		if line != "" {
			res = append(res, line)
		}
	}
	return res
}
