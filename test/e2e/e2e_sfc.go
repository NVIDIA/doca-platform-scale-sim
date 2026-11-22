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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	dpuservicev1 "github.com/nvidia/doca-platform/api/dpuservice/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// sfcChainName is the DPUServiceChain both SFC specs create. Its
	// ServiceChainSet and ServiceChains on the tenant cluster carry the same
	// name and namespace.
	sfcChainName = "e2e-chain"

	// The labels the ServiceChainSet controller puts on the ServiceChains it
	// creates, spelled out because the package that defines them is internal
	// to doca-platform.
	serviceChainSetNameLabel      = "svc.dpu.nvidia.com/servicechainset-name"
	serviceChainSetNamespaceLabel = "svc.dpu.nvidia.com/servicechainset-namespace"

	// nsiNamespace is where DPF keeps every NodeServiceInterfaces object. It
	// is dpf-operator-system on the tenant cluster as well.
	nsiNamespace = dpfOperatorNamespace

	// readyCondition is the aggregate condition DPF sets on every object here.
	readyCondition = "Ready"

	// sfcControllerReadyTimeout bounds the sfc-controller DaemonSet becoming
	// Ready on every node. The DPUService is installed by the operator as soon
	// as the DPUCluster exists, but its pods are scheduled when the nodes
	// join, and each guest pulls the image itself.
	sfcControllerReadyTimeout = 10 * time.Minute

	// sfcReconcileTimeout bounds the host objects, the ServiceChains and the
	// NodeServiceInterfaces reaching their reconciled conditions once
	// sfc-controller is running. It is several reconciles deep: the host
	// controller creates the sets, the set controllers create the per-node
	// objects, and sfc-controller then programs each node.
	sfcReconcileTimeout = 5 * time.Minute

	// sfcDeletionTimeout bounds one set of DPUServiceInterfaces, chains and
	// services going from deleted to gone on both clusters.
	sfcDeletionTimeout = 5 * time.Minute

	// representorFailure is in the Ready message of a PF or VF entry that
	// sfc-controller cannot resolve: it looks up the ECPF's representor, and a
	// guest has no PCI functions to find one on.
	representorFailure = "ECPF"

	// notReadyMessage is in a ServiceChain's Reconciled message when a port it
	// names is not Ready.
	notReadyMessage = "is not ready"

	// dummyServiceName is the DPUService in testdata/dummy.
	dummyServiceName = "e2e-dummy"
)

// The dummy chart is not published anywhere, so the suite cannot assume a
// repository for it. These are the environment variables that point it at
// one; E2E_DUMMY_CHART_REPO switches the dummy DPUService spec on.
const (
	dummyChartRepoEnv       = "E2E_DUMMY_CHART_REPO"
	dummyChartNameEnv       = "E2E_DUMMY_CHART_NAME"
	dummyChartVersionEnv    = "E2E_DUMMY_CHART_VERSION"
	dummyImageRepositoryEnv = "E2E_DUMMY_IMAGE_REPOSITORY"
	dummyImageTagEnv        = "E2E_DUMMY_IMAGE_TAG"
)

// sfcObject names one object the SFC specs create on the host cluster.
type sfcObject struct{ resource, name string }

var (
	physicalChainObjects = []sfcObject{
		{"dpuservicechain", sfcChainName},
		{"dpuserviceinterface", "e2e-p0"},
		{"dpuserviceinterface", "e2e-pf0hpf"},
	}
	dummyChainObjects = []sfcObject{
		{"dpuservicechain", sfcChainName},
		{"dpuservice", dummyServiceName},
		{"dpuserviceinterface", "e2e-dummy-eth1"},
		{"dpuserviceinterface", "e2e-dummy-eth2"},
		{"dpuserviceinterface", "e2e-p0"},
		{"dpuserviceinterface", "e2e-pf0hpf"},
	}
)

func init() {
	// The tenant client uses the client-go scheme, which has no DPF types.
	utilruntime.Must(dpuservicev1.AddToScheme(scheme.Scheme))
}

