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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// kubeletConfiguredCondition is the DPU agent status condition that means
// kubeadm join and kubelet start succeeded. The current DPF pin does not
// export a Go constant for it.
const kubeletConfiguredCondition = "KubeletConfigured"

func getDPU(name string) (*provisioningv1.DPU, error) {
	cmd := exec.Command("kubectl", "get", "dpu", name, "-n", fleetNamespace, "-o", "json")
	output, err := run(cmd)
	if err != nil {
		return nil, err
	}
	dpu := &provisioningv1.DPU{}
	if err := json.Unmarshal([]byte(output), dpu); err != nil {
		return nil, fmt.Errorf("decode DPU %q: %w", name, err)
	}
	return dpu, nil
}

func tenantClient() (client.Client, error) {
	kubeconfig, err := tenantKubeconfig()
	if err != nil {
		return nil, err
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse tenant kubeconfig: %w", err)
	}
	return client.New(restConfig, client.Options{Scheme: scheme.Scheme})
}

func tenantKubeconfig() ([]byte, error) {
	secretName, err := objectJSONPathErr(
		"dpucluster", "e2e-tenant", fleetNamespace, "{.spec.kubeconfig}",
	)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(
		"kubectl", "get", "secret", secretName, "-n", fleetNamespace,
		"-o", `jsonpath={.data.super-admin\.conf}`,
	)
	encoded, err := run(cmd)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode tenant kubeconfig: %w", err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("load tenant kubeconfig: %w", err)
	}
	if err := rewriteTenantKubeconfigClusters(cfg); err != nil {
		return nil, err
	}
	return clientcmd.Write(*cfg)
}

// rewriteTenantKubeconfigClusters points every cluster server at the Kind
// node's InternalIP. Kamaji publishes a keepalived VIP on that node's eth0;
// the suite runs on the host, which can reach the node but not the VIP. TLS
// still verifies the VIP name from the original kubeconfig.
func rewriteTenantKubeconfigClusters(cfg *clientcmdapi.Config) error {
	nodeIP, err := kindNodeInternalIP()
	if err != nil {
		return err
	}
	for _, cluster := range cfg.Clusters {
		if cluster == nil {
			continue
		}
		server, err := url.Parse(cluster.Server)
		if err != nil {
			return fmt.Errorf("parse tenant server %q: %w", cluster.Server, err)
		}
		if cluster.TLSServerName == "" {
			cluster.TLSServerName = server.Hostname()
		}
		if server.Hostname() == nodeIP {
			continue
		}
		port := server.Port()
		if port == "" {
			port = "443"
		}
		server.Host = net.JoinHostPort(nodeIP, port)
		cluster.Server = server.String()
	}
	return nil
}

func withTenantKubeconfig(fn func(path string) error) error {
	kubeconfig, err := tenantKubeconfig()
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "simfleet-e2e-tenant-kubeconfig-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(kubeconfig); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return fn(path)
}

func kindNodeInternalIP() (string, error) {
	cmd := exec.Command(
		"kubectl", "get", "nodes",
		"-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`,
	)
	output, err := run(cmd)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(output)
	if ip == "" {
		return "", fmt.Errorf("Kind node has no InternalIP")
	}
	return ip, nil
}

func dumpDPUAgentDiagnostics() {
	var names []string
	for ordinal := range replicas {
		names = append(names, fleetDPUName(ordinal))
	}
	for _, dpuName := range names {
		By("Fetching DPU " + dpuName + " status")
		cmd := exec.Command("kubectl", "get", "dpu", dpuName, "-n", fleetNamespace, "-o", "yaml")
		output, err := run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "DPU %s:\n%s", dpuName, redactGuestCredentials(output))
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get DPU %s: %s\n", dpuName, err)
		}
	}

	dumpDPUDeviceDiagnostics()
	dumpProvisioningControllerDiagnostics()
	dumpTenantNodeDiagnostics()
	dumpGuestVMDiagnostics()
}

// dumpDPUDeviceDiagnostics explains a DPU that never left Initializing. On the
// Redfish path the DPU waits for its DPUDevice to report a type, and everything
// that can stall that -- BMC credentials, the factory reset, the mTLS exchange,
// the chassis read -- is recorded in the DPUDevice conditions and nowhere else.
// Without them a mock BMC that is never contacted looks like one that answered
// wrongly.
func dumpDPUDeviceDiagnostics() {
	By("Fetching DPUDevices")
	cmd := exec.Command("kubectl", "get", "dpudevices", "-n", fleetNamespace, "-o", "yaml")
	output, err := run(cmd)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "DPUDevices:\n%s", redactGuestCredentials(output))
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get DPUDevices: %s\n", err)
	}
	probeBMCReachability()
}

