# E2E Suite DPF Alignment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the Kind e2e suite in line with how DPF's own mock-agent e2e (`test/e2e/mock_dpuagent.go` in the pinned doca-platform module) proves a DPU is provisioned, and clean up the suite's own weak spots.

**Architecture:** All changes are in the e2e suite (`test/e2e/`), its DPF install script (`hack/e2e/deploy-dpf.sh`), and the timeout budget (`Makefile`, `.github/workflows/e2e.yml`). No controller code changes. DPU helpers go in `test/e2e/e2e_agent.go` next to the existing DPU and tenant helpers; spec flow changes go in `test/e2e/e2e_test.go`.

**Tech Stack:** Go, Ginkgo v2.32, Gomega v1.42 (`StopTrying` is available), controller-runtime client, kubectl, DPF `api/provisioning/v1alpha1`.

## Global Constraints

- Every Go file in `test/e2e/` carries `//go:build e2e`; vet and build with `-tags e2e`.
- DPF helpers in `github.com/nvidia/doca-platform/internal/...` cannot be imported (Go `internal` rule). Re-declare what is needed locally.
- The fleet shares `dpf-operator-system` with DPF. Never delete the namespace and never list-and-wipe Secrets.
- Comments state constraints the code cannot show; do not narrate the change.
- Conventional commits (`test:`, `fix:`, `chore:`).
- `make test-e2e` only runs on Linux amd64 (`check-e2e-host`). On macOS, verify locally with `go vet -tags e2e ./test/e2e/` and `make lint`; run the suite in CI by adding the `e2e` label to the PR (`.github/workflows/e2e.yml`).

## Reference facts (checked against the pinned DPF module)

- DPU phases: `provisioningv1.DPUReady` = `"Ready"`, `provisioningv1.DPUError` = `"Error"`.
- `state.Error` only moves a DPU out of `Error` when it has a deletion timestamp, so `Error` is terminal for a live DPU.
- Bootstrap token Secrets carry label `provisioning.dpu.nvidia.com/dpu-namespace=<DPU namespace>` (`cutil.LabelDPUNamespace`).
- bfb-registry pods carry label `dpu.nvidia.com/component=bfb-registry`.
- DPF's mock suite, after `Ready`: every agent condition `True`, tenant Node `Ready`, its `kube-node-lease` Lease renewed < 1m ago, `{dpu}-kubeadm-join` Secret deleted, no `bootstrap.kubernetes.io/token` Secret left with the DPU-namespace label in either cluster.
- The guest runs the same mock DPU agent DPF's suite uses (`hack/dpuagent/mock.ref`).

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `test/e2e/e2e_agent.go` | DPU observation, readiness and provisioned checks | 1, 2 |
| `test/e2e/e2e_test.go` | Spec flow, cleanup, naming | 1, 2, 3, 5, 6 |
| `test/e2e/e2e_suite_test.go` | Suite-wide Gomega defaults | 5 |
| `hack/e2e/deploy-dpf.sh` | DPF install readiness | 4 |
| `Makefile`, `.github/workflows/e2e.yml` | Timeout budget | 7 |

---

### Task 1: Log DPU phases, fail fast on Error, and wait for DPU Ready

**Files:**
- Modify: `test/e2e/e2e_agent.go` (imports; `expectDPUAgentJoined`, `expectDPUAgentRejoined`; new helpers)
- Modify: `test/e2e/e2e_test.go` (const block; end of spec "creates DPUDevices and DPUNodes from a SimFleet"; replacement spec after the rejoin wait)

**Interfaces:**
- Produces: `observeDPU(g Gomega, name, staleUID string) *provisioningv1.DPU`, `dpuConditionsSummary(dpu *provisioningv1.DPU) string`, `expectDPUReady(g Gomega, name string)`, `expectDPUAgentJoinedSince(g Gomega, tenant client.Client, dpuName, staleUID string) (*provisioningv1.DPU, *corev1.Node)`, const `dpuReadyTimeout`.

- [ ] **Step 1: Add the imports to `e2e_agent.go`**

Add `"time"` to the standard-library group and `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` to the Kubernetes group.

