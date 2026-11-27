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
	"slices"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/dpuagent"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// The management kubeconfig shape dpuagent.ManagementKubeconfig must produce.
const (
	managementConfigName = "management"
	managementServiceURL = "https://kubernetes.default.svc"
)

func fleetWith(agent *scalev1alpha1.DPUAgentSpec) *scalev1alpha1.SimFleet {
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "test-ns"},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              1,
			DevicesPerDPUNode:     1,
			DeviceSelectionLabels: map[string]string{"pool": "scale-3000"},
		},
	}
	if agent != nil {
		fleet.Spec.DPUAgent = agent
	}
	return fleet
}

type cloudConfig struct {
	Hostname   string           `json:"hostname"`
	SSHPwauth  bool             `json:"ssh_pwauth"`
	Chpasswd   cloudChpasswd    `json:"chpasswd"`
	Users      []cloudUser      `json:"users"`
	WriteFiles []cloudWriteFile `json:"write_files"`
	Runcmd     []string         `json:"runcmd"`
}

type cloudChpasswd struct {
	Expire bool   `json:"expire"`
	List   string `json:"list"`
}

type cloudUser struct {
	Name       string `json:"name"`
	LockPasswd *bool  `json:"lock_passwd"`
	Sudo       string `json:"sudo"`
	Groups     string `json:"groups"`
	Shell      string `json:"shell"`
}

type cloudWriteFile struct {
	Path        string `json:"path"`
	Permissions string `json:"permissions"`
	Content     string `json:"content"`
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertCloudConfigTopLevelKeys(userdata string) {
	GinkgoHelper()

	var raw map[string]any
	Expect(yaml.Unmarshal([]byte(userdata), &raw)).To(Succeed(), "parse userdata as map:\n%s", userdata)
	// runcmd is absent for an enabled guest and present only to mask units
	// for a disabled one, so it is optional rather than expected.
	want := []string{"chpasswd", "hostname", "ssh_pwauth", "users", "write_files"}
	got := slices.DeleteFunc(mapKeys(raw), func(key string) bool { return key == "runcmd" })
	Expect(got).To(Equal(want), "top-level keys, plus an optional runcmd")
}

func secretDataKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertForbiddenCloudInitContent(userdata string) {
	GinkgoHelper()
	Expect(validateForbiddenCloudInitContent(userdata)).To(Succeed())
}

func validateForbiddenCloudInitContent(userdata string) error {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(userdata), &raw); err != nil {
		return errors.New("cloud-init userdata is not valid YAML")
	}
	for _, key := range []string{
		"packages", "package_update", "package_upgrade", "apt", "yum", "apk",
	} {
		if _, found := raw[key]; found {
			return fmt.Errorf("cloud-init userdata contains forbidden key %q", key)
		}
	}

	var cloud cloudConfig
	if err := yaml.Unmarshal([]byte(userdata), &cloud); err != nil {
		return errors.New("cloud-init userdata has an invalid shape")
	}
	for _, command := range cloud.Runcmd {
		for _, fragment := range []string{
			"kubeadm join",
			"kubeadm init",
			"systemctl start kubelet",
			"systemctl enable kubelet",
			"kubelet.service",
			"snap install",
			"dnf install",
		} {
			if strings.Contains(command, fragment) {
				return fmt.Errorf("cloud-init command contains forbidden operation %q", fragment)
			}
		}
	}

	foundConf := false
	foundManagementKubeconfig := false
	for _, file := range cloud.WriteFiles {
		if strings.HasSuffix(file.Path, "admin.conf") ||
			strings.Contains(file.Path, "super-admin.conf") {
			return errors.New("cloud-init writes a tenant admin kubeconfig")
		}
		switch file.Path {
		case dpuagent.ConfPath:
			foundConf = true
		case dpuagent.KubeconfigPath:
			if foundManagementKubeconfig {
				return errors.New("cloud-init writes more than one management kubeconfig")
			}
			foundManagementKubeconfig = true
			if err := validateLimitedManagementKubeconfig([]byte(file.Content)); err != nil {
				return err
			}
			continue
		}
		for _, marker := range []string{
			"kubeadm join",
			"client-certificate-data:",
			"client-key-data:",
			"BEGIN PRIVATE KEY",
			"BEGIN RSA PRIVATE KEY",
			"join-discovery.env",
			"BOOTSTRAP_TOKEN=",
			"CA_CERT_HASHES=",
			"API_SERVER=",
			"tenant-ca.crt",
		} {
			if strings.Contains(file.Content, marker) {
				return fmt.Errorf("cloud-init file %q contains forbidden bootstrap material", file.Path)
			}
		}
		if strings.Contains(file.Content, "kind: Config") {
			return fmt.Errorf("cloud-init file %q contains an unapproved kubeconfig", file.Path)
		}
	}
	if foundConf != foundManagementKubeconfig {
		return errors.New("cloud-init dpuagent configuration and management kubeconfig must be written together")
	}
	return nil
}