// probeBMCReachability answers the question a DPUDevice stuck on an
// unanswered BMC leaves open: whether the guest is unreachable or only
// unreachable from where DPF runs. It dials each BMC twice, once from the pod
// network and once from the node's, because the two fail apart. A guest that
// answers the node but not a pod is a pod-network return path that never
// reaches it -- the guest takes its address from KubeVirt's DHCP with the pod
// subnet's own prefix, so it ARPs pod addresses instead of routing to them.
func probeBMCReachability() {
	By("Probing the guest BMC addresses")
	names, err := listedNames("dpudevices", fleetNamespace, fleetLabel)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to list DPUDevices to probe: %s\n", err)
		return
	}
	for i, device := range names {
		bmcIP, err := objectJSONPathErr("dpudevice", device, fleetNamespace, "{.spec.bmcIp}")
		if err != nil || bmcIP == "" {
			_, _ = fmt.Fprintf(GinkgoWriter, "%s publishes no BMC address to probe\n", device)
			continue
		}
		probeBMCFrom(fmt.Sprintf("bmc-probe-pod-%d", i), "pod network", bmcIP, device, nil)
		probeBMCFrom(fmt.Sprintf("bmc-probe-node-%d", i), "node network", bmcIP, device,
			[]string{"--overrides", `{"spec":{"hostNetwork":true}}`})
	}
}

func probeBMCFrom(podName, from, bmcIP, device string, extra []string) {
	args := []string{
		"run", podName, "-n", fleetNamespace,
		"--rm", "--attach", "--restart=Never",
		"--image", bfbFixturesImage, "--image-pull-policy", "IfNotPresent",
	}
	args = append(args, extra...)
	args = append(args, "--command", "--", "sh", "-c",
		fmt.Sprintf("nc -z -w 5 %s 443 && echo REACHABLE || echo UNREACHABLE", bmcIP))

	output, err := run(exec.Command("kubectl", args...))
	_, _ = fmt.Fprintf(GinkgoWriter, "%s BMC %s from the %s: %s\n",
		device, bmcIP, from, strings.TrimSpace(output))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "probe from the %s did not run cleanly: %s\n", from, err)
	}
}

// dumpProvisioningControllerDiagnostics prints the logs of the DPF controllers.
// They are the side that drives Redfish against the guest, so when a DPUDevice
// stalls the reason is in their logs; this suite otherwise only collects the
// SimFleet controller, which has already done its part by then.
func dumpProvisioningControllerDiagnostics() {
	By("Fetching DPF controller logs")
	cmd := exec.Command("kubectl", "get", "pods", "-n", dpfOperatorNamespace, "-o", "wide")
	output, err := run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to list %s pods: %s\n", dpfOperatorNamespace, err)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "%s pods:\n%s", dpfOperatorNamespace, output)

	cmd = exec.Command("kubectl", "get", "pods", "-n", dpfOperatorNamespace,
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")
	output, err = run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to name %s pods: %s\n", dpfOperatorNamespace, err)
		return
	}
	for _, pod := range nonEmptyLines(output) {
		if !strings.Contains(pod, "provisioning") && !strings.Contains(pod, "operator-controller-manager") {
			continue
		}
		logs, err := run(exec.Command("kubectl", "logs", pod,
			"-n", dpfOperatorNamespace, "--all-containers", "--tail=-1"))
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "%s logs:\n%s", pod, redactGuestCredentials(logs))
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get %s logs: %s\n", pod, err)
		}
	}
}