// dummyChartRepo is the repository the dummy DPUService's chart is pulled
// from, or empty when the suite was not given one.
func dummyChartRepo() string {
	return strings.TrimSpace(os.Getenv(dummyChartRepoEnv))
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// renderDummyDPUService fills in the chart repository and the image the dummy
// DPUService runs. The default image is the pause container: the chart's
// DaemonSet gives the container no command, so any image that stays running
// does, and pause is the smallest one a guest can pull.
func renderDummyDPUService() (string, error) {
	template, err := os.ReadFile(filepath.Join("testdata", "dummy", "dpuservice.yaml.tmpl"))
	if err != nil {
		return "", fmt.Errorf("failed to read the dummy DPUService template: %w", err)
	}
	return strings.NewReplacer(
		"${E2E_DUMMY_CHART_REPO}", dummyChartRepo(),
		"${E2E_DUMMY_CHART_NAME}", envOrDefault(dummyChartNameEnv, "dummydpuservice-chart"),
		"${E2E_DUMMY_CHART_VERSION}", envOrDefault(dummyChartVersionEnv, "v0.1.0"),
		"${E2E_DUMMY_IMAGE_REPOSITORY}", envOrDefault(dummyImageRepositoryEnv, "registry.k8s.io/pause"),
		"${E2E_DUMMY_IMAGE_TAG}", envOrDefault(dummyImageTagEnv, "3.10"),
	).Replace(string(template)), nil
}

// applyManifestDir applies every manifest in a testdata directory. DPF's
// admission webhooks and CRDs can lag the DPFOperatorConfig going Ready, so
// the caller retries it.
func applyManifestDir(g Gomega, dir string) {
	cmd := exec.Command("kubectl", "apply", "-f", filepath.Join("testdata", dir))
	_, err := run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to apply testdata/%s", dir)
}

// expectHostConditionsTrue checks conditions on a host cluster object. A
// condition that is absent reads as empty and fails the same way a False one
// does.
func expectHostConditionsTrue(g Gomega, resource, name string, types ...string) {
	for _, conditionType := range types {
		status, err := objectJSONPathErr(resource, name, fleetNamespace,
			fmt.Sprintf(`{.status.conditions[?(@.type=="%s")].status}`, conditionType))
		g.Expect(err).NotTo(HaveOccurred(), "Failed to read %s/%s", resource, name)
		g.Expect(status).To(Equal("True"), "%s/%s condition %s", resource, name, conditionType)
	}
}

// expectHostConditionNotTrue checks that a host cluster object does not report
// a condition as True. Callers pair it with a check on the tenant cluster that
// shows why, because an absent condition passes it too.
func expectHostConditionNotTrue(g Gomega, resource, name, conditionType string) {
	status, err := objectJSONPathErr(resource, name, fleetNamespace,
		fmt.Sprintf(`{.status.conditions[?(@.type=="%s")].status}`, conditionType))
	g.Expect(err).NotTo(HaveOccurred(), "Failed to read %s/%s", resource, name)
	g.Expect(status).NotTo(Equal("True"), "%s/%s condition %s", resource, name, conditionType)
}

// expectObjectConditionsTrue checks conditions on a tenant cluster object. A
// condition only counts when it was written for the object's current
// generation; a True from before the last spec change is what a controller
// that has not caught up still shows.
func expectObjectConditionsTrue(g Gomega, description string, generation int64, conditions []metav1.Condition, types ...string) {
	for _, conditionType := range types {
		condition := meta.FindStatusCondition(conditions, conditionType)
		g.Expect(condition).NotTo(BeNil(), "%s has no %s condition", description, conditionType)
		g.Expect(condition.Status).To(Equal(metav1.ConditionTrue),
			"%s condition %s is %s: %s", description, conditionType, condition.Status, condition.Message)
		g.Expect(condition.ObservedGeneration).To(Equal(generation),
			"%s condition %s was written for generation %d, not %d", description, conditionType, condition.ObservedGeneration, generation)
	}
}

// expectDaemonSetsReady lists the tenant cluster's DaemonSets by label and
// requires each to have rolled out to every node.
func expectDaemonSetsReady(g Gomega, tenant client.Client, selector map[string]string, wantNodes int) {
	list := &appsv1.DaemonSetList{}
	g.Expect(tenant.List(context.Background(), list, client.MatchingLabels(selector))).To(Succeed())
	g.Expect(list.Items).NotTo(BeEmpty(), "no DaemonSet on the tenant cluster has labels %v", selector)
	for i := range list.Items {
		ds := &list.Items[i]
		name := ds.Namespace + "/" + ds.Name
		g.Expect(ds.Status.ObservedGeneration).To(Equal(ds.Generation), "DaemonSet %s has not observed its spec", name)
		g.Expect(ds.Status.DesiredNumberScheduled).To(BeEquivalentTo(wantNodes), "DaemonSet %s is not scheduled on every node", name)
		g.Expect(ds.Status.NumberReady).To(BeEquivalentTo(wantNodes), "DaemonSet %s is not Ready on every node", name)
	}
}

// expectSFCControllerReady waits on the DaemonSet the DPF operator deploys
// from the dpu-networking chart. The chart labels it with its own name.
func expectSFCControllerReady(g Gomega, tenant client.Client, wantNodes int) {
	expectDaemonSetsReady(g, tenant, map[string]string{"app.kubernetes.io/name": "sfc-controller"}, wantNodes)
}

// expectServiceChainsNotReconciled checks that sfc-controller handled the
// chain on every node and refused it: one ServiceChain per node, each
// reporting Reconciled=False with a message containing the given text.
func expectServiceChainsNotReconciled(g Gomega, tenant client.Client, chain string, wantNodes int, message string) {
	list := &dpuservicev1.ServiceChainList{}
	g.Expect(tenant.List(context.Background(), list,
		client.InNamespace(fleetNamespace),
		client.MatchingLabels{
			serviceChainSetNameLabel:      chain,
			serviceChainSetNamespaceLabel: fleetNamespace,
		})).To(Succeed())
	g.Expect(list.Items).To(HaveLen(wantNodes), "ServiceChains of %s", chain)

	reconciled := string(dpuservicev1.ServiceChainReconciled)
	for i := range list.Items {
		serviceChain := &list.Items[i]
		condition := meta.FindStatusCondition(serviceChain.Status.Conditions, reconciled)
		g.Expect(condition).NotTo(BeNil(), "ServiceChain %s has no %s condition", serviceChain.Name, reconciled)
		g.Expect(condition.Status).To(Equal(metav1.ConditionFalse),
			"ServiceChain %s condition %s is %s", serviceChain.Name, reconciled, condition.Status)
		g.Expect(condition.Message).To(ContainSubstring(message),
			"ServiceChain %s condition %s message", serviceChain.Name, reconciled)
	}
}

// sfcNodeServiceInterfaces lists the NodeServiceInterfaces the SFC controllers
// own. Other types, such as the VPC provisioner's, share the namespace.
func sfcNodeServiceInterfaces(g Gomega, tenant client.Client) []dpuservicev1.NodeServiceInterfaces {
	list := &dpuservicev1.NodeServiceInterfacesList{}
	g.Expect(tenant.List(context.Background(), list, client.InNamespace(nsiNamespace))).To(Succeed())
	var sfc []dpuservicev1.NodeServiceInterfaces
	for i := range list.Items {
		if list.Items[i].Spec.Type == dpuservicev1.NSITypeSFC {
			sfc = append(sfc, list.Items[i])
		}
	}
	return sfc
}

// expectNodeServiceInterfaceEntries checks that every node has an SFC
// NodeServiceInterfaces object holding exactly the ready and failing
// DPUServiceInterfaces. Each ready entry is reported Ready for its current
// spec. Each failing entry is reported Ready=False with a message containing
// the given text, and the object itself then reports Reconciled=False, since
// sfc-controller reconciles an object only when all its entries succeed.
//
// An entry is named <namespace>_<ServiceInterfaceSet name>, and the set takes
// its DPUServiceInterface's name and namespace.
func expectNodeServiceInterfaceEntries(g Gomega, tenant client.Client, wantNodes int, ready []string, failing map[string]string) {
	nsis := sfcNodeServiceInterfaces(g, tenant)
	g.Expect(nsis).To(HaveLen(wantNodes), "SFC NodeServiceInterfaces objects")

	seen := map[string]struct{}{}
	for i := range nsis {
		nsi := &nsis[i]
		seen[nsi.Spec.Node] = struct{}{}

		g.Expect(nsi.Spec.Interfaces).To(HaveLen(len(ready)+len(failing)),
			"NodeServiceInterfaces %s entries: %v", nsi.Name, interfaceEntryNames(nsi))
		for _, name := range ready {
			entry := findInterfaceEntry(nsi, name)
			g.Expect(entry).NotTo(BeNil(), "NodeServiceInterfaces %s has no entry for %s: %v", nsi.Name, name, interfaceEntryNames(nsi))
			g.Expect(entry.Terminating).To(BeFalse(), "NodeServiceInterfaces %s entry %s is terminating", nsi.Name, entry.Name)
			g.Expect(nsi.IsEntryReady(entry)).To(BeTrue(),
				"NodeServiceInterfaces %s entry %s is not Ready: %+v", nsi.Name, entry.Name, nsi.FindInterfaceEntryStatus(entry.Name))
		}
		for name, message := range failing {
			entry := findInterfaceEntry(nsi, name)
			g.Expect(entry).NotTo(BeNil(), "NodeServiceInterfaces %s has no entry for %s: %v", nsi.Name, name, interfaceEntryNames(nsi))
			status := nsi.FindInterfaceEntryStatus(entry.Name)
			g.Expect(status).NotTo(BeNil(), "NodeServiceInterfaces %s entry %s has no status yet", nsi.Name, entry.Name)
			condition := meta.FindStatusCondition(status.Conditions, readyCondition)
			g.Expect(condition).NotTo(BeNil(), "NodeServiceInterfaces %s entry %s has no Ready condition", nsi.Name, entry.Name)
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse),
				"NodeServiceInterfaces %s entry %s Ready is %s", nsi.Name, entry.Name, condition.Status)
			g.Expect(condition.Message).To(ContainSubstring(message),
				"NodeServiceInterfaces %s entry %s Ready message", nsi.Name, entry.Name)
		}

		reconciled := string(dpuservicev1.NodeServiceInterfacesReconciled)
		if len(failing) == 0 {
			expectObjectConditionsTrue(g, "NodeServiceInterfaces "+nsi.Name, nsi.Generation, nsi.Status.Conditions,
				readyCondition, reconciled)
			continue
		}
		condition := meta.FindStatusCondition(nsi.Status.Conditions, reconciled)
		g.Expect(condition).NotTo(BeNil(), "NodeServiceInterfaces %s has no %s condition", nsi.Name, reconciled)
		g.Expect(condition.Status).To(Equal(metav1.ConditionFalse),
			"NodeServiceInterfaces %s condition %s is %s", nsi.Name, reconciled, condition.Status)
	}
	g.Expect(seen).To(HaveLen(wantNodes), "SFC NodeServiceInterfaces are not one per node")
}

