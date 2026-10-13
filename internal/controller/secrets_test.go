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

package controller

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/utils"
)

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

func assertCloudConfigTopLevelKeys(t *testing.T, userdata string) {
	t.Helper()

	var raw map[string]any
	if err := yaml.Unmarshal([]byte(userdata), &raw); err != nil {
		t.Fatalf("parse userdata as map: %v\n%s", err, userdata)
	}
	// runcmd is absent for an enabled guest and present only to mask units
	// for a disabled one, so it is optional rather than expected.
	want := []string{"chpasswd", "hostname", "ssh_pwauth", "users", "write_files"}
	got := slices.DeleteFunc(mapKeys(raw), func(key string) bool { return key == "runcmd" })
	if !slices.Equal(got, want) {
		t.Fatalf("top-level keys %v want %v plus an optional runcmd", got, want)
	}
}

func secretDataKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertForbiddenCloudInitContent(t *testing.T, userdata string) {
	t.Helper()
	if err := validateForbiddenCloudInitContent(userdata); err != nil {
		t.Fatal(err)
	}
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
		case dpuAgentConfPath:
			foundConf = true
		case dpuAgentKubeconfigPath:
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

func TestDesiredCloudInitSecret(t *testing.T) {
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "fleet-ns", UID: "uid"},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:          3000,
			DevicesPerDPUNode: 10,
			Bootstrap: &scalev1alpha1.BootstrapSpec{
				DPUClusterRef: scalev1alpha1.DPUClusterReference{Name: "tenant1", Namespace: "dpf-ns"},
			},
		},
	}
	const ordinal = 12
	kubeconfig, err := managementKubeconfig(
		[]byte("test-management-token"),
		[]byte("test-management-ca"),
	)
	if err != nil {
		t.Fatalf("managementKubeconfig: %v", err)
	}
	secret, err := desiredCloudInitSecret(fleet, ordinal, kubeconfig)
	if err != nil {
		t.Fatalf("desiredCloudInitSecret: %v", err)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		t.Fatalf("type %q", secret.Type)
	}
	if got := secretDataKeys(secret.Data); len(got) != 1 || got[0] != "userdata" {
		t.Fatalf("data keys %v", got)
	}
	ud := string(secret.Data["userdata"])
	assertForbiddenCloudInitContent(t, ud)
	assertCloudConfigTopLevelKeys(t, ud)

	var cfg cloudConfig
	if err := yaml.Unmarshal([]byte(ud), &cfg); err != nil {
		t.Fatalf("parse userdata: %v\n%s", err, ud)
	}
	if cfg.Hostname != "scale-3000-dpu-000012" {
		t.Fatalf("hostname %q", cfg.Hostname)
	}
	assertCloudInitDebugUser(t, cfg)
	if len(cfg.WriteFiles) != 3 {
		t.Fatalf("write_files len %d", len(cfg.WriteFiles))
	}
	identity := cfg.WriteFiles[0]
	if identity.Path != dpuIdentityPath {
		t.Fatalf("identity path %q", identity.Path)
	}
	if identity.Permissions != "0600" {
		t.Fatalf("identity permissions %q", identity.Permissions)
	}
	// Every line is derived from the fleet and the ordinal. Nothing here can
	// come from a DPU object, because none exists when this Secret is written.
	wantIdentity := strings.Join([]string{
		"DPU_NAME=scale-3000-host-0001-scale-3000-device-000012",
		"DPU_NAMESPACE=fleet-ns",
		"DPU_NODE_NAME=scale-3000-host-0001",
		"DPU_DEVICE_NAME=scale-3000-device-000012",
		"DPU_CLUSTER_NAME=tenant1",
		"DPU_CLUSTER_NAMESPACE=dpf-ns",
	}, "\n") + "\n"
	if identity.Content != wantIdentity {
		t.Fatalf("identity content\ngot:  %q\nwant: %q", identity.Content, wantIdentity)
	}
	conf := cfg.WriteFiles[1]
	if conf.Path != dpuAgentConfPath || conf.Permissions != "0600" {
		t.Fatalf("dpuagent conf file = %#v", conf)
	}
	wantConf, err := renderMockDPUAgentConfig(fleet, utils.SerialNumber(fleet.Name, ordinal))
	if err != nil {
		t.Fatalf("renderMockDPUAgentConfig: %v", err)
	}
	if conf.Content != wantConf {
		t.Fatalf("dpuagent conf content\ngot:  %q\nwant: %q", conf.Content, wantConf)
	}
	management := cfg.WriteFiles[2]
	if management.Path != dpuAgentKubeconfigPath || management.Permissions != "0600" {
		t.Fatalf("management kubeconfig file = %#v", management)
	}
	if management.Content != string(kubeconfig) {
		t.Fatalf("management kubeconfig content\ngot:  %q\nwant: %q", management.Content, kubeconfig)
	}
	// Both units are baked into the image and enabled there; an enabled
	// guest must neither run commands nor name a unit it does not own.
	if len(cfg.Runcmd) != 0 {
		t.Fatalf("runcmd = %#v, want none", cfg.Runcmd)
	}
	for _, unit := range []string{waitKubeletJoinUnit, mockDPUAgentUnit} {
		if strings.Contains(ud, unit) {
			t.Fatalf("enabled cloud-init userdata mentions %q:\n%s", unit, ud)
		}
	}
	if secret.Labels[labelOrdinal] != "000012" {
		t.Fatalf("ordinal label %q", secret.Labels[labelOrdinal])
	}
	if secret.Labels[labelFleetUID] != "uid" {
		t.Fatalf("fleet uid label %q", secret.Labels[labelFleetUID])
	}
	if len(secret.Data["userdata"]) >= maxCloudInitUserdataBytes {
		t.Fatalf("userdata size = %d, want less than %d", len(secret.Data["userdata"]), maxCloudInitUserdataBytes)
	}
}