// dumpBFBFixtureDiagnostics explains a DPU that never left OS Installing. The
// BFB it installs is served by a Pod this suite's harness deploys, so a
// missing ConfigMap key or an unscheduled fixture Pod looks exactly like a
// mock BMC that refuses the update until these are on the failure output.
func dumpBFBFixtureDiagnostics() {
	By("Fetching the BFB")
	cmd := exec.Command("kubectl", "get", "bfb", "-n", fleetNamespace, "-o", "yaml")
	output, err := run(cmd)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "BFBs:\n%s", output)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get BFBs: %s\n", err)
	}

	const selector = "app.kubernetes.io/name=bfb-fixtures"
	By("Fetching the BFB fixture server")
	for _, args := range [][]string{
		{"describe", "deployment", "bfb-fixtures", "-n", fleetNamespace},
		{"get", "pods", "-n", fleetNamespace, "-l", selector, "-o", "wide"},
		{"describe", "pods", "-n", fleetNamespace, "-l", selector},
		// The access log is the only place that says whether DPF ever asked
		// for the file, which separates a broken server from a broken URL.
		{"logs", "-n", fleetNamespace, "-l", selector, "--tail=-1"},
	} {
		output, err := run(exec.Command("kubectl", args...))
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s", strings.Join(args, " "), output)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to run kubectl %s: %s\n", strings.Join(args, " "), err)
		}
	}
}

func dumpTenantNodeDiagnostics() {
	By("Fetching tenant Nodes")
	err := withTenantKubeconfig(func(path string) error {
		cmd := exec.Command("kubectl", "--kubeconfig", path, "get", "nodes", "-o", "wide")
		output, err := run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "tenant Nodes:\n%s", output)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to list tenant Nodes: %s\n", err)
		}
		cmd = exec.Command("kubectl", "--kubeconfig", path, "describe", "nodes")
		output, err = run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "tenant Node descriptions:\n%s", redactGuestCredentials(output))
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe tenant Nodes: %s\n", err)
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to open tenant kubeconfig: %s\n", err)
	}
}

// dumpGuestVMDiagnostics prints what can be read about a guest from outside it.
// There is no way in: KubeVirt has no guest-exec verb, and virtctl ssh needs a
// key the fleet does not install. What is left is the serial console, which the
// guest-console-log container records in full and which dpuagent, kubelet and
// containerd all write to. It is dumped whole rather than tailed because the
// systemd messages that explain a unit which never started are at the top.
func dumpGuestVMDiagnostics() {
	By("collecting the guest serial console and virt-launcher logs")
	for ordinal := range replicas {
		vmiName := naming.VirtualMachineName(fleetName, ordinal)
		dumpVMIContainerLog(vmiName, "guest-console-log", "serial console")
		dumpVMIContainerLog(vmiName, "compute", "virt-launcher")
		By("Fetching VirtualMachineInstance description")
		cmd := exec.Command("kubectl", "describe", "virtualmachineinstance", vmiName, "-n", fleetNamespace)
		output, err := run(cmd)
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "VirtualMachineInstance %s:\n%s", vmiName, redactGuestCredentials(output))
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe VirtualMachineInstance %s: %s\n", vmiName, err)
		}
	}
}

func dumpVMIContainerLog(vmiName, container, description string) {
	By("Fetching the " + description + " for " + vmiName)
	// A label selector makes kubectl default to the last ten lines, so the
	// whole log has to be asked for explicitly.
	cmd := exec.Command(
		"kubectl", "logs", "-n", fleetNamespace,
		"-l", "vm.kubevirt.io/name="+vmiName,
		"-c", container, "--tail=-1",
	)
	output, err := run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get the %s for %s: %s\n", description, vmiName, err)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "%s %s:\n%s", vmiName, description, redactGuestCredentials(output))
}

// expectPhaseDelaysApplied checks the guest's serial console, where
// mock-dpuagent logs, for each phase the fleet delays: the startup line that
// shows the rendered config reached the mock, and a step line that shows a
// delay was drawn. klog quotes the phase value, but it is matched either way so
// a change in how a named string type is printed cannot fail the check.
func expectPhaseDelaysApplied(g Gomega, vmiName string, phases []string) {
	console, err := run(exec.Command(
		"kubectl", "logs", "-n", fleetNamespace,
		"-l", "vm.kubevirt.io/name="+vmiName,
		"-c", "guest-console-log", "--tail=-1",
	))
	g.Expect(err).NotTo(HaveOccurred(), "read the serial console of %s", vmiName)
	var matched []string
	for _, phase := range phases {
		for _, msg := range []string{"DPU phase delayed", "DPU phase delay"} {
			line := regexp.MustCompile(`[^\n]*` + regexp.QuoteMeta(`"`+msg+`"`) + ` phase="?` +
				regexp.QuoteMeta(phase) + `"?(\s[^\n]*)?(\n|$)`)
			lines := line.FindAllString(console, -1)
			g.Expect(lines).NotTo(BeEmpty(), "%s serial console has no %q line for phase %q", vmiName, msg, phase)
			for _, l := range lines {
				matched = append(matched, strings.TrimSpace(l))
			}
		}
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "%s applied phase delays %q:\n  %s\n", vmiName, phases, strings.Join(matched, "\n  "))
}

