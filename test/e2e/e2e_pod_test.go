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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

const (
	// podRuntimeEnv selects the pod-runtime run. `make test-e2e-pod` sets it
	// to podRuntimeValue; the VM suite never does, so the specs below skip
	// there. They also need the Kind cluster that target prepares: no
	// KubeVirt, config/node-prep/ applied, and the node labelled.
	podRuntimeEnv   = "E2E_RUNTIME"
	podRuntimeValue = "pod"

	// podReplicas is one simulated DPU: enough to run a DPUService on it.
	podReplicas = 1

	// dpuServiceName and dpuServiceLabelKey match testdata/pod/dpuservice.yaml.
	// The hello-world chart in testdata/pod/dpuservice.yaml does not template
	// serviceDaemonSet.labels onto its Pods, so DPF's service label never
	// reaches them. They carry the chart's own name label instead; selecting
	// by it finds them whatever namespace the chart lands in on the DPU
	// cluster.
	dpuServiceName     = "e2e-hello"
	dpuServiceLabelKey = "app.kubernetes.io/name"
	dpuServiceLabelVal = "hello-world"

	// podProvisioningTimeout covers DPF claiming the device, the Redfish flow
	// against the mock BMC in the Pod, the BFB install, and the Pod's
	// kubelet joining the DPU cluster. There is no guest boot to wait for.
	podProvisioningTimeout = 20 * time.Minute

	// dpuServiceTimeout covers ArgoCD fetching the chart and the DPU's inner
	// containerd pulling the image over the host cluster's network.
	dpuServiceTimeout = 10 * time.Minute
)

func podRuntimeEnabled() bool {
	return os.Getenv(podRuntimeEnv) == podRuntimeValue
}