func validateLimitedManagementKubeconfig(data []byte) error {
	config, err := clientcmd.Load(data)
	if err != nil {
		return errors.New("management kubeconfig is invalid")
	}
	if len(config.Clusters) != 1 || len(config.AuthInfos) != 1 ||
		len(config.Contexts) != 1 || config.CurrentContext != managementConfigName {
		return errors.New("management kubeconfig is not limited to one named cluster, user, and context")
	}

	cluster := config.Clusters[managementConfigName]
	if cluster == nil || cluster.Server != managementServiceURL ||
		len(cluster.CertificateAuthorityData) == 0 {
		return errors.New("management kubeconfig does not use the pinned management Service and CA")
	}
	if cluster.TLSServerName != "" || cluster.InsecureSkipTLSVerify ||
		cluster.CertificateAuthority != "" || cluster.ProxyURL != "" ||
		cluster.DisableCompression || len(cluster.Extensions) != 0 {
		return errors.New("management kubeconfig cluster contains unapproved settings")
	}

	auth := config.AuthInfos[managementConfigName]
	if auth == nil || auth.Token == "" {
		return errors.New("management kubeconfig does not use bearer-token authentication")
	}
	if auth.ClientCertificate != "" || len(auth.ClientCertificateData) != 0 ||
		auth.ClientKey != "" || len(auth.ClientKeyData) != 0 ||
		auth.TokenFile != "" || auth.Impersonate != "" || auth.ImpersonateUID != "" ||
		len(auth.ImpersonateGroups) != 0 || len(auth.ImpersonateUserExtra) != 0 ||
		auth.Username != "" || auth.Password != "" || auth.AuthProvider != nil ||
		auth.Exec != nil || len(auth.Extensions) != 0 {
		return errors.New("management kubeconfig contains authentication material other than its bearer token")
	}

	context := config.Contexts[managementConfigName]
	if context == nil || context.Cluster != managementConfigName || context.AuthInfo != managementConfigName {
		return errors.New("management kubeconfig context is not pinned to the management cluster and user")
	}
	if context.Namespace != "" || len(context.Extensions) != 0 {
		return errors.New("management kubeconfig context contains unapproved settings")
	}
	return nil
}

func assertCloudInitDebugUser(cfg cloudConfig) {
	GinkgoHelper()
	Expect(cfg.SSHPwauth).To(BeTrue(), "ssh_pwauth")
	Expect(cfg.Chpasswd.Expire).To(BeFalse(), "chpasswd.expire")
	wantPasswd := cloudInitDebugUser + ":" + cloudInitDebugPassword
	Expect(strings.TrimSpace(cfg.Chpasswd.List)).To(Equal(wantPasswd), "chpasswd.list")
	Expect(cfg.Users).To(HaveLen(1))
	user := cfg.Users[0]
	Expect(user.Name).To(Equal(cloudInitDebugUser), "user name")
	Expect(user.LockPasswd).To(HaveValue(BeFalse()), "lock_passwd")
	Expect(user.Sudo).To(Equal("ALL=(ALL) NOPASSWD:ALL"), "sudo")
	Expect(user.Groups).To(Equal("sudo"), "groups")
	Expect(user.Shell).To(Equal("/bin/bash"), "shell")
}

