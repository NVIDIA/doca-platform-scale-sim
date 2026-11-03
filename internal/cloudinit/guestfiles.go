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

package cloudinit

import (
	"errors"
	"fmt"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// The file names of the guest files. A pod guest Secret uses them as its
// keys; a VirtualMachine's cloud-init userdata writes them to GuestFile.Path.
const (
	IdentityFileName   = "identity.env"
	ConfigFileName     = "config.yaml"
	KubeconfigFileName = "mgmt.kubeconfig"
)

// GuestFile is one file a simulated DPU's guest reads at Path. Both runtimes
// deliver the same bodies: a VirtualMachine through cloud-init write_files, a
// Pod through a Secret.
type GuestFile struct {
	// Name is the file's name, and the key of a Secret that holds it.
	Name string
	// Path is where the guest reads the file.
	Path    string
	Content string
}

// GuestFiles renders the files a guest for ordinal needs: always the
// identity, and with the DPU agent enabled also its configuration and the
// management kubeconfig. It is pure: callers resolve and validate every
// dependency first. managementKubeconfig is ignored while the agent is
// disabled.
func GuestFiles(
	fleet *scalev1alpha1.SimFleet,
	ordinal int,
	managementKubeconfig []byte,
) ([]GuestFile, error) {
	if fleet == nil {
		return nil, errors.New("SimFleet is required to render the guest files")
	}
	files := []GuestFile{{
		Name:    IdentityFileName,
		Path:    dpuIdentityPath,
		Content: dpuIdentityContent(fleet, ordinal),
	}}
	if !dpuagent.Enabled(fleet) {
		return files, nil
	}
	if len(managementKubeconfig) == 0 {
		return nil, fmt.Errorf("management kubeconfig for ordinal %d is empty", ordinal)
	}
	conf, err := dpuagent.RenderConfig(fleet, naming.SerialNumber(fleet.Name, ordinal))
	if err != nil {
		return nil, fmt.Errorf("render dpuagent configuration for ordinal %d: %w", ordinal, err)
	}
	return append(files,
		GuestFile{Name: ConfigFileName, Path: dpuagent.ConfPath, Content: conf},
		GuestFile{Name: KubeconfigFileName, Path: dpuagent.KubeconfigPath, Content: string(managementKubeconfig)},
	), nil
}