- [ ] **Step 2: Add the DPU observation helpers to `e2e_agent.go`** (after `getDPU`)

```go
// dpuPhases is the last phase logged for each DPU. Specs run serially, so one
// map shared by every spec is enough.
var dpuPhases = map[string]provisioningv1.DPUPhase{}

// observeDPU reads a DPU inside an Eventually. It logs the phase whenever it
// changes, so a long wait shows where provisioning is, and ends the Eventually
// at once when the DPU is in Error: DPF only leaves Error once the DPU is
// deleted, so polling on would spend the rest of the budget for nothing. A DPU
// with staleUID is the one a replacement is waiting to see gone, and its Error
// is expected.
func observeDPU(g Gomega, name, staleUID string) *provisioningv1.DPU {
	dpu, err := getDPU(name)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get DPU %s", name)
	if dpuPhases[name] != dpu.Status.Phase {
		dpuPhases[name] = dpu.Status.Phase
		_, _ = fmt.Fprintf(GinkgoWriter, "%s DPU %s phase %q\n",
			time.Now().UTC().Format(time.RFC3339), name, dpu.Status.Phase)
	}
	if dpu.Status.Phase == provisioningv1.DPUError && string(dpu.UID) != staleUID {
		StopTrying(fmt.Sprintf("DPU %s entered Error: %s", name, dpuConditionsSummary(dpu))).Now()
	}
	return dpu
}

// dpuConditionsSummary lists the DPU conditions that are not True, which is
// where DPF records why a DPU went to Error.
func dpuConditionsSummary(dpu *provisioningv1.DPU) string {
	parts := make([]string, 0, len(dpu.Status.Conditions))
	for _, c := range dpu.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			parts = append(parts, fmt.Sprintf("%s=%s(%s: %s)", c.Type, c.Status, c.Reason, c.Message))
		}
	}
	return strings.Join(parts, "; ")
}

func expectDPUReady(g Gomega, name string) {
	dpu := observeDPU(g, name, "")
	g.Expect(dpu.Status.Phase).To(Equal(provisioningv1.DPUReady), "DPU %s is %s", name, dpu.Status.Phase)
}
```

- [ ] **Step 3: Route the joined checks through `observeDPU`**

Replace `expectDPUAgentJoined` and `expectDPUAgentRejoined` in `e2e_agent.go` with:

```go
func expectDPUAgentJoined(g Gomega, tenant client.Client, dpuName string) (*provisioningv1.DPU, *corev1.Node) {
	return expectDPUAgentJoinedSince(g, tenant, dpuName, "")
}

// expectDPUAgentJoinedSince is expectDPUAgentJoined for a DPU that must not be
// the one with staleUID.
func expectDPUAgentJoinedSince(g Gomega, tenant client.Client, dpuName, staleUID string) (*provisioningv1.DPU, *corev1.Node) {
	dpu := observeDPU(g, dpuName, staleUID)
	g.Expect(string(dpu.UID)).NotTo(Equal(staleUID), "%s is still the DPU from before the replacement", dpuName)
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

// expectDPUAgentRejoined is expectDPUAgentJoined for a DPU whose guest was
// replaced. The replacement keeps the DPU and Node names, and the old tenant
// Node can outlive the old guest, so a joined check alone can pass on it.
// The agent status has to be on a DPU other than the stale one, and the Node
// has to be reporting from a boot other than the stale one.
func expectDPUAgentRejoined(g Gomega, tenant client.Client, dpuName string, staleDPUUID, staleBootID string) {
	_, node := expectDPUAgentJoinedSince(g, tenant, dpuName, staleDPUUID)
	g.Expect(node.Status.NodeInfo.BootID).NotTo(BeEmpty(), "tenant Node %s reports no boot ID", dpuName)
	g.Expect(node.Status.NodeInfo.BootID).NotTo(Equal(staleBootID),
		"tenant Node %s still reports the boot from before the replacement", dpuName)
}
```

- [ ] **Step 4: Add `dpuReadyTimeout` to the const block in `e2e_test.go`** (after `bfbReadyTimeout`)