func TestCloudInitUserdataRejectsMissingDependencies(t *testing.T) {
	if _, err := cloudInitUserdata(nil, 0, []byte("kubeconfig")); err == nil {
		t.Fatal("cloudInitUserdata accepted a nil SimFleet")
	}
	for _, kubeconfig := range [][]byte{nil, {}} {
		secret, err := desiredCloudInitSecret(fleetWith(nil), 3, kubeconfig)
		if err == nil || secret != nil {
			t.Fatalf("desiredCloudInitSecret accepted an empty management kubeconfig %#v", kubeconfig)
		}
		if !strings.Contains(err.Error(), "ordinal 3") || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("error does not name the empty kubeconfig and ordinal: %v", err)
		}
	}
}

func TestDPUIdentityContentWithoutBootstrap(t *testing.T) {
	lines := strings.Split(dpuIdentityContent(fleetWith(nil), 0), "\n")
	if lines[len(lines)-1] != "" {
		t.Fatalf("identity content does not end with a newline: %q", lines)
	}
	lines = lines[:len(lines)-1]
	if len(lines) != 6 {
		t.Fatalf("identity lines %q, want 6", lines)
	}
	if lines[4] != "DPU_CLUSTER_NAME=" || lines[5] != "DPU_CLUSTER_NAMESPACE=" {
		t.Fatalf("cluster lines %q, want empty values", lines[4:])
	}
}