func findInterfaceEntry(nsi *dpuservicev1.NodeServiceInterfaces, interfaceName string) *dpuservicev1.InterfaceEntry {
	for i := range nsi.Spec.Interfaces {
		if nsi.Spec.Interfaces[i].Name == fleetNamespace+"_"+interfaceName {
			return &nsi.Spec.Interfaces[i]
		}
	}
	return nil
}

func interfaceEntryNames(nsi *dpuservicev1.NodeServiceInterfaces) []string {
	names := make([]string, 0, len(nsi.Spec.Interfaces))
	for _, entry := range nsi.Spec.Interfaces {
		names = append(names, entry.Name)
	}
	return names
}

// deleteSFCObjects deletes the host objects and waits for them, and for what
// they own on the tenant cluster, to be gone. An SFC NodeServiceInterfaces is
// deleted once its last entry is, so none left means every entry was released
// by sfc-controller.
func deleteSFCObjects(tenant client.Client, objects []sfcObject) {
	for _, object := range objects {
		cmd := exec.Command("kubectl", "delete", object.resource, object.name, "-n", fleetNamespace,
			"--ignore-not-found", "--wait=false")
		_, err := run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete %s/%s", object.resource, object.name)
	}
	Eventually(func(g Gomega) {
		for _, object := range objects {
			expectGone(g, object.resource, object.name)
		}
		serviceChains := &dpuservicev1.ServiceChainList{}
		g.Expect(tenant.List(context.Background(), serviceChains, client.InNamespace(fleetNamespace),
			client.MatchingLabels{serviceChainSetNameLabel: sfcChainName})).To(Succeed())
		g.Expect(serviceChains.Items).To(BeEmpty(), "ServiceChains left behind")
		g.Expect(sfcNodeServiceInterfaces(g, tenant)).To(BeEmpty(), "SFC NodeServiceInterfaces left behind")
	}, sfcDeletionTimeout, 5*time.Second).Should(Succeed())
}