```go
	// dpuReadyTimeout bounds a DPU going from a joined kubelet to Ready:
	// DPF's DPU Cluster Config, Host Network Configuration, Service Readiness
	// and Node Effect Removal. The fleet deploys no DPUServices and uses
	// nodeEffect noEffect, so none of those has work of its own to wait for.
	dpuReadyTimeout = 5 * time.Minute
```

- [ ] **Step 5: Wait for Ready at the end of the first spec**

In `e2e_test.go`, after the "waiting for every SimInstance to be Ready with every child recorded" `Eventually` (the last statement of the first `It`), add:

```go
		// A joined kubelet is where the simulator's part ends, but DPF still
		// has phases to run; Ready is what DPF itself counts as provisioned.
		By("waiting for every DPU to reach Ready")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				expectDPUReady(g, fleetDPUName(ordinal))
			}
		}, dpuReadyTimeout, 5*time.Second).Should(Succeed())
```

- [ ] **Step 6: Wait for Ready after the replacement**

In the "replaces an ordinal whose VirtualMachine was deleted" spec, after the "waiting for the replacement to be Ready with a rotated credential and a rejoined agent" `Eventually` and before "verifying ordinal 0 was left alone", add:

```go
		By("waiting for the replacement DPU to reach Ready")
		Eventually(func(g Gomega) {
			expectDPUReady(g, dpuName)
		}, dpuReadyTimeout, 5*time.Second).Should(Succeed())
```

- [ ] **Step 7: Vet**

Run: `go vet -tags e2e ./test/e2e/`
Expected: no output, exit 0.

- [ ] **Step 8: Lint**

Run: `make lint`
Expected: exit 0.

- [ ] **Step 9: Commit**

```bash
git add test/e2e/e2e_agent.go test/e2e/e2e_test.go
git commit -m "test(e2e): wait for DPF to report every DPU Ready and stop on Error"
```

---

### Task 2: Verify what a provisioned DPU leaves behind

**Files:**
- Modify: `test/e2e/e2e_agent.go` (imports; new helpers)
- Modify: `test/e2e/e2e_test.go` (after the two Ready waits added in Task 1)

**Interfaces:**
- Consumes: `getDPU`, `expectGone(g Gomega, resource, name string)` (in `e2e_test.go`), `naming.KubeadmJoinSecretName`, `run`, `fleetNamespace`, `fleetDPUName`.
- Produces: `expectDPUProvisioned(g Gomega, tenant client.Client, dpuName string)`, `expectBootstrapTokensRevoked(g Gomega, tenant client.Client)`.

- [ ] **Step 1: Add the import to `e2e_agent.go`**

Add `coordinationv1 "k8s.io/api/coordination/v1"` to the Kubernetes group. `client-go/kubernetes/scheme` already registers it, so the tenant client needs no change.

- [ ] **Step 2: Add the helpers to `e2e_agent.go`** (after `expectDPUAgentRejoined`)

