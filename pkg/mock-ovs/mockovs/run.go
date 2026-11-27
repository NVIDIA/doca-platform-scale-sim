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

// Package mockovs wires the OVSDB server, the vswitchd emulator and the
// OpenFlow switches into one process.
package mockovs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/NVIDIA/doca-platform-scale-sim/pkg/mock-ovs/ofswitch"
	"github.com/NVIDIA/doca-platform-scale-sim/pkg/mock-ovs/ovsdbserver"
	"github.com/NVIDIA/doca-platform-scale-sim/pkg/mock-ovs/vswitchd"

	"github.com/go-logr/logr"
)

const (
	// DBSocket is the OVSDB socket name in the run dir.
	DBSocket = "db.sock"
	// VswitchdPidFile and OvsdbPidFile are written so sfc-controller's pidfile watcher works.
	VswitchdPidFile = "ovs-vswitchd.pid"
	OvsdbPidFile    = "ovsdb-server.pid"
)

// Config configures a mock-ovs process.
type Config struct {
	RunDir         string
	Bridges        []string
	PhysicalPorts  []string
	OfportDelay    time.Duration
	FailInterfaces []string
	Log            logr.Logger
	// Ready, if set, is called once the sockets and pidfiles exist.
	Ready func(*Instance)
}

// Instance exposes the running parts, mainly for tests.
type Instance struct {
	Server   *ovsdbserver.Server
	Switches *ofswitch.Manager
}

// Run serves until ctx is done, then removes sockets and pidfiles.
func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv, err := ovsdbserver.Start(ctx, filepath.Join(cfg.RunDir, DBSocket))
	if err != nil {
		return err
	}
	defer srv.Close()

	switches := ofswitch.NewManager(cfg.RunDir, cfg.Log)
	defer switches.Close()

	emu, err := vswitchd.New(ctx, vswitchd.Config{
		Endpoint:       srv.Endpoint(),
		Bridges:        cfg.Bridges,
		PhysicalPorts:  cfg.PhysicalPorts,
		OfportDelay:    cfg.OfportDelay,
		FailInterfaces: cfg.FailInterfaces,
	}, switches.Sync)
	if err != nil {
		return err
	}
	switches.Sync(cfg.Bridges)

	pid := []byte(strconv.Itoa(os.Getpid()) + "\n")
	for _, f := range []string{OvsdbPidFile, VswitchdPidFile} {
		p := filepath.Join(cfg.RunDir, f)
		if err := os.WriteFile(p, pid, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		defer func() { _ = os.Remove(p) }()
	}
	cfg.Log.Info("mock-ovs ready", "runDir", cfg.RunDir, "bridges", cfg.Bridges)
	if cfg.Ready != nil {
		cfg.Ready(&Instance{Server: srv, Switches: switches})
	}
	return emu.Run(ctx)
}
