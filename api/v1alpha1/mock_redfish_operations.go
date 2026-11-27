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

package v1alpha1

// MockRedfishOperations is the route table of the mock-dpuagent pinned by
// hack/dpuagent/mock.ref: each operation name that
// spec.dpuAgent.bmc.responseDelayOverrides may name, and the HTTP methods it
// serves. The mock refuses to start on any other name or method, so the name
// enum and the methods CEL rule on DPUAgentResponseDelayOverride must list
// exactly this. CRD markers cannot be computed, and the mock keeps its table
// unexported, so it is spelled out here; tests hold it to the mock's source
// and to what the mock's own server accepts.
var MockRedfishOperations = map[string][]string{
	"Account":                               {"PATCH"},
	"AccountService":                        {"PATCH"},
	"BMCRshim":                              {"GET", "PATCH"},
	"Bios":                                  {"GET"},
	"BiosSettings":                          {"PATCH"},
	"CertificateService.GenerateCSR":        {"POST"},
	"CertificateService.ReplaceCertificate": {"POST"},
	"Chassis":                               {"GET"},
	"ComputerSystem.Reset":                  {"POST"},
	"Drive":                                 {"GET"},
	"FirmwareInventory":                     {"GET"},
	"HostPrivilegeConfig":                   {"PATCH"},
	"HostRshim.Set":                         {"POST"},
	"Manager":                               {"GET"},
	"Manager.Reset":                         {"POST"},
	"Manager.ResetToDefaults":               {"POST"},
	"Managers":                              {"GET"},
	"Mode.Set":                              {"POST"},
	"NetworkAdapter":                        {"GET"},
	"NetworkDeviceFunction":                 {"GET"},
	"NetworkPort":                           {"GET"},
	"NvidiaChassis.AuxPowerReset":           {"POST"},
	"NvidiaChassis.Reset":                   {"POST"},
	"NvidiaUpdateService.CommitImage":       {"POST"},
	"Processor":                             {"GET"},
	"SELEntries":                            {"GET"},
	"SOC.ForceReset":                        {"POST"},
	"SecureBoot":                            {"GET", "PATCH"},
	"ServerCertificate":                     {"GET"},
	"ServiceRoot":                           {"GET"},
	"System":                                {"GET"},
	"SystemOem":                             {"GET"},
	"SystemSettings":                        {"GET", "PATCH"},
	"Systems":                               {"GET"},
	"Task":                                  {"GET"},
	"TruststoreCertificates":                {"DELETE", "GET", "POST"},
	"UpdateMultipart":                       {"POST"},
	"UpdateService":                         {"GET", "POST"},
	"UpdateService.Activate":                {"POST"},
	"UpdateService.SimpleUpdate":            {"POST"},
	"VirtualMedia":                          {"GET", "POST"},
}