```go
// dpuNamespaceLabel is the label DPF puts on the bootstrap token Secrets it
// mints for a DPU, valued with the DPU's namespace. DPF declares it in an
// internal package.
const dpuNamespaceLabel = "provisioning.dpu.nvidia.com/dpu-namespace"

// expectDPUProvisioned checks what a DPU leaves behind once DPF reports it
// Ready: every agent operation succeeded, a tenant Node that is Ready and
// still heartbeating, and DPU Cluster Config having revoked the join Secret.
func expectDPUProvisioned(g Gomega, tenant client.Client, dpuName string) {
	dpu, err := getDPU(dpuName)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to get DPU %s", dpuName)
	g.Expect(dpu.Status.AgentStatus).NotTo(BeNil(), "%s has no agentStatus", dpuName)
	for _, cond := range dpu.Status.AgentStatus.Conditions {
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
			"DPU %s agent condition %s: %s %s", dpuName, cond.Type, cond.Reason, cond.Message)
	}

	ctx := context.Background()
	node := &corev1.Node{}
	g.Expect(tenant.Get(ctx, client.ObjectKey{Name: dpuName}, node)).To(Succeed(), "tenant Node %s is missing", dpuName)
	ready := false
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			ready = cond.Status == corev1.ConditionTrue
		}
	}
	g.Expect(ready).To(BeTrue(), "tenant Node %s is not Ready", dpuName)

	lease := &coordinationv1.Lease{}
	g.Expect(tenant.Get(ctx, client.ObjectKey{Namespace: "kube-node-lease", Name: dpuName}, lease)).To(Succeed(),
		"tenant Node %s has no Lease", dpuName)
	g.Expect(lease.Spec.RenewTime).NotTo(BeNil(), "Lease %s was never renewed", dpuName)
	g.Expect(time.Since(lease.Spec.RenewTime.Time)).To(BeNumerically("<", time.Minute), "Lease %s is stale", dpuName)

	expectGone(g, "secret", naming.KubeadmJoinSecretName(dpuName))
}

// expectBootstrapTokensRevoked checks that DPU Cluster Config revoked both
// bootstrap tokens DPF minted for the fleet's DPUs: the node-join token in the
// tenant cluster and the dpu-agent token in the management cluster. The label
// is per namespace, not per DPU, so this only holds once every DPU is Ready.
func expectBootstrapTokensRevoked(g Gomega, tenant client.Client) {
	tokens := &corev1.SecretList{}
	g.Expect(tenant.List(context.Background(), tokens, client.InNamespace("kube-system"),
		client.MatchingLabels{dpuNamespaceLabel: fleetNamespace})).To(Succeed())
	for _, s := range tokens.Items {
		g.Expect(s.Type).NotTo(Equal(corev1.SecretTypeBootstrapToken),
			"bootstrap token %s in the tenant cluster was not revoked", s.Name)
	}

	output, err := run(exec.Command("kubectl", "get", "secrets", "-n", "kube-system",
		"-l", dpuNamespaceLabel+"="+fleetNamespace,
		"-o", "go-template={{ range .items }}{{ .metadata.name }} {{ .type }}{{ \"\\n\" }}{{ end }}"))
	g.Expect(err).NotTo(HaveOccurred())
	for _, line := range nonEmptyLines(output) {
		g.Expect(line).NotTo(HaveSuffix(" "+string(corev1.SecretTypeBootstrapToken)),
			"bootstrap token in the management cluster was not revoked: %s", line)
	}
}
```

- [ ] **Step 3: Check after the first Ready wait**

In `e2e_test.go`, directly after the "waiting for every DPU to reach Ready" `Eventually` from Task 1 Step 5, add:

```go
		By("verifying what every provisioned DPU left behind")
		Eventually(func(g Gomega) {
			for ordinal := range replicas {
				expectDPUProvisioned(g, tenant, fleetDPUName(ordinal))
			}
			expectBootstrapTokensRevoked(g, tenant)
		}).Should(Succeed())
```

- [ ] **Step 4: Check after the replacement Ready wait**

Directly after the "waiting for the replacement DPU to reach Ready" `Eventually` from Task 1 Step 6, add:

```go
		By("verifying what the replacement DPU left behind")
		Eventually(func(g Gomega) {
			expectDPUProvisioned(g, tenant, dpuName)
			expectBootstrapTokensRevoked(g, tenant)
		}).Should(Succeed())
```

- [ ] **Step 5: Vet and lint**

Run: `go vet -tags e2e ./test/e2e/ && make lint`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/e2e_agent.go test/e2e/e2e_test.go
git commit -m "test(e2e): check agent conditions, Node health and revoked join material"
```

---

### Task 3: Let DPF clean up its own objects before forcing

**Files:**
- Modify: `test/e2e/e2e_test.go` (const block; `cleanupFleetInventory`; new `deleteGracefully`)

**Interfaces:**
- Consumes: `deleteFleetGracefully`, `stripFinalizersAndDelete`, `run`.
- Produces: `type namedObject struct{ resource, name string }`, `deleteGracefully(ns string, timeout time.Duration, objects []namedObject)`, const `dpfCleanupTimeout`.

- [ ] **Step 1: Add the timeout to the const block** (after `fleetDeletionTraceInterval`)

```go
	// dpfCleanupTimeout is the one budget DPF gets to run its own finalizers on
	// the objects the suite applied before cleanup strips them.
	dpfCleanupTimeout = 3 * time.Minute
