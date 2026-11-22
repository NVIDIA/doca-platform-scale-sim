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

// Package vswitchd emulates the OVSDB side effects of ovs-vswitchd: it seeds
// the root row and bridges, and fills ofport, link_state, admin_state, error
// and cur_cfg the way ovs-vswitchd does.
package vswitchd

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nvidia/doca-platform/pkg/ovsmodel"
	"github.com/nvidia/doca-platform/pkg/ovsutils"

	"github.com/go-logr/logr"
	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/utils/ptr"
)

const (
	// LocalOfport is the ofport of the bridge's own internal interface.
	LocalOfport = 65534
	// MaxOfport is the highest ofport a regular interface can get.
	MaxOfport = 65279
	// FailedOfport is the ofport ovs-vswitchd reports for interfaces it could not open.
	FailedOfport = -1

	ovsVersion = "3.3.0-mock"
)

// Config configures the emulator.
type Config struct {
	// Endpoint is the OVSDB endpoint, e.g. unix:/var/run/openvswitch/db.sock.
	Endpoint string
	// Bridges are created at startup with datapath_type netdev and fail_mode secure.
	Bridges []string
	// PhysicalPorts get ofports 1..N, in order, on whatever bridge they are added to,
	// like the uplinks of a real DPU.
	PhysicalPorts []string
	// OfportDelay is how long an interface waits after it first appears before it gets
	// an ofport and link_state up, to model ovs-vswitchd latency.
	OfportDelay time.Duration
	// FailInterfaces are path.Match patterns. Matching interfaces get ofport -1 and an
	// error, the way ovs-vswitchd reports a missing netdev.
	FailInterfaces []string
	// Log receives libovsdb client logs; the zero value discards them.
	Log logr.Logger
}

// Emulator is a libovsdb client that plays the role of ovs-vswitchd.
type Emulator struct {
	cfg       Config
	ovs       client.Client
	trigger   chan struct{}
	onBridges func([]string)

	sync.Mutex
	firstSeen map[string]time.Time
	bridges   []string
}

// New connects to cfg.Endpoint, monitors the database and seeds the root row and bridges.
// onBridges, if set, is called with the sorted bridge names whenever they change.
func New(ctx context.Context, cfg Config, onBridges func([]string)) (*Emulator, error) {
	for _, p := range cfg.FailInterfaces {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid fail-interfaces pattern %q: %w", p, err)
		}
	}
	dbModel, err := ovsmodel.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	log := cfg.Log
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	ovs, err := client.NewOVSDBClient(dbModel, client.WithEndpoint(cfg.Endpoint), client.WithLogger(&log))
	if err != nil {
		return nil, err
	}
	if err := ovs.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect to %s: %w", cfg.Endpoint, err)
	}
	e := &Emulator{
		cfg:       cfg,
		ovs:       ovs,
		trigger:   make(chan struct{}, 1),
		onBridges: onBridges,
		firstSeen: map[string]time.Time{},
	}
	ovs.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc:    func(string, model.Model) { e.poke() },
		UpdateFunc: func(string, model.Model, model.Model) { e.poke() },
		DeleteFunc: func(string, model.Model) { e.poke() },
	})
	if _, err := ovs.MonitorAll(ctx); err != nil {
		ovs.Close()
		return nil, fmt.Errorf("monitor: %w", err)
	}
	if err := e.seed(ctx); err != nil {
		ovs.Close()
		return nil, err
	}
	return e, nil
}

func (e *Emulator) poke() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

func (e *Emulator) seed(ctx context.Context) error {
	var roots []ovsmodel.OpenvSwitch
	if err := e.ovs.List(ctx, &roots); err != nil {
		return fmt.Errorf("list Open_vSwitch: %w", err)
	}
	if len(roots) == 0 {
		root := &ovsmodel.OpenvSwitch{
			UUID:          "root",
			OVSVersion:    ptr.To(ovsVersion),
			DbVersion:     ptr.To(ovsmodel.Schema().Version),
			DatapathTypes: []string{"netdev", "system"},
			IfaceTypes:    []string{"dpdk", "internal", "patch", "system", "tap", "vxlan", "geneve"},
		}
		ops, err := e.ovs.Create(root)
		if err != nil {
			return err
		}
		if err := e.transact(ctx, ops); err != nil {
			return fmt.Errorf("insert Open_vSwitch root row: %w", err)
		}
	}
	oc := &ovsutils.Client{Client: e.ovs}
	for _, br := range e.cfg.Bridges {
		err := oc.AddBridge(ctx, ovsutils.BridgeConfig{
			Name:         br,
			DatapathType: "netdev",
			FailMode:     ptr.To(string(ovsmodel.BridgeFailModeSecure)),
		})
		if err != nil {
			return fmt.Errorf("seed bridge %s: %w", br, err)
		}
	}
	return nil
}