var _ = Describe("Cloud-init Secret", func() {
	Describe("DesiredSecret", func() {
		It("renders the identity, dpuagent configuration and management kubeconfig for an enabled guest", func() {
			fleet := &scalev1alpha1.SimFleet{
				ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "fleet-ns", UID: "uid"},
				Spec: scalev1alpha1.SimFleetSpec{
					Replicas:          3000,
					DevicesPerDPUNode: 10,
				},
			}
			const ordinal = 12
			kubeconfig, err := dpuagent.ManagementKubeconfig(
				[]byte("test-management-token"),
				[]byte("test-management-ca"),
			)
			Expect(err).NotTo(HaveOccurred())
			secret, err := DesiredSecret(fleet, ordinal, kubeconfig)
			Expect(err).NotTo(HaveOccurred())
			Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
			Expect(secretDataKeys(secret.Data)).To(Equal([]string{"userdata"}))
			ud := string(secret.Data["userdata"])
			assertForbiddenCloudInitContent(ud)
			assertCloudConfigTopLevelKeys(ud)

			var cfg cloudConfig
			Expect(yaml.Unmarshal([]byte(ud), &cfg)).To(Succeed(), "parse userdata:\n%s", ud)
			Expect(cfg.Hostname).To(Equal("scale-3000-dpu-000012"))
			assertCloudInitDebugUser(cfg)
			Expect(cfg.WriteFiles).To(HaveLen(3))
			identity := cfg.WriteFiles[0]
			Expect(identity.Path).To(Equal(dpuIdentityPath))
			Expect(identity.Permissions).To(Equal("0600"))
			// Every line is derived from the fleet and the ordinal. Nothing here can
			// come from a DPU object, because none exists when this Secret is written.
			wantIdentity := strings.Join([]string{
				"DPU_NAME=scale-3000-host-0001-scale-3000-device-000012",
				"DPU_NAMESPACE=fleet-ns",
				"DPU_NODE_NAME=scale-3000-host-0001",
				"DPU_DEVICE_NAME=scale-3000-device-000012",
			}, "\n") + "\n"
			Expect(identity.Content).To(Equal(wantIdentity))
			conf := cfg.WriteFiles[1]
			Expect(conf.Path).To(Equal(dpuagent.ConfPath))
			Expect(conf.Permissions).To(Equal("0600"))
			wantConf, err := dpuagent.RenderConfig(fleet, naming.SerialNumber(fleet.Name, ordinal))
			Expect(err).NotTo(HaveOccurred())
			Expect(conf.Content).To(Equal(wantConf))
			management := cfg.WriteFiles[2]
			Expect(management.Path).To(Equal(dpuagent.KubeconfigPath))
			Expect(management.Permissions).To(Equal("0600"))
			Expect(management.Content).To(Equal(string(kubeconfig)))
			// Both units are baked into the image and enabled there; an enabled
			// guest must neither run commands nor name a unit it does not own.
			Expect(cfg.Runcmd).To(BeEmpty())
			for _, unit := range []string{dpuagent.WaitKubeletJoinUnit, dpuagent.MockDPUAgentUnit} {
				Expect(ud).NotTo(ContainSubstring(unit), "enabled cloud-init userdata mentions %q", unit)
			}
			Expect(secret.Labels).To(HaveKeyWithValue(naming.LabelOrdinal, "000012"))
			Expect(secret.Labels).To(HaveKeyWithValue(naming.LabelFleetUID, "uid"))
			Expect(len(secret.Data["userdata"])).To(BeNumerically("<", maxCloudInitUserdataBytes))
		})

		It("rejects a missing SimFleet or an empty management kubeconfig", func() {
			_, err := cloudInitUserdata(nil, 0, []byte("kubeconfig"))
			Expect(err).To(HaveOccurred(), "cloudInitUserdata accepted a nil SimFleet")
			for _, kubeconfig := range [][]byte{nil, {}} {
				secret, err := DesiredSecret(fleetWith(nil), 3, kubeconfig)
				Expect(err).To(HaveOccurred(), "DesiredSecret accepted an empty management kubeconfig %#v", kubeconfig)
				Expect(secret).To(BeNil(), "DesiredSecret accepted an empty management kubeconfig %#v", kubeconfig)
				Expect(err.Error()).To(And(ContainSubstring("ordinal 3"), ContainSubstring("empty")),
					"error does not name the empty kubeconfig and ordinal")
			}
		})

		It("omits agent dependencies and persistently masks both units when the agent is disabled", func() {
			disabled := false
			fleet := fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &disabled})
			fleet.UID = "fleet-uid"
			secret, err := DesiredSecret(fleet, 0, nil)
			Expect(err).NotTo(HaveOccurred())
			userdata := string(secret.Data["userdata"])
			assertForbiddenCloudInitContent(userdata)
			assertCloudConfigTopLevelKeys(userdata)

			var cfg cloudConfig
			Expect(yaml.Unmarshal([]byte(userdata), &cfg)).To(Succeed(), "parse userdata:\n%s", userdata)
			Expect(cfg.WriteFiles).To(HaveLen(1))
			Expect(cfg.WriteFiles[0].Path).To(Equal(dpuIdentityPath))
			// Without a credential the join helper would fail on a loop, so both
			// baked units must be masked, and persistently so (see cloudInitUserdata).
			for _, unit := range []string{dpuagent.MockDPUAgentUnit, dpuagent.WaitKubeletJoinUnit} {
				Expect(cfg.Runcmd).To(ContainElement("systemctl mask --now "+unit),
					"disabled runcmd does not persistently mask %s", unit)
			}
			Expect(cfg.Runcmd).NotTo(ContainElement(And(ContainSubstring("systemctl mask"), ContainSubstring("--runtime"))),
				"runcmd masks a unit only for this boot")
			assertCloudInitDebugUser(cfg)
		})

		It("rejects oversized userdata without leaking credentials", func() {
			const credentialMarker = "do-not-leak-management-credential"
			kubeconfig := []byte(strings.Repeat(credentialMarker, maxCloudInitUserdataBytes))
			secret, err := DesiredSecret(fleetWith(nil), 7, kubeconfig)
			Expect(err).To(HaveOccurred())
			Expect(secret).To(BeNil(), "DesiredSecret returned a Secret for oversized userdata")
			Expect(err.Error()).To(ContainSubstring("ordinal 7"), "error does not identify the ordinal")
			Expect(strings.Contains(err.Error(), credentialMarker)).To(BeFalse(), "error leaked credential material")
		})

		It("embeds a management kubeconfig limited to the pinned Service, CA and bearer token", func() {
			const testToken = "do-not-log-management-token"
			testCA := []byte("test-management-ca")
			kubeconfig, err := dpuagent.ManagementKubeconfig([]byte(testToken), testCA)
			Expect(err).NotTo(HaveOccurred())
			secret, err := DesiredSecret(fleetWith(nil), 0, kubeconfig)
			Expect(err).NotTo(HaveOccurred())

			var cloud cloudConfig
			Expect(yaml.Unmarshal(secret.Data["userdata"], &cloud)).To(Succeed())
			var embedded string
			for _, file := range cloud.WriteFiles {
				if file.Path == dpuagent.KubeconfigPath {
					embedded = file.Content
				}
			}
			Expect(embedded).NotTo(BeEmpty(), "management kubeconfig is absent")
			config, err := clientcmd.Load([]byte(embedded))
			Expect(err).NotTo(HaveOccurred())
			Expect(config.Clusters).To(HaveLen(1))
			Expect(config.AuthInfos).To(HaveLen(1))
			Expect(config.Contexts).To(HaveLen(1))
			cluster := config.Clusters[managementConfigName]
			Expect(cluster).NotTo(BeNil())
			Expect(cluster.Server).To(Equal(managementServiceURL))
			Expect(cluster.CertificateAuthorityData).To(Equal(testCA))
			Expect(cluster.InsecureSkipTLSVerify).To(BeFalse())
			auth := config.AuthInfos[managementConfigName]
			Expect(auth).NotTo(BeNil())
			Expect(auth.Token == testToken).To(BeTrue(),
				"management kubeconfig does not contain the expected bearer-token authentication")
			Expect(auth.ClientCertificateData).To(BeEmpty())
			Expect(auth.ClientKeyData).To(BeEmpty())
			Expect(auth.Username).To(BeEmpty())
			Expect(auth.Password).To(BeEmpty())
			Expect(auth.Exec).To(BeNil())
			Expect(auth.AuthProvider).To(BeNil())
			assertForbiddenCloudInitContent(string(secret.Data["userdata"]))
		})

		// The largest spec.dpuAgent.bmc the CRD admits must still fit a cloud-init
		// Secret next to a realistically sized management credential. The renderer
		// does not validate overrides, so the longest operation name repeats 32 times with four
		// methods here even though admission would refuse that list; only the bytes
		// matter.
		It("fits the largest admitted BMC settings into cloud-init", func() {
			longest := strings.Repeat("x", 64)
			overrides := make([]scalev1alpha1.DPUAgentResponseDelayOverride, 32)
			for i := range overrides {
				overrides[i] = scalev1alpha1.DPUAgentResponseDelayOverride{
					Name:       "CertificateService.ReplaceCertificate",
					Methods:    []string{"DELETE", "GET", "PATCH", "POST"},
					MinSeconds: 600,
					MaxSeconds: 600,
				}
			}
			fleet := &scalev1alpha1.SimFleet{
				ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "fleet-ns", UID: "uid"},
				Spec: scalev1alpha1.SimFleetSpec{
					Replicas:          3000,
					DevicesPerDPUNode: 10,
					DPUAgent: &scalev1alpha1.DPUAgentSpec{
						DPUType:      ptr.To("bf4"),
						RebootMethod: ptr.To("SLR,PowerCycle,NoAction"),
						BMC: &scalev1alpha1.DPUAgentBMCSpec{
							PSID: ptr.To(strings.Repeat("M", 32)),
							Firmware: &scalev1alpha1.DPUAgentFirmware{
								BMC: &longest, ERoT: &longest, UEFI: &longest, NIC: &longest,
							},
							ResponseDelay:          &scalev1alpha1.DPUAgentResponseDelay{MinSeconds: 600, MaxSeconds: 600},
							ResponseDelayOverrides: overrides,
						},
					},
				},
			}
			kubeconfig, err := dpuagent.ManagementKubeconfig(
				[]byte(strings.Repeat("t", 1400)),
				[]byte(strings.Repeat("c", 1900)),
			)
			Expect(err).NotTo(HaveOccurred())
			secret, err := DesiredSecret(fleet, 2999, kubeconfig)
			Expect(err).NotTo(HaveOccurred())
			userdata := string(secret.Data["userdata"])
			Expect(strings.Count(userdata, "name: CertificateService.ReplaceCertificate")).To(Equal(len(overrides)),
				"rendered overrides")
			Expect(len(userdata)).To(BeNumerically("<", maxCloudInitUserdataBytes), "userdata size")
		})
	})

	Describe("validateForbiddenCloudInitContent", func() {
		const testToken = "do-not-log-tenant-token"

		makeUserdata := func(mutate func(*cloudConfig)) string {
			GinkgoHelper()
			kubeconfig, err := dpuagent.ManagementKubeconfig([]byte(testToken), []byte("test-ca"))
			Expect(err).NotTo(HaveOccurred())
			secret, err := DesiredSecret(fleetWith(nil), 0, kubeconfig)
			Expect(err).NotTo(HaveOccurred())
			var cloud cloudConfig
			Expect(yaml.Unmarshal(secret.Data["userdata"], &cloud)).To(Succeed())
			mutate(&cloud)
			data, err := yaml.Marshal(cloud)
			Expect(err).NotTo(HaveOccurred())
			return "#cloud-config\n" + string(data)
		}

		fileAt := func(cloud *cloudConfig, path string) *cloudWriteFile {
			GinkgoHelper()
			for i := range cloud.WriteFiles {
				if cloud.WriteFiles[i].Path == path {
					return &cloud.WriteFiles[i]
				}
			}
			Fail("cloud-config writes no " + path)
			return nil
		}

		DescribeTable("rejects foreign and tenant kubeconfigs",
			func(mutate func(*cloudConfig), wantErr string) {
				err := validateForbiddenCloudInitContent(makeUserdata(mutate))
				if wantErr == "" {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
				Expect(strings.Contains(err.Error(), testToken)).To(BeFalse(), "validation error leaked kubeconfig token")
			},
			Entry("unmodified round trip", func(*cloudConfig) {}, ""),
			Entry("foreign management server", func(cloud *cloudConfig) {
				management := fileAt(cloud, dpuagent.KubeconfigPath)
				config, err := clientcmd.Load([]byte(management.Content))
				Expect(err).NotTo(HaveOccurred())
				config.Clusters[managementConfigName].Server = "https://tenant.example.test:6443"
				data, err := clientcmd.Write(*config)
				Expect(err).NotTo(HaveOccurred())
				management.Content = string(data)
			}, "does not use the pinned management Service and CA"),
			Entry("token kubeconfig outside management path", func(cloud *cloudConfig) {
				copied := *fileAt(cloud, dpuagent.KubeconfigPath)
				copied.Path = "/etc/dpf-sim/tenant.kubeconfig"
				cloud.WriteFiles = append(cloud.WriteFiles, copied)
			}, "contains an unapproved kubeconfig"),
			Entry("kubeconfig inside the dpuagent configuration", func(cloud *cloudConfig) {
				management := fileAt(cloud, dpuagent.KubeconfigPath)
				conf := fileAt(cloud, dpuagent.ConfPath)
				conf.Content += management.Content
			}, "contains an unapproved kubeconfig"),
		)
	})

	Describe("yamlLiteral", func() {
		DescribeTable("round-trips adversarial content",
			func(content string) {
				indicator, block := yamlLiteral(content, "  ")
				document := "content: " + indicator + "\n" + block
				var decoded struct {
					Content string `json:"content"`
				}
				Expect(yaml.Unmarshal([]byte(document), &decoded)).To(Succeed())
				Expect(decoded.Content).To(Equal(content), "literal content changed")
			},
			Entry("first line begins with spaces", "    indented first\nsecond"),
			Entry("YAML punctuation", "|:#[]{} &anchor *alias !tag\n---\n..."),
			Entry("no trailing newline", "first\nsecond"),
			Entry("one trailing newline", "first\nsecond\n"),
			Entry("multiple trailing newlines", "first\nsecond\n\n\n"),
		)
	})

	Describe("newCloudInitSecret", func() {
		It("enforces the userdata size limit", func() {
			secret, err := newCloudInitSecret(
				fleetWith(nil),
				0,
				strings.Repeat("x", maxCloudInitUserdataBytes),
			)
			Expect(err).To(HaveOccurred(), "newCloudInitSecret accepted oversized userdata")
			Expect(secret).To(BeNil(), "newCloudInitSecret accepted oversized userdata")

			secret, err = newCloudInitSecret(
				fleetWith(nil),
				0,
				strings.Repeat("x", maxCloudInitUserdataBytes-1),
			)
			Expect(err).NotTo(HaveOccurred(), "newCloudInitSecret rejected userdata under the limit")
			Expect(secret).NotTo(BeNil(), "newCloudInitSecret rejected userdata under the limit")
		})
	})
})