// cleanupSFCObjects is the AfterAll for a run that failed between creating the
// SFC objects and the spec deleting them. It gives the controllers one
// deletion budget, then forces the rest, and never fails the suite: the
// tenant cluster goes with the fleet, so a leftover there is harmless, and a
// finalizer left on the host would only hold the fleet's own cleanup.
func cleanupSFCObjects() {
	By("deleting the SFC objects the specs left behind")
	for _, object := range dummyChainObjects {
		cmd := exec.Command("kubectl", "delete", object.resource, object.name, "-n", fleetNamespace,
			"--ignore-not-found", "--wait=false")
		_, _ = run(cmd)
	}
	deadline := time.Now().Add(sfcDeletionTimeout)
	for time.Now().Before(deadline) {
		remaining := false
		for _, object := range dummyChainObjects {
			output, err := run(exec.Command("kubectl", "get", object.resource, object.name, "-n", fleetNamespace,
				"--ignore-not-found", "-o", "name"))
			if err != nil || strings.TrimSpace(output) != "" {
				remaining = true
			}
		}
		if !remaining {
			return
		}
		time.Sleep(5 * time.Second)
	}
	for _, object := range dummyChainObjects {
		stripFinalizersAndDelete(object.resource, object.name, fleetNamespace)
	}
}