func TestDesiredCloudInitSecretDisabledOmitsAgentDependenciesAndMasksService(t *testing.T) {
	disabled := false
	fleet := fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &disabled})
	fleet.UID = "fleet-uid"
	secret, err := desiredCloudInitSecret(fleet, 0, nil)
	if err != nil {
		t.Fatalf("desiredCloudInitSecret: %v", err)
	}
	userdata := string(secret.Data["userdata"])
	assertForbiddenCloudInitContent(t, userdata)
	assertCloudConfigTopLevelKeys(t, userdata)

	var cfg cloudConfig
	if err := yaml.Unmarshal([]byte(userdata), &cfg); err != nil {
		t.Fatalf("parse userdata: %v\n%s", err, userdata)
	}
	if len(cfg.WriteFiles) != 1 || cfg.WriteFiles[0].Path != dpuIdentityPath {
		t.Fatalf("disabled write_files = %#v", cfg.WriteFiles)
	}
	// Without a credential the join helper would fail on a loop, so both
	// baked units must be masked, and persistently so (see cloudInitUserdata).
	for _, unit := range []string{mockDPUAgentUnit, waitKubeletJoinUnit} {
		if !slices.Contains(cfg.Runcmd, "systemctl mask --now "+unit) {
			t.Fatalf("disabled runcmd does not persistently mask %s: %#v", unit, cfg.Runcmd)
		}
	}
	for _, command := range cfg.Runcmd {
		if strings.Contains(command, "systemctl mask") &&
			strings.Contains(command, "--runtime") {
			t.Fatalf("runcmd masks a unit only for this boot: %q", command)
		}
	}
	assertCloudInitDebugUser(t, cfg)
}

func assertCloudInitDebugUser(t *testing.T, cfg cloudConfig) {
	t.Helper()
	if !cfg.SSHPwauth {
		t.Fatal("ssh_pwauth is false")
	}
	if cfg.Chpasswd.Expire {
		t.Fatal("chpasswd.expire is true")
	}
	wantPasswd := cloudInitDebugUser + ":" + cloudInitDebugPassword
	if strings.TrimSpace(cfg.Chpasswd.List) != wantPasswd {
		t.Fatalf("chpasswd.list %q want %q", cfg.Chpasswd.List, wantPasswd)
	}
	if len(cfg.Users) != 1 {
		t.Fatalf("users %#v", cfg.Users)
	}
	user := cfg.Users[0]
	if user.Name != cloudInitDebugUser {
		t.Fatalf("user name %q", user.Name)
	}
	if user.LockPasswd == nil || *user.LockPasswd {
		t.Fatalf("lock_passwd %#v", user.LockPasswd)
	}
	if user.Sudo != "ALL=(ALL) NOPASSWD:ALL" {
		t.Fatalf("sudo %q", user.Sudo)
	}
	if user.Groups != "sudo" {
		t.Fatalf("groups %q", user.Groups)
	}
	if user.Shell != "/bin/bash" {
		t.Fatalf("shell %q", user.Shell)
	}
}

func TestDesiredCloudInitSecretRejectsOversizeWithoutLeakingCredentials(t *testing.T) {
	const credentialMarker = "do-not-leak-management-credential"
	kubeconfig := []byte(strings.Repeat(credentialMarker, maxCloudInitUserdataBytes))
	secret, err := desiredCloudInitSecret(fleetWith(nil), 7, kubeconfig)
	if err == nil {
		t.Fatal("desiredCloudInitSecret returned nil error")
	}
	if secret != nil {
		t.Fatal("desiredCloudInitSecret returned a Secret for oversized userdata")
	}
	if !strings.Contains(err.Error(), "ordinal 7") {
		t.Fatalf("error does not identify the ordinal: %v", err)
	}
	if strings.Contains(err.Error(), credentialMarker) {
		t.Fatalf("error leaked credential material: %v", err)
	}
}

