/*
Copyright 2026 NVIDIA

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

package mockovs

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	oflow "github.com/nvidia/doca-platform/pkg/openflow"
	"github.com/nvidia/doca-platform/pkg/ovsmodel"
	"github.com/nvidia/doca-platform/pkg/ovsutils"
	"github.com/ovn-org/libovsdb/client"

	"antrea.io/antrea/pkg/ovs/openflow"
	kexec "k8s.io/utils/exec"
	"k8s.io/utils/ptr"
)

func start(t *testing.T, cfg Config) (string, ovsutils.API) {
	t.Helper()
	// Unix socket paths are limited to 108 bytes, so avoid the long t.TempDir().
	dir, err := os.MkdirTemp("", "mockovs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg.RunDir = dir
	if len(cfg.Bridges) == 0 {
		cfg.Bridges = []string{"br-sfc"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	cfg.Ready = func(*Instance) { close(ready) }
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, VswitchdPidFile)); !os.IsNotExist(err) {
			t.Errorf("pidfile not removed: %v", err)
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("mock-ovs not ready")
	}

	api, err := connectOVSDB(ctx, "unix:"+filepath.Join(dir, DBSocket))
	if err != nil {
		t.Fatal(err)
	}
	return dir, api
}

// connectOVSDB opens an OVSDB client the same way dpf-ovs-cni does. The
// connector lives under internal/ in the doca-platform module, which this
// module cannot import, so the logic is copied here.
func connectOVSDB(ctx context.Context, ovsSocket string) (ovsutils.API, error) {
	dbmodel, err := ovsmodel.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	// Without a logger, libovsdb sets the global stdr verbosity to 5 on every
	// new client, which logs every cache update and races with the goroutines
	// of the other clients and the server in this process.
	log := logr.Discard()
	ovsDB, err := client.NewOVSDBClient(dbmodel, client.WithEndpoint(ovsSocket), client.WithLogger(&log))
	if err != nil {
		return nil, err
	}
	if err := ovsDB.Connect(ctx); err != nil {
		return nil, err
	}
	if _, err := ovsDB.MonitorAll(ctx); err != nil {
		return nil, err
	}
	return &ovsutils.Client{Client: ovsDB}, nil
}

func waitIface(t *testing.T, api ovsutils.API, name string, ok func(*ovsmodel.Interface) bool) *ovsmodel.Interface {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		iface, err := api.GetIfaceWithName(context.Background(), name)
		if err == nil && ok(iface) {
			return iface
		}
		if time.Now().After(deadline) {
			t.Fatalf("interface %s: last=%+v err=%v", name, iface, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hasOfport(i *ovsmodel.Interface) bool { return i.Ofport != nil }

func addPort(t *testing.T, api ovsutils.API, pc ovsutils.PortConfig) {
	t.Helper()
	if pc.BridgeName == "" {
		pc.BridgeName = "br-sfc"
	}
	if err := api.AddPort(context.Background(), pc); err != nil {
		t.Fatalf("AddPort %s: %v", pc.Name, err)
	}
}

func TestOfportAllocation(t *testing.T) {
	_, api := start(t, Config{
		Bridges:        []string{"br-sfc", "br-hbn"},
		PhysicalPorts:  []string{"p0", "p1"},
		FailInterfaces: []string{"bad*"},
	})
	ctx := context.Background()

	addPort(t, api, ovsutils.PortConfig{Name: "pf0hpf"})
	addPort(t, api, ovsutils.PortConfig{Name: "p1"})
	addPort(t, api, ovsutils.PortConfig{Name: "x1", BridgeName: "br-hbn", OFPortRequest: ptr.To(7), WaitForOFPortFree: true})
	addPort(t, api, ovsutils.PortConfig{Name: "bad0"})

	for name, want := range map[string]int{"br-sfc": 65534, "p1": 2, "pf0hpf": 3, "x1": 7} {
		iface := waitIface(t, api, name, hasOfport)
		if *iface.Ofport != want {
			t.Errorf("%s ofport = %d, want %d", name, *iface.Ofport, want)
		}
		if state, _ := api.GetInterfaceLinkState(ctx, name); state != ovsmodel.InterfaceLinkStateUp {
			t.Errorf("%s link_state = %q", name, state)
		}
	}

	bad := waitIface(t, api, "bad0", hasOfport)
	if *bad.Ofport != -1 || bad.Error == nil {
		t.Errorf("bad0 = ofport %d error %v", *bad.Ofport, bad.Error)
	}
	withErr, err := api.ListInterfacesWithError(ctx)
	if err != nil || len(withErr) != 1 || withErr[0] != "bad0" {
		t.Errorf("ListInterfacesWithError = %v, %v", withErr, err)
	}

	if br, err := api.GetBridgeNameByInterface(ctx, "x1"); err != nil || br != "br-hbn" {
		t.Errorf("GetBridgeNameByInterface(x1) = %q, %v", br, err)
	}

	if err := api.DelPortByName(ctx, "br-sfc", "pf0hpf", nil); err != nil {
		t.Fatal(err)
	}
	addPort(t, api, ovsutils.PortConfig{Name: "pf1hpf"})
	if iface := waitIface(t, api, "pf1hpf", hasOfport); *iface.Ofport != 3 {
		t.Errorf("freed ofport not reused: pf1hpf = %d", *iface.Ofport)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var ovs []ovsmodel.OpenvSwitch
		if err := api.List(ctx, &ovs); err != nil || len(ovs) != 1 {
			t.Fatalf("Open_vSwitch rows = %v, %v", ovs, err)
		}
		if ovs[0].CurCfg == ovs[0].NextCfg {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cur_cfg %d != next_cfg %d", ovs[0].CurCfg, ovs[0].NextCfg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOfportDelay(t *testing.T) {
	_, api := start(t, Config{OfportDelay: 500 * time.Millisecond})
	added := time.Now()
	addPort(t, api, ovsutils.PortConfig{Name: "slow"})

	time.Sleep(200 * time.Millisecond)
	if iface, err := api.GetIfaceWithName(context.Background(), "slow"); err != nil || iface.Ofport != nil {
		t.Fatalf("ofport assigned before the delay: %+v, %v", iface, err)
	}
	waitIface(t, api, "slow", hasOfport)
	if d := time.Since(added); d < 500*time.Millisecond {
		t.Errorf("ofport assigned after %v", d)
	}
}

// requireTool skips the test when an Open vSwitch tool is not installed, so
// the tests run on machines without Open vSwitch. When CI is set, which
// GitHub Actions does, a missing tool fails the test instead: a skipped test
// would leave the OpenFlow and OVSDB protocol code untested without anyone
// noticing.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err == nil {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s is not installed; CI must install Open vSwitch", name)
	}
	t.Skipf("%s not installed", name)
}

// TestOVSVsctl checks that the OVS C IDL, which needs the compatibility proxy,
// can transact and see vswitchd's cur_cfg acknowledgment.
func TestOVSVsctl(t *testing.T) {
	requireTool(t, "ovs-vsctl")
	dir, _ := start(t, Config{PhysicalPorts: []string{"p0"}, FailInterfaces: []string{"bad*"}})
	vsctl := func(args ...string) (string, error) {
		args = append([]string{"--db=unix:" + filepath.Join(dir, DBSocket), "--timeout=5", "-vovsdb_idl:off"}, args...)
		out, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
		return string(out), err
	}
	if out, err := vsctl("add-port", "br-sfc", "p0", "--", "add-port", "br-sfc", "pf0hpf"); err != nil {
		t.Fatalf("add-port: %v: %s", err, out)
	}
	if out, err := vsctl("get", "interface", "pf0hpf", "ofport"); err != nil || out != "2\n" {
		t.Errorf("pf0hpf ofport = %q, %v", out, err)
	}
	// Like real OVS, ovs-vsctl reports the interface error but still succeeds.
	if out, _ := vsctl("add-port", "br-sfc", "bad0"); !strings.Contains(out, "could not open network device bad0") {
		t.Errorf("add-port bad0 = %q", out)
	}
	if out, err := vsctl("del-port", "br-sfc", "pf0hpf"); err != nil {
		t.Errorf("del-port: %v: %s", err, out)
	}
	if out, err := vsctl("list-ports", "br-sfc"); err != nil || out != "bad0\np0\n" {
		t.Errorf("list-ports = %q, %v", out, err)
	}
}

// TestOpenFlow drives the switch the way sfc-controller does: Antrea's
// OFBridge over the .mgmt socket, and pkg/openflow shelling out to ovs-ofctl.
func TestOpenFlow(t *testing.T) {
	requireTool(t, "ovs-ofctl")
	dir, api := start(t, Config{})
	t.Setenv("OVS_RUNDIR", dir)
	ctx := context.Background()

	// Register the table before connecting, as Antrea does. OFBridge reads its
	// table cache without a lock in the goroutine it starts when the switch
	// connects, so NewTable after Connect is a data race inside Antrea.
	// Connecting initializes the registered tables.
	ofb := openflow.NewOFBridge("br-sfc", filepath.Join(dir, "br-sfc.mgmt"))
	ofb.NewTable(openflow.NewOFTable(0, "Learning", 0, 0, openflow.TableMissActionNone), 0, openflow.TableMissActionNone)
	if err := ofb.Connect(5, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ofb.Disconnect() }()

	of := &oflow.OpenFlow{Exec: kexec.New()}
	if err := of.AddMeter(ctx, "meter=1,kbps,band=type=drop,rate=1000", "br-sfc"); err != nil {
		t.Fatal(err)
	}
	flows := "table=0,priority=100,in_port=1,cookie=0x55,actions=output:2\n" +
		"table=0,priority=5,ip,nw_dst=10.0.0.1,cookie=0x55,actions=meter:1,goto_table:1\n" +
		"table=1,priority=10,cookie=0x66,actions=drop\n"
	if err := of.AddFlows(ctx, flows, "br-sfc"); err != nil {
		t.Fatal(err)
	}

	got, err := ofb.DumpFlows(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0x55] == nil || got[0x66] == nil {
		t.Fatalf("DumpFlows = %v", got)
	}
	if err := ofb.DeleteFlowsByCookie(0x55, math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	if got, err = ofb.DumpFlows(0, 0); err != nil || len(got) != 1 || got[0x66] == nil {
		t.Fatalf("after delete DumpFlows = %v, %v", got, err)
	}

	if err := api.AddBridge(ctx, ovsutils.BridgeConfig{Name: "br-new", DatapathType: "netdev"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "br-new.mgmt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no management socket for a bridge added over OVSDB")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, err := exec.Command("ovs-ofctl", "-O", "OpenFlow13", "dump-flows", "br-new").CombinedOutput(); err != nil {
		t.Fatalf("dump-flows br-new: %v: %s", err, out)
	}
}