var _ = Describe("SimFleet pod runtime", Ordered, Label("pod-runtime"), func() {
	BeforeAll(func() {
		if !podRuntimeEnabled() {
			Skip(fmt.Sprintf("%s=%s is not set; run `make test-e2e-pod`", podRuntimeEnv, podRuntimeValue))
		}
	})

	AfterAll(func() {
		if !podRuntimeEnabled() {
			return
		}
		stripFinalizersAndDelete("dpuservice", dpuServiceName, fleetNamespace)
		cleanupFleetInventory(fleetNamespace, fleetName)
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		dumpPodRuntimeDiagnostics()
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("provisions a simulated DPU Pod and runs a DPUService on it", func() {
		By("checking that KubeVirt is not installed")
		_, err := run(exec.Command("kubectl", "get", "crd", "virtualmachines.kubevirt.io"))
		Expect(err).To(HaveOccurred(), "the pod-runtime run must not install KubeVirt")

		By("applying the SimFleet with a podTemplate")
		// DPF's CRDs and admission webhooks can lag the DPFOperatorConfig
		// going Ready, so retry until every object applies. The shared
		// objects come from testdata; the fleet is the pod variant.
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "apply",
				"-f", "testdata/bfb.yaml",
				"-f", "testdata/bmc-shared-password.yaml",
				"-f", "testdata/dpuflavor.yaml",
				"-f", "testdata/dpuset.yaml",
				"-f", "testdata/pod/simfleet.yaml")
			_, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to apply the pod-runtime e2e testdata")
		}).Should(Succeed())

		By("applying the DPUCluster")
		dpuCluster, err := renderDPUCluster()
		Expect(err).NotTo(HaveOccurred(), "Failed to render the DPUCluster")
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(dpuCluster)
		_, err = run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the DPUCluster")

		By("shrinking the tenant control plane to one replica")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "patch", "tenantcontrolplane", "e2e-tenant", "-n", fleetNamespace,
				"--type=merge", "-p", tenantControlPlanePatch)
			_, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to patch the TenantControlPlane")
		}).Should(Succeed())

		By("waiting for the DPU Pod to run")
		podName := naming.GuestName(fleetName, 0)
		Eventually(func(g Gomega) {
			phase, err := objectJSONPathErr("pod", podName, fleetNamespace, "{.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Running"))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		Expect(objectJSONPath("pod", podName, fleetNamespace, "{.metadata.ownerReferences[0].kind}")).To(Equal("SimInstance"))
		Expect(objectJSONPath("pod", podName, fleetNamespace,
			`{.spec.nodeSelector['scale\.dpu\.nvidia\.com/sim-node']}`)).To(Equal("true"))

		By("waiting for the BFB to download from the in-cluster fixture server")
		Eventually(func(g Gomega) {
			phase, err := objectJSONPathErr("bfb", "e2e-bfb", fleetNamespace, "{.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Ready"))
		}, bfbReadyTimeout, 5*time.Second).Should(Succeed())

		By("waiting for the Pod's address to make the device selectable")
		device := naming.DeviceName(fleetName, 0)
		Eventually(func(g Gomega) {
			podIP, err := objectJSONPathErr("pod", podName, fleetNamespace, "{.status.podIP}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podIP).NotTo(BeEmpty())
			bmcIP, err := objectJSONPathErr("dpudevice", device, fleetNamespace, "{.spec.bmcIp}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(bmcIP).To(Equal(podIP))
			pool, err := objectJSONPathErr("dpudevice", device, fleetNamespace,
				`{.metadata.labels['scale\.dpu\.nvidia\.com/pool']}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pool).To(Equal("e2e"))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the Kamaji DPUCluster")
		Eventually(func(g Gomega) {
			phase, err := objectJSONPathErr("dpucluster", "e2e-tenant", fleetNamespace, "{.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Ready"))
			kubeconfigSecret, err := objectJSONPathErr("dpucluster", "e2e-tenant", fleetNamespace, "{.spec.kubeconfig}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(secretDataKeyExists(kubeconfigSecret, "super-admin.conf")).To(BeTrue())
		}, 10*time.Minute, time.Second).Should(Succeed())

		By("waiting for the DPU agent in the Pod to join the tenant cluster")
		tenant, err := tenantClient()
		Expect(err).NotTo(HaveOccurred(), "Failed to build the tenant client")
		var nodeName string
		Eventually(func(g Gomega) {
			dpu, node := expectDPUAgentJoined(g, tenant, fleetDPUName(0))
			expectNodeJoinedInConfigureKubelet(g, dpu, node)
			nodeName = node.Name
		}, podProvisioningTimeout, 5*time.Second).Should(Succeed())

		By("waiting for the SimInstance to report DPUProvisioned")
		Eventually(func(g Gomega) {
			expectPodInstanceProvisioned(g, 0)
			expectFleetCounts(g, podReplicas)
		}).Should(Succeed())

		By("stopping kube-proxy from writing nf_conntrack_max")
		// The sysctl is read-only in the DPU Pod network namespace, and
		// kube-proxy exits when the write fails. Zero leaves the kernel value.
		Eventually(func(g Gomega) {
			g.Expect(withTenantKubeconfig(relaxTenantKubeProxyConntrack)).To(Succeed())
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(withTenantKubeconfig(expectTenantKubeProxyReady)).To(Succeed())
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("deploying a DPUService")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "apply", "-f", "testdata/pod/dpuservice.yaml")
			_, err := run(cmd)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to apply the DPUService")
		}).Should(Succeed())

		By("waiting for a DPUService Pod to run on the simulated DPU")
		Eventually(func(g Gomega) {
			expectDPUServicePodRunning(g, tenant, nodeName)
		}, dpuServiceTimeout, 5*time.Second).Should(Succeed())
	})
})

// expectPodInstanceProvisioned is the pod-runtime counterpart of
// expectInstanceReady. It reads the phase and DPUProvisioned only: the UIDs
// expectInstanceReady also demands are the VirtualMachine ones.
func expectPodInstanceProvisioned(g Gomega, ordinal int) {
	name := naming.InstanceName(fleetName, ordinal)
	phase, err := objectJSONPathErr("siminstance", name, fleetNamespace, "{.status.phase}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(phase).To(Equal("Ready"), "SimInstance %s is not Ready", name)
	provisioned, err := objectJSONPathErr("siminstance", name, fleetNamespace,
		`{.status.conditions[?(@.type=="DPUProvisioned")].status}`)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(provisioned).To(Equal("True"), "SimInstance %s DPUProvisioned is not True", name)
	podUID, err := objectJSONPathErr("siminstance", name, fleetNamespace, "{.status.observedUIDs.pod}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(podUID).NotTo(BeEmpty(), "SimInstance %s has not recorded its Pod", name)
}

// expectDPUServicePodRunning looks for a DPUService Pod in the DPU cluster and
// requires it to be Running on the simulated DPU's Node. It selects the
// hello-world chart's Pods by their app.kubernetes.io/name label.
func expectDPUServicePodRunning(g Gomega, tenant client.Client, nodeName string) {
	pods := &corev1.PodList{}
	g.Expect(tenant.List(context.Background(), pods, client.MatchingLabels{dpuServiceLabelKey: dpuServiceLabelVal})).To(Succeed())
	g.Expect(pods.Items).NotTo(BeEmpty(), "the DPU cluster has no Pod labelled %s=%s", dpuServiceLabelKey, dpuServiceLabelVal)
	running := 0
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == nodeName && pod.Status.Phase == corev1.PodRunning {
			running++
		}
	}
	g.Expect(running).To(BeNumerically(">=", 1),
		"no DPUService Pod is Running on tenant Node %s; Pods: %s", nodeName, describePods(pods.Items))
}

func describePods(pods []corev1.Pod) string {
	var parts []string
	for _, pod := range pods {
		parts = append(parts, fmt.Sprintf("%s/%s on %q is %s", pod.Namespace, pod.Name, pod.Spec.NodeName, pod.Status.Phase))
	}
	return strings.Join(parts, ", ")
}

// dumpPodRuntimeDiagnostics collects what explains a failed pod-runtime run.
// The VM suite's guest diagnostics read VirtualMachineInstances, which this
// run does not have, so the Pod's own description and logs stand in for them.
func dumpPodRuntimeDiagnostics() {
	if podName := controllerPodName(); podName != "" {
		By("Fetching controller manager pod logs")
		logs, err := run(exec.Command("kubectl", "logs", podName, "-n", namespace))
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", logs)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
		}
	}

	for _, args := range [][]string{
		{"get", "events", "-n", fleetNamespace, "--sort-by=.lastTimestamp"},
		{"get", "pods", "-n", fleetNamespace, "-l", fleetLabel, "-o", "wide"},
		{"describe", "pods", "-n", fleetNamespace, "-l", fleetLabel},
		// The DPU Pod's stdout stays empty: systemd is PID 1, so the inner
		// kubelet and the join script report to the journal. That journal is
		// read below with kubectl exec.
		{"logs", "-n", fleetNamespace, "-l", fleetLabel, "--tail=-1"},
		{"get", "siminstances", "-n", fleetNamespace, "-l", fleetLabel, "-o", "yaml"},
		{"get", "dpu", fleetDPUName(0), "-n", fleetNamespace, "-o", "yaml"},
		{"get", "dpuservice", dpuServiceName, "-n", fleetNamespace, "-o", "yaml"},
		{"get", "application", "-n", fleetNamespace, "-o", "wide"},
		{"describe", "dpucluster", "e2e-tenant", "-n", fleetNamespace},
		{"get", "daemonset,pods", "-n", "simfleet-node-prep", "-o", "wide"},
	} {
		By("Running kubectl " + strings.Join(args, " "))
		output, err := run(exec.Command("kubectl", args...))
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s", strings.Join(args, " "), redactGuestCredentials(output))
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to run kubectl %s: %s\n", strings.Join(args, " "), err)
		}
	}

	// journalctl is the guest console. kubectl logs cannot see it.
	dumpGuestJournal("kubelet", "wait-kubelet-join")

	By("Fetching the DPU cluster's Pods")
	err := withTenantKubeconfig(func(path string) error {
		output, err := run(exec.Command("kubectl", "--kubeconfig", path, "get", "pods", "-A", "-o", "wide"))
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "DPU cluster Pods:\n%s", output)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to list DPU cluster Pods: %s\n", err)
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to open tenant kubeconfig: %s\n", err)
	}

	dumpDPUDeviceDiagnostics()
	dumpProvisioningControllerDiagnostics()
	dumpBFBFixtureDiagnostics()
	dumpTenantNodeDiagnostics()
}

// dumpGuestJournal reads the named systemd units from every DPU Pod. The
// join script and the inner kubelet write there, not to the container's
// stdout, so a failed join is invisible in kubectl logs.
func dumpGuestJournal(units ...string) {
	// kubectl exec has no label selector, so resolve the Pod names first.
	names, err := run(exec.Command("kubectl", "get", "pods", "-n", fleetNamespace, "-l", fleetLabel, "-o", "name"))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to list DPU Pods for the guest journal: %s\n", err)
		return
	}
	journal := []string{"journalctl", "--no-pager", "-n", "400"}
	for _, unit := range units {
		journal = append(journal, "-u", unit)
	}
	for _, name := range nonEmptyLines(names) {
		args := append([]string{"exec", "-n", fleetNamespace, name, "--"}, journal...)
		By("Running kubectl " + strings.Join(args, " "))
		output, err := run(exec.Command("kubectl", args...))
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to read the guest journal from %s: %s\n", name, err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Guest journal from %s:\n%s", name, redactGuestCredentials(output))
	}
}

// relaxTenantKubeProxyConntrack sets conntrack maxPerCore and min to 0 and
// restarts kube-proxy so the new pods read it. Kamaji writes null back into
// the ConfigMap; a pod that already started keeps running.
func relaxTenantKubeProxyConntrack(kubeconfigPath string) error {
	get := exec.Command("kubectl", "--kubeconfig", kubeconfigPath,
		"-n", "kube-system", "get", "cm", "kube-proxy", "-o", "json")
	raw, err := run(get)
	if err != nil {
		return err
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &cm); err != nil {
		return fmt.Errorf("decode kube-proxy ConfigMap: %w", err)
	}
	updated, err := disableKubeProxyConntrack(cm.Data["config.conf"])
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{"config.conf": updated},
	})
	if err != nil {
		return err
	}
	apply := exec.Command("kubectl", "--kubeconfig", kubeconfigPath,
		"-n", "kube-system", "patch", "cm", "kube-proxy", "--type=merge", "-p", string(patch))
	if _, err := run(apply); err != nil {
		return err
	}
	restart := exec.Command("kubectl", "--kubeconfig", kubeconfigPath,
		"-n", "kube-system", "delete", "pod", "-l", "k8s-app=kube-proxy", "--wait=false")
	_, err = run(restart)
	return err
}

func expectTenantKubeProxyReady(kubeconfigPath string) error {
	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfigPath,
		"-n", "kube-system", "get", "pods", "-l", "k8s-app=kube-proxy",
		"-o", "jsonpath={.items[*].status.containerStatuses[0].ready}")
	out, err := run(cmd)
	if err != nil {
		return err
	}
	ready := strings.Fields(out)
	if len(ready) == 0 {
		return fmt.Errorf("kube-proxy has no pods")
	}
	for _, item := range ready {
		if item != "true" {
			return fmt.Errorf("kube-proxy is not ready: %s", out)
		}
	}
	return nil
}