// Run reconciles on every database change until ctx is done.
func (e *Emulator) Run(ctx context.Context) error {
	defer e.ovs.Close()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-e.trigger:
		case <-timer.C:
		}
		next, err := e.reconcile(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			next = 100 * time.Millisecond
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if next > 0 {
			timer.Reset(next)
		}
	}
}

// reconcile makes one pass over the whole database and writes all changes in one
// transaction. It returns how long until a delayed interface becomes due, or 0.
func (e *Emulator) reconcile(ctx context.Context) (time.Duration, error) {
	var (
		roots   []ovsmodel.OpenvSwitch
		bridges []ovsmodel.Bridge
		ports   []ovsmodel.Port
		ifaces  []ovsmodel.Interface
	)
	for _, l := range []any{&roots, &bridges, &ports, &ifaces} {
		if err := e.ovs.List(ctx, l); err != nil {
			return 0, err
		}
	}
	portByUUID := make(map[string]*ovsmodel.Port, len(ports))
	for i := range ports {
		portByUUID[ports[i].UUID] = &ports[i]
	}
	ifaceByUUID := make(map[string]*ovsmodel.Interface, len(ifaces))
	for i := range ifaces {
		ifaceByUUID[ifaces[i].UUID] = &ifaces[i]
	}

	var (
		next time.Duration
		ops  []ovsdb.Operation
	)
	now := time.Now()
	seen := map[string]bool{}

	names := make([]string, 0, len(bridges))
	for _, brdg := range bridges {
		names = append(names, brdg.Name)
		var members []*ovsmodel.Interface
		used := map[int]bool{}
		for _, pu := range brdg.Ports {
			p, ok := portByUUID[pu]
			if !ok {
				continue
			}
			for _, iu := range p.Interfaces {
				if iface, ok := ifaceByUUID[iu]; ok {
					members = append(members, iface)
					if iface.Ofport != nil && *iface.Ofport > 0 {
						used[*iface.Ofport] = true
					}
				}
			}
		}
		sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })

		for _, iface := range members {
			seen[iface.UUID] = true
			op, wait, err := e.reconcileInterface(iface, brdg.Name, used, now)
			if err != nil {
				return 0, err
			}
			ops = append(ops, op...)
			if wait > 0 && (next == 0 || wait < next) {
				next = wait
			}
		}
	}

	e.Lock()
	for u := range e.firstSeen {
		if !seen[u] {
			delete(e.firstSeen, u)
		}
	}
	e.Unlock()

	// Acknowledge configuration changes the way ovs-vswitchd does. A client that
	// changes the database (ovs-vsctl, libovsdb users) bumps Open_vSwitch.next_cfg
	// in the same transaction and, unless it was told not to wait, blocks until
	// cur_cfg catches up. Copying next_cfg to cur_cfg releases it.
	//
	// The update is appended to ops rather than sent on its own, so it commits
	// together with the ofport and link_state changes of this pass. A client that
	// sees cur_cfg advance therefore also sees the ofport it is waiting for.
	// Interfaces held back by OfportDelay are not part of this pass's ops, so
	// they do not delay the acknowledgement.
	//
	// Only the cur_cfg column is written, so the stale copy of the row cannot
	// overwrite other columns. The write triggers one more pass, which finds
	// cur_cfg == next_cfg and does nothing.
	for i := range roots {
		root := &roots[i]
		if root.CurCfg != root.NextCfg {
			root.CurCfg = root.NextCfg
			op, err := e.ovs.Where(root).Update(root, &root.CurCfg)
			if err != nil {
				return 0, err
			}
			ops = append(ops, op...)
		}
	}

	if len(ops) > 0 {
		if err := e.transact(ctx, ops); err != nil {
			return 0, err
		}
	}

	sort.Strings(names)
	e.Lock()
	changed := !slices.Equal(names, e.bridges)
	e.bridges = names
	e.Unlock()
	if changed && e.onBridges != nil {
		e.onBridges(names)
	}
	return next, nil
}