func TestDesiredCloudInitSecretEmbedsLimitedManagementKubeconfig(t *testing.T) {
	const testToken = "do-not-log-management-token"
	testCA := []byte("test-management-ca")
	kubeconfig, err := managementKubeconfig([]byte(testToken), testCA)
	if err != nil {
		t.Fatalf("managementKubeconfig: %v", err)
	}
	secret, err := desiredCloudInitSecret(fleetWith(nil), 0, kubeconfig)
	if err != nil {
		t.Fatalf("desiredCloudInitSecret: %v", err)
	}

	var cloud cloudConfig
	if err := yaml.Unmarshal(secret.Data["userdata"], &cloud); err != nil {
		t.Fatalf("parse cloud-config: %v", err)
	}
	var embedded string
	for _, file := range cloud.WriteFiles {
		if file.Path == dpuAgentKubeconfigPath {
			embedded = file.Content
		}
	}
	if embedded == "" {
		t.Fatal("management kubeconfig is absent")
	}
	config, err := clientcmd.Load([]byte(embedded))
	if err != nil {
		t.Fatalf("parse embedded management kubeconfig: %v", err)
	}
	if len(config.Clusters) != 1 || len(config.AuthInfos) != 1 || len(config.Contexts) != 1 {
		t.Fatal("management kubeconfig is not limited to one cluster, user, and context")
	}
	cluster := config.Clusters[managementConfigName]
	if cluster == nil || cluster.Server != managementServiceURL ||
		!bytes.Equal(cluster.CertificateAuthorityData, testCA) ||
		cluster.InsecureSkipTLSVerify {
		t.Fatal("management kubeconfig cluster is not pinned to the management Service and CA")
	}
	auth := config.AuthInfos[managementConfigName]
	if auth == nil || auth.Token != testToken {
		t.Fatal("management kubeconfig does not contain the expected bearer-token authentication")
	}
	if len(auth.ClientCertificateData) != 0 || len(auth.ClientKeyData) != 0 ||
		auth.Username != "" || auth.Password != "" || auth.Exec != nil || auth.AuthProvider != nil {
		t.Fatal("management kubeconfig contains authentication material other than its bearer token")
	}
	assertForbiddenCloudInitContent(t, string(secret.Data["userdata"]))
}

func TestForbiddenCloudInitContentRejectsForeignAndTenantKubeconfigs(t *testing.T) {
	const testToken = "do-not-log-tenant-token"
	makeUserdata := func(t *testing.T, mutate func(*cloudConfig)) string {
		t.Helper()
		kubeconfig, err := managementKubeconfig([]byte(testToken), []byte("test-ca"))
		if err != nil {
			t.Fatalf("managementKubeconfig: %v", err)
		}
		secret, err := desiredCloudInitSecret(fleetWith(nil), 0, kubeconfig)
		if err != nil {
			t.Fatalf("desiredCloudInitSecret: %v", err)
		}
		var cloud cloudConfig
		if err := yaml.Unmarshal(secret.Data["userdata"], &cloud); err != nil {
			t.Fatalf("parse cloud-config: %v", err)
		}
		mutate(&cloud)
		data, err := yaml.Marshal(cloud)
		if err != nil {
			t.Fatalf("encode cloud-config: %v", err)
		}
		return "#cloud-config\n" + string(data)
	}

	fileAt := func(t *testing.T, cloud *cloudConfig, path string) *cloudWriteFile {
		t.Helper()
		for i := range cloud.WriteFiles {
			if cloud.WriteFiles[i].Path == path {
				return &cloud.WriteFiles[i]
			}
		}
		t.Fatalf("cloud-config writes no %s", path)
		return nil
	}

	tests := []struct {
		name    string
		mutate  func(*testing.T, *cloudConfig)
		wantErr string
	}{
		{
			name:   "unmodified round trip",
			mutate: func(*testing.T, *cloudConfig) {},
		},
		{
			name: "foreign management server",
			mutate: func(t *testing.T, cloud *cloudConfig) {
				management := fileAt(t, cloud, dpuAgentKubeconfigPath)
				config, err := clientcmd.Load([]byte(management.Content))
				if err != nil {
					t.Fatalf("parse management kubeconfig: %v", err)
				}
				config.Clusters[managementConfigName].Server = "https://tenant.example.test:6443"
				data, err := clientcmd.Write(*config)
				if err != nil {
					t.Fatalf("encode foreign kubeconfig: %v", err)
				}
				management.Content = string(data)
			},
			wantErr: "does not use the pinned management Service and CA",
		},
		{
			name: "token kubeconfig outside management path",
			mutate: func(t *testing.T, cloud *cloudConfig) {
				copied := *fileAt(t, cloud, dpuAgentKubeconfigPath)
				copied.Path = "/etc/dpf-sim/tenant.kubeconfig"
				cloud.WriteFiles = append(cloud.WriteFiles, copied)
			},
			wantErr: "contains an unapproved kubeconfig",
		},
		{
			name: "kubeconfig inside the dpuagent configuration",
			mutate: func(t *testing.T, cloud *cloudConfig) {
				management := fileAt(t, cloud, dpuAgentKubeconfigPath)
				conf := fileAt(t, cloud, dpuAgentConfPath)
				conf.Content += management.Content
			},
			wantErr: "contains an unapproved kubeconfig",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			userdata := makeUserdata(t, func(cloud *cloudConfig) { testCase.mutate(t, cloud) })
			err := validateForbiddenCloudInitContent(userdata)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("validateForbiddenCloudInitContent: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("validateForbiddenCloudInitContent error %v, want %q", err, testCase.wantErr)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("validation error leaked kubeconfig token")
			}
		})
	}
}

func TestYAMLLiteralRoundTripAdversarialContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "first line begins with spaces", content: "    indented first\nsecond"},
		{name: "YAML punctuation", content: "|:#[]{} &anchor *alias !tag\n---\n..."},
		{name: "no trailing newline", content: "first\nsecond"},
		{name: "one trailing newline", content: "first\nsecond\n"},
		{name: "multiple trailing newlines", content: "first\nsecond\n\n\n"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			indicator, block := yamlLiteral(testCase.content, "  ")
			document := "content: " + indicator + "\n" + block
			var decoded struct {
				Content string `json:"content"`
			}
			if err := yaml.Unmarshal([]byte(document), &decoded); err != nil {
				t.Fatalf("parse literal block: %v", err)
			}
			if decoded.Content != testCase.content {
				t.Fatalf("literal content changed: got %q, want %q", decoded.Content, testCase.content)
			}
		})
	}
}

func TestNewCloudInitSecretEnforcesSizeLimit(t *testing.T) {
	secret, err := newCloudInitSecret(
		fleetWith(nil),
		0,
		strings.Repeat("x", maxCloudInitUserdataBytes),
	)
	if err == nil || secret != nil {
		t.Fatal("newCloudInitSecret accepted oversized userdata")
	}

	secret, err = newCloudInitSecret(
		fleetWith(nil),
		0,
		strings.Repeat("x", maxCloudInitUserdataBytes-1),
	)
	if err != nil || secret == nil {
		t.Fatalf("newCloudInitSecret rejected userdata under the limit: %v", err)
	}
}

// The largest spec.dpuAgent.bmc the CRD admits must still fit a cloud-init
// Secret next to a realistically sized management credential. The renderer
// does not validate overrides, so the longest operation name repeats 32 times with four
// methods here even though admission would refuse that list; only the bytes
// matter.
func TestLargestAdmittedBMCSettingsFitCloudInit(t *testing.T) {
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
			Bootstrap: &scalev1alpha1.BootstrapSpec{
				DPUClusterRef: scalev1alpha1.DPUClusterReference{Name: "tenant1", Namespace: "dpf-ns"},
			},
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
	kubeconfig, err := managementKubeconfig(
		[]byte(strings.Repeat("t", 1400)),
		[]byte(strings.Repeat("c", 1900)),
	)
	if err != nil {
		t.Fatalf("managementKubeconfig: %v", err)
	}
	secret, err := desiredCloudInitSecret(fleet, 2999, kubeconfig)
	if err != nil {
		t.Fatalf("desiredCloudInitSecret: %v", err)
	}
	userdata := string(secret.Data["userdata"])
	if got := strings.Count(userdata, "name: CertificateService.ReplaceCertificate"); got != len(overrides) {
		t.Fatalf("userdata renders %d overrides, want %d", got, len(overrides))
	}
	if size := len(userdata); size >= maxCloudInitUserdataBytes {
		t.Fatalf("userdata size = %d, want less than %d", size, maxCloudInitUserdataBytes)
	}
}
