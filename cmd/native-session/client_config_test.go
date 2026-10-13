package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"k8s.io/client-go/tools/clientcmd"
)

func TestFreshCodexProjectTrustCannotComeFromHistory(t *testing.T) {
	dir := t.TempDir()
	if err := installCodexProjectTrust(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "[projects.\"/workspace/session/workspace\"]\ntrust_level = \"trusted\"\n" {
		t.Fatalf("fresh trust configuration=%q err=%v", data, err)
	}
	if err = installCodexProjectTrust(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("untrusted history configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = installCodexProjectTrust(dir); err == nil {
		t.Fatal("accepted conflicting native trust configuration")
	}
}

func TestFreshKubernetesClientUsesInspectedAuthorityAndPlaceholderCredential(t *testing.T) {
	dir := t.TempDir()
	c := config{ClientConfig: &harness.NativeClientConfig{Kubernetes: true}}
	env, err := installNativeClients(c, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "kubeconfig")
	if len(env) != 1 || env[0] != "KUBECONFIG="+path {
		t.Fatalf("fresh client environment=%v", env)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.CurrentContext != "sproozi" ||
		parsed.Clusters["sproozi"].Server != "https://kubernetes.default.svc" ||
		parsed.Clusters["sproozi"].CertificateAuthority != harness.NativeLocalTrustBundle ||
		parsed.AuthInfos["sproozi"].Token != "sproozi-worker-placeholder" ||
		parsed.AuthInfos["sproozi"].Exec != nil {
		t.Fatalf("unexpected Kubernetes binding=%+v", parsed)
	}
	if _, err = installNativeClients(c, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "untrusted"), path); err != nil {
		t.Fatal(err)
	}
	if _, err = installNativeClients(c, dir); err == nil {
		t.Fatal("client installer accepted a workload symlink")
	}
}
