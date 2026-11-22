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

// mock-ovs replaces ovsdb-server and ovs-vswitchd on a SimFleet VM.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/NVIDIA/doca-platform-scale-sim/pkg/mock-ovs/mockovs"

	"github.com/go-logr/stdr"
)

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	runDir := flag.String("run-dir", "/var/run/openvswitch", "Directory for db.sock, <bridge>.mgmt and pidfiles.")
	bridges := flag.String("bridges", "br-sfc", "Comma-separated bridges to create at startup. br-sfc is always created.")
	physicalPorts := flag.String("physical-ports", "p0,p1", "Comma-separated uplink names that get ofports 1..N.")
	ofportDelay := flag.Duration("ofport-delay", 0, "Delay before a new interface gets an ofport and link_state up.")
	failInterfaces := flag.String("fail-interfaces", "", "Comma-separated glob patterns; matching interfaces get ofport -1 and an error.")
	verbose := flag.Bool("v", false, "Verbose logging, including libovsdb server logs.")
	flag.Parse()

	if !*verbose {
		stdr.SetVerbosity(0)
	}
	logger := stdr.New(log.New(os.Stderr, "", log.LstdFlags)).WithName("mock-ovs")

	brs := splitList(*bridges)
	if !strings.Contains(","+*bridges+",", ",br-sfc,") {
		brs = append([]string{"br-sfc"}, brs...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := mockovs.Run(ctx, mockovs.Config{
		RunDir:         *runDir,
		Bridges:        brs,
		PhysicalPorts:  splitList(*physicalPorts),
		OfportDelay:    *ofportDelay,
		FailInterfaces: splitList(*failInterfaces),
		Log:            logger,
	})
	if err != nil {
		logger.Error(err, "mock-ovs failed")
		os.Exit(1)
	}
}