// redactGuestCredentials drops the lines that carry credential material and
// keeps the rest. The agent logs the path of its kubeconfig, so redacting a
// whole dump on sight of that path would hide the failure it was collected for.
func redactGuestCredentials(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		for _, marker := range []string{
			"client-key-data",
			"client-certificate-data",
			"certificate-authority-data",
			"token:",
			"--token",
		} {
			if strings.Contains(line, marker) {
				lines[i] = "[redacted: guest credential material]"
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func expectDPUAgentJoined(g Gomega, tenant client.Client, dpuName string) (*provisioningv1.DPU, *corev1.Node) {
	dpu, err := getDPU(dpuName)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get DPU %s", dpuName)
	g.Expect(dpu.Status.AgentStatus).NotTo(BeNil(), "%s has no agentStatus", dpuName)
	g.Expect(meta.IsStatusConditionTrue(
		dpu.Status.AgentStatus.Conditions,
		kubeletConfiguredCondition,
	)).To(BeTrue(), "%s KubeletConfigured is not True", dpuName)
	g.Expect(dpu.Status.AgentStatus.RebootMethod).NotTo(BeNil(), "%s rebootMethod is unset", dpuName)
	g.Expect(*dpu.Status.AgentStatus.RebootMethod).To(Equal(provisioningv1.RebootMethodNoAction))

	node := &corev1.Node{}
	g.Expect(tenant.Get(context.Background(), client.ObjectKey{Name: dpu.Name}, node)).To(Succeed(),
		"tenant Node %s is missing", dpu.Name)
	return dpu, node
}

// expectNodeJoinedInConfigureKubelet fails when the tenant Node was created
// before the agent finished Configure Kubelet, which is when a real DPU joins.
// A guest that joined earlier makes the controller see DPU Cluster Config as
// already done, so the phase delays up to it would gate nothing.
func expectNodeJoinedInConfigureKubelet(g Gomega, dpu *provisioningv1.DPU, node *corev1.Node) {
	configured := meta.FindStatusCondition(dpu.Status.AgentStatus.Conditions, kubeletConfiguredCondition)
	g.Expect(configured).NotTo(BeNil(), "%s has no %s condition", dpu.Name, kubeletConfiguredCondition)
	g.Expect(node.CreationTimestamp.Before(&configured.LastTransitionTime)).To(BeFalse(),
		"tenant Node %s was created at %s, before %s became True at %s",
		node.Name, node.CreationTimestamp, kubeletConfiguredCondition, configured.LastTransitionTime)
}

// expectDPUAgentRejoined is expectDPUAgentJoined for a DPU whose guest was
// replaced. The replacement keeps the DPU and Node names, and the old tenant
// Node can outlive the old guest, so a joined check alone can pass on it.
// The agent status has to be on a DPU other than the stale one, and the Node
// has to be reporting from a boot other than the stale one.
func expectDPUAgentRejoined(g Gomega, tenant client.Client, dpuName string, staleDPUUID, staleBootID string) {
	dpu, node := expectDPUAgentJoined(g, tenant, dpuName)
	g.Expect(string(dpu.UID)).NotTo(Equal(staleDPUUID), "%s is still the DPU from before the replacement", dpuName)
	g.Expect(node.Status.NodeInfo.BootID).NotTo(BeEmpty(), "tenant Node %s reports no boot ID", dpuName)
	g.Expect(node.Status.NodeInfo.BootID).NotTo(Equal(staleBootID),
		"tenant Node %s still reports the boot from before the replacement", dpuName)
}

// tenantNodeBootID reads the boot a tenant Node's kubelet last reported.
func tenantNodeBootID(tenant client.Client, name string) (string, error) {
	node := &corev1.Node{}
	if err := tenant.Get(context.Background(), client.ObjectKey{Name: name}, node); err != nil {
		return "", err
	}
	return node.Status.NodeInfo.BootID, nil
}
