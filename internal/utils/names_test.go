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

package utils

import (
	"regexp"
	"testing"
)

func TestDeviceName(t *testing.T) {
	if got := DeviceName("scale-3000", 0); got != "scale-3000-device-000000" {
		t.Fatalf("got %q", got)
	}
}

func TestInstanceName(t *testing.T) {
	tests := []struct {
		fleet   string
		ordinal int
		want    string
	}{
		{fleet: "fleet-a", ordinal: 0, want: "fleet-a-000000"},
		{fleet: "fleet-a", ordinal: 123, want: "fleet-a-000123"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := InstanceName(tt.fleet, tt.ordinal); got != tt.want {
				t.Fatalf("InstanceName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNodeName(t *testing.T) {
	if got := NodeName("scale-3000", 0); got != "scale-3000-host-0000" {
		t.Fatalf("got %q", got)
	}
}

func TestSerialNumber(t *testing.T) {
	if got := SerialNumber("scale-3000", 0); got != "MT5799000000" {
		t.Fatalf("got %q", got)
	}
	if got := SerialNumber("scale-3000", 2999); got != "MT5799002999" {
		t.Fatalf("got %q", got)
	}
	if SerialNumber("scale-3000", 0) == SerialNumber("other-fleet", 0) {
		t.Fatal("two fleets minted the same serial for ordinal 0")
	}
	mockBMC := regexp.MustCompile(`^MT[0-9]{4}[A-Z0-9-]{6}$`)
	for _, serial := range []string{
		SerialNumber("scale-3000", 0),
		SerialNumber("e2e-fleet", 1),
		SerialNumber("passthrough-fleet", 12),
	} {
		if !mockBMC.MatchString(serial) {
			t.Fatalf("%q is not a 12-character mock BMC serial", serial)
		}
	}
}

func TestVirtualMachineName(t *testing.T) {
	if got := VirtualMachineName("scale-3000", 0); got != "scale-3000-dpu-000000" {
		t.Fatalf("got %q", got)
	}
}

func TestCloudInitSecretName(t *testing.T) {
	if got := CloudInitSecretName("scale-3000", 0); got != "scale-3000-dpu-000000-cloud-init" {
		t.Fatalf("got %q", got)
	}
}

func TestKubeadmJoinSecretName(t *testing.T) {
	if got := KubeadmJoinSecretName("scale-3000-dpu-000000"); got != "scale-3000-dpu-000000-kubeadm-join" {
		t.Fatalf("got %q", got)
	}
}

func TestDPUName(t *testing.T) {
	tests := []struct {
		name              string
		fleet             string
		devicesPerDPUNode int
		ordinal           int
		want              string
	}{
		{
			name:              "first ordinal of the first host",
			fleet:             "e2e-fleet",
			devicesPerDPUNode: 1,
			ordinal:           0,
			want:              "e2e-fleet-host-0000-e2e-fleet-device-000000",
		},
		{
			name:              "second host group",
			fleet:             "scale-3000",
			devicesPerDPUNode: 10,
			ordinal:           12,
			want:              "scale-3000-host-0001-scale-3000-device-000012",
		},
		{
			name:              "unusable host grouping still names a DPU",
			fleet:             "scale-3000",
			devicesPerDPUNode: 0,
			ordinal:           12,
			want:              "scale-3000-host-0000-scale-3000-device-000012",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DPUName(tt.fleet, tt.devicesPerDPUNode, tt.ordinal)
			if got != tt.want {
				t.Fatalf("DPUName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDPUNameForHost(t *testing.T) {
	if got, want := DPUNameForHost("scale-3000", 1, 12), "scale-3000-host-0001-scale-3000-device-000012"; got != want {
		t.Fatalf("DPUNameForHost() = %q, want %q", got, want)
	}
	// The host index is taken as given, so a name recorded under an
	// earlier devicesPerDPUNode still resolves to the DPU DPF created.
	if got, want := DPUNameForHost("scale-3000", 3, 12), "scale-3000-host-0003-scale-3000-device-000012"; got != want {
		t.Fatalf("DPUNameForHost() = %q, want %q", got, want)
	}
	if got := DPUName("scale-3000", 10, 12); got != DPUNameForHost("scale-3000", 1, 12) {
		t.Fatalf("DPUName() = %q, want DPUNameForHost of host 1", got)
	}
}

// A DPU name is a Kubernetes object name and the tenant Node name kubelet
// registers, so the widest fleet this controller supports must still fit in
// the 63-character DNS label limit.
func TestDPUNameFitsADNSLabel(t *testing.T) {
	name := DPUName("scale-3000", 10, 2999)
	if len(name) > 63 {
		t.Fatalf("DPUName() = %q is %d characters, want at most 63", name, len(name))
	}
}

func TestOrdinalFromDeviceName(t *testing.T) {
	ord, ok := OrdinalFromDeviceName("scale-3000", "scale-3000-device-000012")
	if !ok || ord != 12 {
		t.Fatalf("got %d %v", ord, ok)
	}
	if _, ok := OrdinalFromDeviceName("scale-3000", "other-device-000000"); ok {
		t.Fatal("expected not ok")
	}
	if _, ok := OrdinalFromDeviceName("scale-3000", "scale-3000-device-abc"); ok {
		t.Fatal("expected not ok")
	}
}

func TestHostIndexAndNodeCount(t *testing.T) {
	if HostIndex(0, 10) != 0 || HostIndex(9, 10) != 0 || HostIndex(10, 10) != 1 {
		t.Fatal("HostIndex")
	}
	if DesiredNodeCount(0, 10) != 0 || DesiredNodeCount(20, 10) != 2 || DesiredNodeCount(21, 10) != 3 {
		t.Fatal("DesiredNodeCount")
	}
}

// A CRD minimum keeps devicesPerDPUNode >= 1, but callers reach HostIndex with
// values read from unvalidated objects, so it must not divide by zero.
func TestHostIndexGuardsNonPositiveDevicesPerDPUNode(t *testing.T) {
	if got := HostIndex(1234, 0); got != 0 {
		t.Fatalf("HostIndex(1234, 0) = %d, want 0", got)
	}
	if got := HostIndex(1234, -5); got != 0 {
		t.Fatalf("HostIndex(1234, -5) = %d, want 0", got)
	}
}

func TestHostOrdinalRange(t *testing.T) {
	start, end := HostOrdinalRange(0, 10, 20)
	if start != 0 || end != 10 {
		t.Fatalf("host 0: got [%d, %d)", start, end)
	}
	start, end = HostOrdinalRange(1, 10, 15)
	if start != 10 || end != 15 {
		t.Fatalf("partial last host: got [%d, %d)", start, end)
	}
}