```

- [ ] **Step 2: Replace `cleanupFleetInventory`**

```go
type namedObject struct{ resource, name string }

func cleanupFleetInventory(ns, fleetName string) {
	deleteFleetGracefully(ns, fleetName)

	// The fleet shares dpf-operator-system with the operator. Never delete
	// the namespace, and never list-and-wipe Secrets or other types: that
	// would take down DPF's CA, Issuer, and controller credentials.
	dpfObjects := []namedObject{
		{"dpuset", "e2e-dpuset"},
		{"dpucluster", "e2e-tenant"},
		{"tenantcontrolplane", "e2e-tenant"},
		{"dpuflavor", "e2e-flavor"},
		{"bfb", "e2e-bfb"},
		{"secret", "bmc-shared-password"},
	}
	// DPF's finalizers run its own cleanup: the DPUs of a DPUSet, and the
	// Kamaji control plane and keepalived behind a DPUCluster. Stripping them
	// skips that, and on a reused Kind cluster the leftovers carry into the
	// next run.
	By("deleting the DPF objects the suite applied through DPF's finalizers")
	deleteGracefully(ns, dpfCleanupTimeout, dpfObjects)

	By("force-deleting whatever SimFleet inventory is left")
	for _, item := range append([]namedObject{{"simfleet", fleetName}}, dpfObjects...) {
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

// deleteGracefully deletes objects without touching their finalizers and waits
// up to timeout for all of them to be gone. Like deleteFleetGracefully it never
// fails the suite: whatever is left is for the forced cleanup.
func deleteGracefully(ns string, timeout time.Duration, objects []namedObject) {
	for _, o := range objects {
		cmd := exec.Command("kubectl", "delete", o.resource, o.name, "-n", ns, "--ignore-not-found", "--wait=false")
		if _, err := run(cmd); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Could not delete %s %s: %s\n", o.resource, o.name, err)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		var left []string
		for _, o := range objects {
			output, err := run(exec.Command("kubectl", "get", o.resource, o.name, "-n", ns,
				"--ignore-not-found", "-o", "name"))
			if err != nil || strings.TrimSpace(output) != "" {
				left = append(left, o.resource+"/"+o.name)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(GinkgoWriter, "%v still exist after %s; forcing cleanup\n", left, timeout)
			return
		}
		time.Sleep(5 * time.Second)
	}
}
```

- [ ] **Step 3: Vet and lint**

Run: `go vet -tags e2e ./test/e2e/ && make lint`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/e2e_test.go
git commit -m "test(e2e): let DPF finalize the suite's objects before forcing cleanup"
```

---

### Task 4: Wait for bfb-registry after DPF reports Ready

**Files:**
- Modify: `hack/e2e/deploy-dpf.sh` (inside the `if [[ -n "${DPF_OPERATOR_CONFIG}" ]]` block, after the `DPFOperatorConfig` wait)

- [ ] **Step 1: Add the wait**

Insert after the closing `fi` of the `kubectl wait --for=condition=Ready dpfoperatorconfig/...` block and before the outer `fi`:

```bash
	# Every DPU's BFB transfer on the Redfish path goes through bfb-registry,
	# and the DPFOperatorConfig can report Ready before that pod exists.
	# kubectl wait fails at once on a selector that matches nothing yet.
	registry_selector="dpu.nvidia.com/component=bfb-registry"
	echo "Waiting for the bfb-registry pod..."
	for _ in $(seq 60); do
		if [[ -n "$("${KUBECTL_BIN}" get pods --namespace "${DPF_OPERATOR_NAMESPACE}" \
			-l "${registry_selector}" -o name)" ]]; then
			break
		fi
		sleep 5
	done
	if ! "${KUBECTL_BIN}" wait --for=condition=Ready pod -l "${registry_selector}" \
		--namespace "${DPF_OPERATOR_NAMESPACE}" \
		--timeout 5m; then
		dump_dpf_state
		exit 1
	fi
```

- [ ] **Step 2: Check syntax**

Run: `bash -n hack/e2e/deploy-dpf.sh && (command -v shellcheck >/dev/null && shellcheck hack/e2e/deploy-dpf.sh || true)`
Expected: exit 0, no shellcheck findings.

- [ ] **Step 3: Commit**

```bash
git add hack/e2e/deploy-dpf.sh
git commit -m "test(e2e): wait for bfb-registry before applying DPF objects"
```

---

### Task 5: Fold the delegated-address check into the BMC wait; set Gomega defaults in BeforeSuite

**Files:**
- Modify: `test/e2e/e2e_test.go` (`expectDeviceSelectable`; "waiting for every guest BMC address" step; delete "verifying every delegated pod address" step; remove the two `SetDefault…` calls)
- Modify: `test/e2e/e2e_suite_test.go`

The later step re-checked what `expectDeviceSelectable` already proves; only the uniqueness check was new. Its `10.0.2.2` check is redundant: `guestIP == launcherIP` already rules out KubeVirt's masquerade address, since a pod IP is never 10.0.2.2.

- [ ] **Step 1: Make `expectDeviceSelectable` return the address**

```go
func expectDeviceSelectable(g Gomega, ordinal int) string {
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
	g.Expect(bmcIP).To(Equal(guestIP),
		"%s publishes BMC address %q, but %s was delegated %q", device, bmcIP, vmiName, guestIP)

	pool, err := objectJSONPathErr("dpudevice", device, fleetNamespace,
		`{.metadata.labels['scale\.dpu\.nvidia\.com/pool']}`)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pool).To(Equal("e2e"))
	return guestIP
}
```

- [ ] **Step 2: Check uniqueness in the BMC wait**

Replace the body of the "waiting for every guest BMC address to make its device selectable" `Eventually`:

```go
		Eventually(func(g Gomega) {
			owners := map[string]int{}
			for ordinal := range replicas {
				ip := expectDeviceSelectable(g, ordinal)
				if other, taken := owners[ip]; taken {
					StopTrying(fmt.Sprintf("ordinals %d and %d were delegated the same address %s",
						other, ordinal, ip)).Now()
				}
				owners[ip] = ordinal
			}
		}, guestAgentTimeout(), 5*time.Second).Should(Succeed())
```

- [ ] **Step 3: Delete the later step**

Delete the whole block that starts with `By("verifying every delegated pod address is unique and published as a BMC address")` and ends with the closing `}` of its `for ordinal := range replicas` loop (just before `By("waiting for every DPU agent to join the tenant cluster")`). Keep the `dumpGuestVMDiagnostics()` call before it.

- [ ] **Step 4: Move the Gomega defaults**

Delete these two lines from the `Describe` body in `e2e_test.go`:

```go
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
```

In `e2e_suite_test.go`, add `"time"` to the imports and, after `TestE2E`:

```go
var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
})
```

- [ ] **Step 5: Vet and lint**

Run: `go vet -tags e2e ./test/e2e/ && make lint`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/e2e_test.go test/e2e/e2e_suite_test.go
git commit -m "test(e2e): check BMC address uniqueness where it is first published"
```

---

### Task 6: Use the naming helpers and check every ordinal

**Files:**
- Modify: `test/e2e/e2e_test.go` (first spec only)

- [ ] **Step 1: Validate every DPUDevice and DPUNode**

Replace the block from `By("validating the first DPUDevice")` through the last `Expect` of `By("validating the first DPUNode")` with:

```go
		for ordinal := range replicas {
			deviceName := naming.DeviceName(fleetName, ordinal)
			nodeName := naming.NodeName(fleetName, naming.HostIndex(ordinal, devicesPerDPUNode))

			By("validating DPUDevice " + deviceName)
			Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.spec.serialNumber}")).To(Equal(naming.SerialNumber(fleetName, ordinal)))
			// The DPUSet selector must not match yet. DPF opens Redfish against
			// spec.bmcIP as soon as it claims a device, and no guest has reported
			// an address at this point.
			Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace,
				`{.metadata.labels['scale\.dpu\.nvidia\.com/pool']}`)).To(BeEmpty())
			Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.spec.bmcIp}")).To(BeEmpty())
			Expect(objectLabel("dpudevice", deviceName, fleetNamespace, "provisioning.dpu.nvidia.com/dpunode-name")).To(Equal(nodeName))
			Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.metadata.ownerReferences[0].kind}")).To(Equal("SimInstance"))
			Expect(objectJSONPath("dpudevice", deviceName, fleetNamespace, "{.metadata.ownerReferences[0].name}")).To(Equal(naming.InstanceName(fleetName, ordinal)))
		}

		for host := range nodes {
			nodeName := naming.NodeName(fleetName, host)

			By("validating DPUNode " + nodeName)
			Expect(listedJSONPath("dpunode", nodeName, fleetNamespace, "{.spec.dpus[*].name}")).To(HaveLen(devicesPerDPUNode))
			Expect(objectJSONPath("dpunode", nodeName, fleetNamespace, "{.spec.nodeRebootMethod.external}")).NotTo(BeEmpty())
			Expect(objectJSONPath("dpunode", nodeName, fleetNamespace, "{.metadata.ownerReferences[0].kind}")).To(Equal("SimFleet"))
		}