func (e *Emulator) reconcileInterface(iface *ovsmodel.Interface, bridge string, used map[int]bool, now time.Time) ([]ovsdb.Operation, time.Duration, error) {
	var fields []any

	if e.failed(iface.Name) {
		if iface.Ofport == nil || *iface.Ofport != FailedOfport {
			iface.Ofport = new(FailedOfport)
			iface.Error = new(fmt.Sprintf("could not open network device %s (No such device)", iface.Name))
			iface.LinkState = new(ovsmodel.InterfaceLinkStateDown)
			iface.AdminState = new(ovsmodel.InterfaceAdminStateDown)
			fields = append(fields, &iface.Ofport, &iface.Error, &iface.LinkState, &iface.AdminState)
		}
		return e.update(iface, fields)
	}

	if iface.Ofport == nil || *iface.Ofport == FailedOfport {
		if e.cfg.OfportDelay > 0 {
			e.Lock()
			first, ok := e.firstSeen[iface.UUID]
			if !ok {
				first = now
				e.firstSeen[iface.UUID] = now
			}
			e.Unlock()
			if due := first.Add(e.cfg.OfportDelay); now.Before(due) {
				return nil, due.Sub(now), nil
			}
		}
		ofport := e.allocateOfport(iface, bridge, used)
		used[ofport] = true
		iface.Ofport = new(ofport)
		fields = append(fields, &iface.Ofport)
		if iface.Error != nil {
			// error from previous reconciliation is stale, clear it
			iface.Error = nil
			fields = append(fields, &iface.Error)
		}
	}
	if iface.LinkState == nil || *iface.LinkState != ovsmodel.InterfaceLinkStateUp {
		iface.LinkState = new(ovsmodel.InterfaceLinkStateUp)
		fields = append(fields, &iface.LinkState)
	}
	if iface.AdminState == nil || *iface.AdminState != ovsmodel.InterfaceAdminStateUp {
		iface.AdminState = new(ovsmodel.InterfaceAdminStateUp)
		fields = append(fields, &iface.AdminState)
	}
	return e.update(iface, fields)
}

func (e *Emulator) update(iface *ovsmodel.Interface, fields []any) ([]ovsdb.Operation, time.Duration, error) {
	if len(fields) == 0 {
		return nil, 0, nil
	}
	op, err := e.ovs.Where(iface).Update(iface, fields...)
	return op, 0, err
}

func (e *Emulator) failed(name string) bool {
	for _, p := range e.cfg.FailInterfaces {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// allocateOfport follows ovs-vswitchd: the bridge's own interface gets 65534, a
// free ofport_request is honoured, otherwise the lowest free ofport is used.
func (e *Emulator) allocateOfport(iface *ovsmodel.Interface, bridge string, used map[int]bool) int {
	if iface.Name == bridge {
		return LocalOfport
	}
	if i := slices.Index(e.cfg.PhysicalPorts, iface.Name); i >= 0 && !used[i+1] {
		return i + 1
	}
	if r := iface.OfportRequest; r != nil && *r >= 1 && *r <= MaxOfport && !used[*r] {
		return *r
	}
	for p := 1; p <= MaxOfport; p++ {
		if used[p] {
			continue
		}
		if i := slices.Index(e.cfg.PhysicalPorts, iface.Name); i < 0 && p <= len(e.cfg.PhysicalPorts) {
			continue
		}
		return p
	}
	return FailedOfport
}

func (e *Emulator) transact(ctx context.Context, ops []ovsdb.Operation) error {
	res, err := e.ovs.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	if _, err := ovsdb.CheckOperationResults(res, ops); err != nil {
		return errors.Join(err, fmt.Errorf("transaction failed: %v", res))
	}
	return nil
}
