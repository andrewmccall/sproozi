package localsetup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testInstallation(t *testing.T) (Layout, Installation) {
	t.Helper()
	l := Layout{Root: filepath.Join(testutil.PrivateDir(t), "root")}
	if err := l.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	inst := Installation{Version: 1, ID: "0a1b2c3d",
		Docker:   &Docker{Binary: "/usr/bin/docker", Socket: "/var/run/docker.sock"},
		Image:    &Image{Reference: testImage, ID: testImage, Source: SourceOverride, Contract: helperContract},
		Network:  &Network{Name: "sproozi-0a1b2c3d", Subnet: "172.30.0.0/16", Gateway: "172.30.0.1"},
		Endpoint: &Endpoint{GatewayIPv4: "192.168.65.254", ListenHost: "127.0.0.1"},
		Harnesses: []Harness{{Harness: harness.NativeCodex, Name: "local", Port: 18443, Model: "gpt-fixture",
			ProviderURL: openAIURL, Credential: filepath.Join(l.Credentials(), "openai")}}}
	return l, inst
}

func TestRenderedConfigIsAcceptedByTheHomeService(t *testing.T) {
	l, inst := testInstallation(t)
	inst.Harnesses = append(inst.Harnesses, Harness{Harness: harness.NativeClaude, Name: "local-claude", Port: 18444,
		Model: "claude-fixture", ProviderURL: anthropicURL, Credential: filepath.Join(l.Credentials(), "anthropic")})
	c := renderConfig(l, inst, DefaultInstructions)
	if err := writeConfig(l, c, false); err != nil {
		t.Fatal(err)
	}
	read, err := home.ReadConfig(l.Config())
	if err != nil {
		t.Fatal(err)
	}
	if read.Gateway.PKIDirectory != l.PKI() || read.Gateway.Listen != "127.0.0.1:18443" || read.Gateway.CertificateFile != "" {
		t.Fatalf("gateway %+v", read.Gateway)
	}
	if len(read.Harnesses) != 1 || read.Harnesses[0].GatewayPort != 18444 || read.Harnesses[0].NativeOptions != nil {
		t.Fatalf("second harness %+v", read.Harnesses)
	}
	if read.MaxDuration != "30m" || read.Allowances["model.inference"].Units.Value != 200000 || read.Allowances["model.inference"].CostMicros.Bounded {
		t.Fatal("starter limits differ from the proposal")
	}
	if read.Local.AgentLimits != agentLimits || read.Local.VolumeBytes != 512<<20 || read.Local.MaxCheckpointBytes != 8<<20 {
		t.Fatal("starter worker limits differ from the proposal")
	}
	for _, path := range []string{read.Directory, read.DockerConfig, read.Gateway.ProviderTokenFile} {
		if strings.HasPrefix(path, l.Agents()) {
			t.Fatalf("trusted path %s is inside the agents root", path)
		}
	}
}

func TestMergeKeepsUserEditsAddsHarnessesAndRefusesPlacementChanges(t *testing.T) {
	l, inst := testInstallation(t)
	existing := renderConfig(l, inst, DefaultInstructions)
	existing.MaxDuration = "2h"
	existing.Model = "edited-model"
	inst.Harnesses = append(inst.Harnesses, Harness{Harness: harness.NativeClaude, Name: "local-claude", Port: 18444,
		Model: "claude-fixture", ProviderURL: anthropicURL, Credential: "/x/anthropic"})
	merged, changed, err := mergeConfig(existing, renderConfig(l, inst, DefaultInstructions))
	if err != nil || !changed {
		t.Fatalf("merge changed=%v err=%v", changed, err)
	}
	if merged.MaxDuration != "2h" || merged.Model != "edited-model" || len(merged.Harnesses) != 1 {
		t.Fatalf("merge lost user edits or harness: %+v", merged)
	}
	if _, changed, err = mergeConfig(merged, renderConfig(l, inst, DefaultInstructions)); err != nil || changed {
		t.Fatalf("repeat merge changed=%v err=%v", changed, err)
	}
	moved := renderConfig(l, inst, DefaultInstructions)
	moved.Local.GatewayIPv4 = "10.0.0.1"
	var failure *Failure
	if _, _, err = mergeConfig(merged, moved); !errors.As(err, &failure) || failure.Component != "config" {
		t.Fatalf("placement change accepted: %v", err)
	}
	if _, _, err = mergeConfig(merged, renderConfig(l, inst, "other approved instructions")); err == nil {
		t.Fatal("conflicting instructions accepted")
	}
}