```

- [ ] **Step 2: Replace the `"2"` literals in the InventoryReady wait**

In the "waiting for InventoryReady status" `Eventually`, change `Equal("2")` for `admittedReplicas` and `createdInstances` to `Equal(strconv.Itoa(replicas))`, and for `dpuNodes` to `Equal(strconv.Itoa(nodes))`.

- [ ] **Step 3: Name the cloud-init Secrets through the helper**

Replace `for _, secretName := range []string{ fleetName + "-dpu-000000-cloud-init", fleetName + "-dpu-000001-cloud-init", } {` with:

```go
		for ordinal := range replicas {
			secretName := naming.CloudInitSecretName(fleetName, ordinal)
```

(The loop body is unchanged.)

- [ ] **Step 4: Validate every VirtualMachine spec**

Replace `vmName := fleetName + "-dpu-000000"` and the four `Expect(objectJSONPath("virtualmachine", vmName, ...` assertions after it with:

```go
		for ordinal := range replicas {
			vmName := naming.VirtualMachineName(fleetName, ordinal)
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
		}
```

- [ ] **Step 5: Check every VirtualMachine Ready**

In the "waiting for VirtualMachines to become Ready" `Eventually`, change `Equal("2")` for `vmReadyInstances` to `Equal(strconv.Itoa(replicas))`, and replace the `firstVMReady` / `secondVMReady` blocks with:

```go
			for ordinal := range replicas {
				vmName := naming.VirtualMachineName(fleetName, ordinal)
				vmReady, err := objectJSONPathErr("virtualmachine", vmName, fleetNamespace, "{.status.ready}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(vmReady).To(Equal("true"), "VirtualMachine %s is not Ready", vmName)
			}
```

- [ ] **Step 6: Name the VMIs in the guest-agent wait through the helper**

Replace `for _, vmiName := range []string{fleetName + "-dpu-000000", fleetName + "-dpu-000001"} {` with:

```go
			for ordinal := range replicas {
				vmiName := naming.VirtualMachineName(fleetName, ordinal)
```

(The loop body is unchanged.)

- [ ] **Step 7: Check nothing hardcoded is left**

Run: `rg -n '"-(dpu|device|host)-0' test/e2e/e2e_test.go`
Expected: no matches.

- [ ] **Step 8: Vet and lint**

Run: `go vet -tags e2e ./test/e2e/ && make lint`
Expected: exit 0.

- [ ] **Step 9: Commit**

```bash
git add test/e2e/e2e_test.go
git commit -m "test(e2e): validate every ordinal and derive names from internal/naming"
```

---

### Task 7: Re-budget the e2e timeouts

New worst-case waits: Task 1 adds 5m in the first spec and 5m in the replacement; Task 2 adds the 2m default `Eventually` in each; Task 3 adds 3m; Task 5 removes the 2m-per-instance delegated-address wait (4m). The existing comment's 115m becomes 128m.

The existing budget does not count the fleet-deletion spec (6m) or `deleteFleetGracefully` in `AfterAll` (6m). This task keeps that convention; flag it in the PR description rather than changing it silently.

**Files:**
- Modify: `Makefile:161-169`
- Modify: `.github/workflows/e2e.yml:24-26`

- [ ] **Step 1: Update the Makefile budget**

Replace the comment and `E2E_TEST_TIMEOUT ?= 119m` with:

```make
# Must exceed the suite's own waits: go test panics the process at its timeout,
# which kills the failure diagnostics before Ginkgo can report them. Under
# software emulation those waits are 20m for VirtualMachine Ready plus 20m for
# the guest agent, 5m for the BFB download, 27m for Redfish provisioning and the
# tenant join, 5m for the DPUs to reach Ready and 2m for the provisioned checks;
# then 6m to tear the replaced ordinal down and 27m + 5m + 2m to bring it back,
# 6m to tear the scaled-away ordinal down, and 3m for DPF to finalize the
# suite's objects. That is 128m, which leaves the rest ~4m. The 24h management
# token lifetime is the hard ceiling.
E2E_TEST_TIMEOUT ?= 132m
```

- [ ] **Step 2: Update the job timeout**

In `.github/workflows/e2e.yml`, replace:

```yaml
    # Must exceed E2E_TEST_TIMEOUT (119m) plus the cluster, DPF, KubeVirt and
    # node image setup, or the job is killed before the suite reports.
    timeout-minutes: 180
```

with:

```yaml
    # Must exceed E2E_TEST_TIMEOUT (132m) plus the cluster, DPF, KubeVirt and
    # node image setup, or the job is killed before the suite reports.
    timeout-minutes: 195
```

- [ ] **Step 3: Check the two agree**

Run: `rg -n 'E2E_TEST_TIMEOUT \?=|E2E_TEST_TIMEOUT \(|timeout-minutes' Makefile .github/workflows/e2e.yml`
Expected: `132m` in the Makefile, `(132m)` and `195` in the workflow.

- [ ] **Step 4: Commit**

```bash
git add Makefile .github/workflows/e2e.yml
git commit -m "chore(e2e): budget the DPU Ready, provisioned and cleanup waits"
```

---

### Task 8: Run the suite in CI and resolve the open questions

These are assumptions the plan could not verify without a run. Each one stops the work for a decision; do not weaken an assertion to get green.

- [ ] **Step 1: Push and trigger the e2e job**

```bash
git push
gh pr edit --add-label e2e
gh run watch "$(gh run list --workflow e2e.yml --limit 1 --json databaseId -q '.[0].databaseId')"
```

- [ ] **Step 2: Check each open question in the job log**

| Question | Where to look | If it fails |
|---|---|---|
| Do the simulated DPUs reach `Ready` under Kamaji? | `DPU ... phase` lines after "waiting for every DPU to reach Ready" | Record the last phase and the DPU conditions from the diagnostics dump; bring them back before changing anything. |
| Is the tenant Node `Ready`? The guest bakes kube-flannel's conflist and relies on DPF's Flannel DPUService to copy `/opt/cni/bin/flannel` (see `images/simfleet-node/test.sh`); the suite never checks that DPUService comes up in the tenant cluster. | "verifying what every provisioned DPU left behind" failure message; `kubectl --kubeconfig <tenant> get ds -A` in the diagnostics | If Flannel is missing, find why the DPUService did not deploy before touching the check; drop Node Ready only if Flannel is deliberately out of scope (keep the Lease check either way). |
| Does DPU Cluster Config delete `{dpu}-kubeadm-join`? `expectOrdinalGone` checks it only at teardown, which suggests it may outlive Ready. | Same step, `secret ... still exists` | Check DPF's `dpu_cluster_config.go` for this path; if DPF keeps it, drop that one assertion. |
| Are all agent conditions `True` on the guest's mock agent? | Same step, `agent condition ...` | Compare the failing condition against `expectedMockAgentConditions` in DPF's `mock_dpuagent.go`. |
| Does `deleteGracefully` finish within 3m? | `still exist after 3m0s; forcing cleanup` in the AfterAll output | Name the object that hung; raise the budget in Task 7 or report a DPF finalizer issue. |

- [ ] **Step 3: Update the PR description** with the answers and the uncounted 12m noted in Task 7.

---

## Out of scope

- Switching the suite from `kubectl` jsonpath to typed DPF clients. Worth doing, but it touches every helper and is not needed for any fix above.
