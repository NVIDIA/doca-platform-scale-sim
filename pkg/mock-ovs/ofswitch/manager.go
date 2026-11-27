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

package ofswitch

import (
	"path/filepath"
	"sync"

	"github.com/go-logr/logr"
)

// Manager runs one Switch per bridge in runDir.
type Manager struct {
	runDir string
	log    logr.Logger
	sync.Mutex
	switches map[string]*Switch
}

// NewManager returns a Manager serving <runDir>/<bridge>.mgmt sockets.
func NewManager(runDir string, log logr.Logger) *Manager {
	return &Manager{runDir: runDir, log: log, switches: map[string]*Switch{}}
}

// Sync starts switches for new bridges and stops switches for removed ones.
func (m *Manager) Sync(names []string) {
	m.Lock()
	defer m.Unlock()
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
		if _, ok := m.switches[n]; ok {
			continue
		}
		sw, err := NewSwitch(n, filepath.Join(m.runDir, n+".mgmt"), m.log)
		if err != nil {
			m.log.Error(err, "failed to start openflow switch", "bridge", n)
			continue
		}
		m.switches[n] = sw
	}
	for n, sw := range m.switches {
		if !want[n] {
			sw.Close()
			delete(m.switches, n)
		}
	}
}

// Switch returns the switch for bridge, or nil.
func (m *Manager) Switch(bridge string) *Switch {
	m.Lock()
	defer m.Unlock()
	return m.switches[bridge]
}

// Close stops all switches.
func (m *Manager) Close() {
	m.Sync(nil)
}