func TestWriteConfigBacksUpTheExistingFile(t *testing.T) {
	l, inst := testInstallation(t)
	c := renderConfig(l, inst, DefaultInstructions)
	if err := writeConfig(l, c, false); err != nil {
		t.Fatal(err)
	}
	c.MaxDuration = "1h"
	if err := writeConfig(l, c, true); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(l.Root, "config.json.backup-*"))
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	old, err := privatefs.ReadFile(backups[0], 256<<10)
	if err != nil || !strings.Contains(string(old), `"30m"`) {
		t.Fatalf("backup content: %v", err)
	}
}

func TestCredentialInputsStorePrivatelyAndRejectBadValues(t *testing.T) {
	l, _ := testInstallation(t)
	path, err := StoreCredential(l, "openai", CredentialInput{Stdin: strings.NewReader("sk-test-123\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Check(path, false); err != nil {
		t.Fatalf("credential not private: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "sk-test-123\n" {
		t.Fatalf("stored %q", data)
	}
	if _, err = StoreCredential(l, "openai", CredentialInput{Prompt: func(string) (string, error) { return "has space", nil }}); err == nil {
		t.Fatal("credential with whitespace accepted")
	}
	public := filepath.Join(t.TempDir(), "public-key")
	if err = os.WriteFile(public, []byte("sk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != goosWindows {
		if _, err = StoreCredential(l, "openai", CredentialInput{File: public}); err == nil {
			t.Fatal("world-readable credential file accepted")
		}
	}
	var failure *Failure
	if _, err = StoreCredential(l, "anthropic", CredentialInput{}); !errors.As(err, &failure) || !strings.Contains(failure.Next, "credentials add anthropic") {
		t.Fatalf("missing input: %v", err)
	}
}

func TestInstallationJournalRoundTripsAndRejectsCorruption(t *testing.T) {
	l, inst := testInstallation(t)
	fresh, err := LoadInstallation(l)
	if err != nil || len(fresh.ID) != 8 {
		t.Fatalf("fresh journal %+v %v", fresh, err)
	}
	inst.done("network")
	if err = l.save(inst); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadInstallation(l)
	if err != nil || loaded.ID != inst.ID || loaded.Network.Name != inst.Network.Name || loaded.Stages["network"].Completed.IsZero() {
		t.Fatalf("loaded %+v %v", loaded, err)
	}
	if err = privatefs.Replace(l.Root, "installation.json", []byte(`{"version":1,"id":"x","surprise":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadInstallation(l); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt journal accepted: %v", err)
	}
}

func TestSetupLockAdmitsOneOwner(t *testing.T) {
	l, _ := testInstallation(t)
	unlock, err := l.lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.lock(); err == nil {
		t.Fatal("second setup owner admitted")
	}
	unlock()
	if unlock, err = l.lock(); err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestRootDiscoveryPrefersPlatformConfigThenRecordedRoot(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("UserConfigDir ignores HOME on Windows")
	}
	homeDir := testutil.PrivateDir(t)
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	root, err := ResolveRoot("")
	if err != nil || root != filepath.Join(homeDir, "sproozi") {
		t.Fatalf("default root %s %v", root, err)
	}
	custom := filepath.Join(homeDir, "elsewhere")
	if err = RememberRoot(custom); err != nil {
		t.Fatal(err)
	}
	if root, err = ResolveRoot(""); err != nil || root != custom {
		t.Fatalf("recorded root %s %v", root, err)
	}
	l := Layout{Root: custom}
	if err = privatefs.EnsureDir(custom); err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Replace(custom, "config.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if path, err := DefaultConfigPath(); err != nil || path != l.Config() {
		t.Fatalf("local config path %s %v", path, err)
	}
	dir, _ := os.UserConfigDir()
	if err = privatefs.Replace(filepath.Join(dir, "sproozi"), "config.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if path, err := DefaultConfigPath(); err != nil || path != filepath.Join(dir, "sproozi", "config.json") {
		t.Fatalf("existing platform configuration must stay authoritative: %s %v", path, err)
	}
}

func TestFreePortSkipsRecordedAndBoundPorts(t *testing.T) {
	_, inst := testInstallation(t)
	port, err := freePort(18443, inst)
	if err != nil || port == 18443 {
		t.Fatalf("recorded port reused: %d %v", port, err)
	}
}