// dumpSFCDiagnostics prints what explains an SFC spec that never reconciled:
// the host objects' conditions, and on the tenant cluster the objects and the
// sfc-controller pods that should have acted on them. It never fails.
func dumpSFCDiagnostics() {
	for _, object := range dummyChainObjects {
		description, err := run(exec.Command("kubectl", "describe", object.resource, object.name, "-n", fleetNamespace))
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe %s/%s: %s\n", object.resource, object.name, err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "%s/%s description:\n%s\n", object.resource, object.name, description)
	}

	err := withTenantKubeconfig(func(path string) error {
		for _, args := range [][]string{
			{"get", "servicechains,serviceinterfaces,nodeserviceinterfaces", "-n", fleetNamespace, "-o", "yaml"},
			{"get", "daemonsets,pods", "-A", "-o", "wide"},
			{"logs", "-n", fleetNamespace, "-l", "app.kubernetes.io/name=sfc-controller", "--tail=200", "--prefix"},
		} {
			cmd := exec.Command("kubectl", append([]string{"--kubeconfig", path}, args...)...)
			output, err := run(cmd)
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to run kubectl %s on the tenant cluster: %s\n", strings.Join(args, " "), err)
				continue
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Tenant cluster kubectl %s:\n%s\n", strings.Join(args, " "), output)
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to reach the tenant cluster: %s\n", err)
	}
}
