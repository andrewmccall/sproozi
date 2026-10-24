package localsetup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/modelauth"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// claudePlacement is the second harness's placement name.
const claudePlacement = "local-claude"

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
	inst.Harnesses = append(inst.Harnesses, Harness{Harness: harness.NativeClaude, Name: claudePlacement, Port: 18444,
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
	// The test root's control socket path can exceed the platform limit; the
	// default root does not, so only the other problems apply here.
	problems := slices.DeleteFunc(read.ServeProblems(), func(p string) bool { return strings.HasPrefix(p, "directory (") })
	if len(problems) != 0 {
		t.Fatalf("serve would refuse the rendered configuration: %q", problems)
	}
	if len(read.Harnesses) != 1 || read.Harnesses[0].GatewayPort != 18444 || read.Harnesses[0].NativeOptions != nil {
		t.Fatalf("second harness %+v", read.Harnesses)
	}
	if read.MaxDuration != "30m" || read.Allowances["model.inference"].Units.Value != 200000 || read.Allowances["model.inference"].CostMicros.Bounded {
		t.Fatal("starter limits differ from the proposal")
	}
	if read.Local.AgentLimits != agentLimits || read.Local.VolumeBytes != 512<<20 || read.Local.MaxCheckpointBytes != 32<<20 || read.Local.RetainedBytes != 256<<20 {
		t.Fatal("starter worker limits differ from the proposal")
	}
	for _, path := range []string{read.Directory, read.DockerConfig, read.Gateway.ProviderTokenFile} {
		if strings.HasPrefix(path, l.Profile()) {
			t.Fatalf("trusted path %s is inside the profile", path)
		}
	}
}

func TestMergeKeepsUserEditsAddsHarnessesAndRefusesPlacementChanges(t *testing.T) {
	l, inst := testInstallation(t)
	existing := renderConfig(l, inst, DefaultInstructions)
	existing.MaxDuration = "2h"
	existing.Model = "edited-model"
	inst.Harnesses = append(inst.Harnesses, Harness{Harness: harness.NativeClaude, Name: claudePlacement, Port: 18444,
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
	// A configuration from before the control connection gains its MCP
	// authority; one the user set is kept.
	older := renderConfig(l, inst, DefaultInstructions)
	older.Gateway.MCPAuthority = ""
	if added, changed, err := mergeConfig(older, renderConfig(l, inst, DefaultInstructions)); err != nil || !changed ||
		added.Gateway.MCPAuthority != MCPAuthority {
		t.Fatalf("missing MCP authority: changed=%v err=%v authority=%q", changed, err, added.Gateway.MCPAuthority)
	}
	older.Gateway.MCPAuthority = "tools.example:443"
	if kept, _, err := mergeConfig(older, renderConfig(l, inst, DefaultInstructions)); err != nil ||
		kept.Gateway.MCPAuthority != "tools.example:443" {
		t.Fatalf("user MCP authority replaced: %q %v", kept.Gateway.MCPAuthority, err)
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

// The root is --root, else $SPROOZI_ROOT, else the root init recorded, else
// ~/.sproozi; the configuration is always the root's.
func TestRootDiscoveryPrefersFlagThenEnvironmentThenRecordedRoot(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("UserConfigDir ignores HOME on Windows")
	}
	homeDir := testutil.PrivateDir(t)
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	t.Setenv(RootEnvironment, "")
	root, err := ResolveRoot("")
	if err != nil || root != filepath.Join(homeDir, ".sproozi") {
		t.Fatalf("default root %s %v", root, err)
	}
	custom := filepath.Join(homeDir, "elsewhere")
	if err = RememberRoot(custom); err != nil {
		t.Fatal(err)
	}
	if root, err = ResolveRoot(""); err != nil || root != custom {
		t.Fatalf("recorded root %s %v", root, err)
	}
	if path, err := DefaultConfigPath(); err != nil || path != (Layout{Root: custom}).Config() {
		t.Fatalf("config path %s %v", path, err)
	}
	environment := filepath.Join(homeDir, "from-env")
	t.Setenv(RootEnvironment, environment)
	if root, err = ResolveRoot(""); err != nil || root != environment {
		t.Fatalf("environment root %s %v", root, err)
	}
	// A root the variable chooses is not recorded.
	if err = RememberRoot(environment); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RootEnvironment, "")
	if root, err = ResolveRoot(""); err != nil || root != custom {
		t.Fatalf("environment root was recorded: %s %v", root, err)
	}
	if root, err = ResolveRoot("flag"); err != nil || !filepath.IsAbs(root) || filepath.Base(root) != "flag" {
		t.Fatalf("flag root %s %v", root, err)
	}
}

func TestFreePortSkipsRecordedAndBoundPorts(t *testing.T) {
	_, inst := testInstallation(t)
	port, err := freePort(18443, inst)
	if err != nil || port == 18443 {
		t.Fatalf("recorded port reused: %d %v", port, err)
	}
}

// A later agent takes the next port after the existing ones, unless --port
// says where to start.
func TestAddHarnessPortFollowsExistingOrFlag(t *testing.T) {
	for _, tc := range []struct {
		flag uint16
		want uint16
	}{{0, 18444}, {18600, 18600}} {
		l, inst := testInstallation(t)
		i := &installer{l: l, inst: inst, o: Options{Harness: harness.NativeClaude, Port: tc.flag, Progress: io.Discard}}
		if !PortFree("127.0.0.1", tc.want) || !PortFree("0.0.0.0", tc.want) {
			t.Skipf("port %d is in use on this host", tc.want)
		}
		if err := i.addHarness(); err != nil {
			t.Fatal(err)
		}
		if got := i.selected.Port; got != tc.want {
			t.Fatalf("--port %d: port %d, want %d", tc.flag, got, tc.want)
		}
	}
}

// Setup refuses a root it does not own, such as a Git checkout, and never asks
// for its permissions to be changed; a new, empty or installed root is fine.
func TestCheckRootRefusesForeignDirectories(t *testing.T) {
	l, _ := testInstallation(t)
	if err := l.checkRoot(); err != nil {
		t.Fatalf("installation root refused: %v", err)
	}
	if err := (Layout{Root: filepath.Join(l.Root, "missing")}).checkRoot(); err != nil {
		t.Fatalf("missing root refused: %v", err)
	}
	checkout := Layout{Root: filepath.Join(testutil.PrivateDir(t), "sproozi")}
	if err := os.MkdirAll(filepath.Join(checkout.Root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkout.checkRoot()
	if err == nil || !strings.Contains(err.Error(), "is a Git checkout") || !strings.Contains(err.Error(), "--root") ||
		strings.Contains(err.Error(), "chmod") {
		t.Fatalf("checkout root: %v", err)
	}
	other := Layout{Root: filepath.Join(testutil.PrivateDir(t), "other")}
	if err = os.Mkdir(other.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = testutil.WriteFile(filepath.Join(other.Root, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = other.checkRoot(); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("foreign root: %v", err)
	}
}

// A harness added without a model leaves the choice to the harness, and serve
// accepts that configuration.
func TestRenderedConfigWithoutModel(t *testing.T) {
	l, inst := testInstallation(t)
	inst.Harnesses[0].Model = ""
	if err := writeConfig(l, renderConfig(l, inst, DefaultInstructions), false); err != nil {
		t.Fatal(err)
	}
	read, err := home.ReadConfig(l.Config())
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range read.ServeProblems() {
		if strings.HasPrefix(problem, "model") {
			t.Fatalf("serve requires a model: %s", problem)
		}
	}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "provider CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// A public provider CA bundle is copied into the installation as a private
// file the gateway accepts; a bundle anyone can change, or no PEM, is refused.
func TestStoreProviderCACopiesAPrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	l, _ := testInstallation(t)
	source := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(source, testCAPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}
	stored, err := StoreProviderCA(l, "openai", source)
	if err != nil || stored != filepath.Join(l.Credentials(), "openai-ca.pem") {
		t.Fatalf("stored %s %v", stored, err)
	}
	if _, err = privatefs.ReadFile(stored, 64<<10); err != nil {
		t.Fatalf("stored copy fails the private-file check: %v", err)
	}
	if err = os.Chmod(source, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err = StoreProviderCA(l, "openai", source); err == nil || !strings.Contains(err.Error(), "writable by every account") {
		t.Fatalf("world-writable source: %v", err)
	}
	if err = os.WriteFile(source, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = StoreProviderCA(l, "openai", source); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("non-PEM source: %v", err)
	}
}

// An explicit CA bundle on a later run replaces the stored path for the
// harness it names, whether it is the primary harness or a further one.
func TestSetProviderCAReplacesTheStoredValue(t *testing.T) {
	c := home.Config{Local: runtimedocker.PlacementConfig{Harness: harness.NativeCodex},
		Gateway:   home.GatewayConfig{ProviderCAFile: "/old/ca.pem"},
		Harnesses: []home.LocalHarness{{Harness: harness.NativeClaude}}}
	if !setProviderCA(&c, harness.NativeCodex, "/new/openai-ca.pem") || c.Gateway.ProviderCAFile != "/new/openai-ca.pem" {
		t.Fatalf("primary: %+v", c.Gateway)
	}
	if setProviderCA(&c, harness.NativeCodex, "/new/openai-ca.pem") {
		t.Fatal("unchanged value reported as a change")
	}
	if !setProviderCA(&c, harness.NativeClaude, "/new/anthropic-ca.pem") || c.Harnesses[0].ProviderCAFile != "/new/anthropic-ca.pem" {
		t.Fatalf("further harness: %+v", c.Harnesses)
	}
}

// A provider certificate the client does not trust names --provider-ca-file.
func TestUntrustedProviderCertificateNamesTheCAFlag(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	l, _ := testInstallation(t)
	key, err := StoreCredential(l, "openai", CredentialInput{Stdin: strings.NewReader("sk-test")})
	if err != nil {
		t.Fatal(err)
	}
	err = CheckCredential(t.Context(), harness.NativeCodex, server.URL, "", key)
	if err == nil || !strings.Contains(err.Error(), "certificate is not trusted") ||
		!strings.Contains(err.Error(), "--provider-ca-file") {
		t.Fatalf("untrusted certificate: %v", err)
	}
}

// A pending restart is recorded in the root, which setup still owns.
func TestRestartPendingIsRecordedInTheRoot(t *testing.T) {
	l, _ := testInstallation(t)
	if PendingRestart(l) != "" {
		t.Fatal("pending restart without a record")
	}
	if err := markRestartPending(l, "1 session(s) are running"); err != nil {
		t.Fatal(err)
	}
	if PendingRestart(l) != "1 session(s) are running" || l.checkRoot() != nil {
		t.Fatalf("pending restart %q, root %v", PendingRestart(l), l.checkRoot())
	}
}

// Each harness keeps either a gateway-held key or its own sign-in, and setup
// can switch it either way (ADR 0047).
func TestSignInIsChosenPerHarness(t *testing.T) {
	l, inst := testInstallation(t)
	keyed := inst
	keyed.Harnesses = append(slices.Clone(inst.Harnesses), Harness{Harness: harness.NativeClaude, Name: claudePlacement, Port: 18444,
		ProviderURL: anthropicURL, Credential: filepath.Join(l.Credentials(), "anthropic")})
	withKey := renderConfig(l, keyed, DefaultInstructions)
	signedIn := keyed
	signedIn.Harnesses = slices.Clone(keyed.Harnesses)
	signedIn.Harnesses[1].Credential = ""
	toSignIn, changed, err := mergeConfig(withKey, renderConfig(l, signedIn, DefaultInstructions))
	if err != nil || !changed || toSignIn.Harnesses[0].ProviderTokenFile != "" || toSignIn.Gateway.ProviderTokenFile == "" {
		t.Fatalf("switch to sign-in: changed=%v claude=%q codex=%q err=%v", changed, toSignIn.Harnesses[0].ProviderTokenFile,
			toSignIn.Gateway.ProviderTokenFile, err)
	}
	if problems := slices.DeleteFunc(toSignIn.ServeProblems(), func(p string) bool { return strings.HasPrefix(p, "directory (") }); len(problems) != 0 {
		t.Fatalf("serve would refuse the signed-in configuration: %q", problems)
	}
	back, changed, err := mergeConfig(toSignIn, withKey)
	if err != nil || !changed || back.Harnesses[0].ProviderTokenFile == "" {
		t.Fatalf("switch back to a key: changed=%v claude=%q err=%v", changed, back.Harnesses[0].ProviderTokenFile, err)
	}
	edited := withKey
	edited.Harnesses = slices.Clone(withKey.Harnesses)
	edited.Harnesses[0].ProviderTokenFile = "/elsewhere/anthropic-key"
	if kept, _, _ := mergeConfig(edited, withKey); kept.Harnesses[0].ProviderTokenFile != "/elsewhere/anthropic-key" {
		t.Fatal("an edited key path was replaced")
	}
}

// A gateway-held ChatGPT sign-in is reused while valid, so a later init needs
// no browser, and only an OpenAI harness can choose it (ADR 0047).
func TestChatGPTSignInIsReusedForAnOpenAIHarness(t *testing.T) {
	l, inst := testInstallation(t)
	session := modelauth.Session{Issuer: modelauth.Issuer, Subject: "account", ClientID: "oaiapp_fixture",
		HostID: "urn:uuid:fixture", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: "fixture-id",
		TokenType: "Bearer", ExpiresIn: 3600, SavedAt: time.Now().UTC(), Scopes: []string{modelauth.PlanScope}}
	if err := modelauth.NewFileSessionStore(filepath.Join(l.Credentials(), "chatgpt-session.json")).Save(session); err != nil {
		t.Fatal(err)
	}
	i := &installer{l: l, inst: inst, o: Options{Harness: harness.NativeCodex, ChatGPT: true, Progress: io.Discard}, selected: &inst.Harnesses[0]}
	if err := i.signInChatGPT(t.Context(), ProviderFor(harness.NativeCodex, harness.CredentialKey)); err != nil {
		t.Fatal(err)
	}
	if i.selected.Kind != modelauth.ChatGPTMode || tokenKind(*i.selected) != modelauth.ChatGPTMode {
		t.Fatalf("harness %+v", *i.selected)
	}
	i.o.Harness = harness.NativeClaude
	if err := i.signInChatGPT(t.Context(), ProviderFor(harness.NativeClaude, harness.CredentialKey)); err == nil {
		t.Fatal("Claude Code was given a ChatGPT sign-in")
	}
}

// Copilot reaches OpenAI with a key, in bring-your-own-key mode, and GitHub's
// Copilot service with a plan token or its own sign-in. The stored
// credential and default provider URL follow each change of mechanism.
func TestCopilotProviderFollowsItsMechanism(t *testing.T) {
	for credential, want := range map[harness.NativeCredential]Provider{
		harness.CredentialKey:    {Name: "openai", URL: openAIURL},
		harness.CredentialToken:  {Name: ProviderCopilot, URL: copilotURL},
		harness.CredentialSignIn: {Name: ProviderCopilot, URL: copilotURL},
	} {
		if got := ProviderFor(harness.NativeCopilot, credential); got != want {
			t.Fatalf("%s: %+v", credential, got)
		}
	}
	if got := ProviderFor(harness.NativeCodex, harness.CredentialSignIn); got.Name != "openai" {
		t.Fatalf("Codex signed in: %+v", got)
	}
	l, inst := testInstallation(t)
	inst.Harnesses = append(inst.Harnesses, Harness{Harness: harness.NativeCopilot, Name: "local-copilot", Port: 18444,
		ProviderURL: openAIURL, Credential: filepath.Join(l.Credentials(), "openai")})
	i := &installer{l: l, inst: inst, o: Options{Harness: harness.NativeCopilot, Token: true, Progress: io.Discard,
		Credential: CredentialInput{Stdin: strings.NewReader("github_pat_fixture")}}}
	if err := i.prepareHarness(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := *i.selected
	if h.Mechanism() != harness.CredentialToken || h.ProviderURL != copilotURL ||
		h.Credential != filepath.Join(l.Credentials(), ProviderCopilot) || !HasCredential(l, ProviderCopilot) {
		t.Fatalf("token: %+v", h)
	}
	i.o = Options{Harness: harness.NativeCopilot, SignIn: true, Progress: io.Discard}
	if err := i.prepareHarness(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h = *i.selected; h.Mechanism() != harness.CredentialSignIn || h.ProviderURL != copilotURL {
		t.Fatalf("sign-in: %+v", h)
	}
	i.o = Options{Harness: harness.NativeCopilot, Progress: io.Discard,
		Credential: CredentialInput{Stdin: strings.NewReader("sk-fixture")}}
	if err := i.prepareHarness(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h = *i.selected; h.Mechanism() != harness.CredentialKey || h.ProviderURL != openAIURL ||
		h.Credential != filepath.Join(l.Credentials(), "openai") {
		t.Fatalf("key: %+v", h)
	}
	// A private endpoint stays as configured.
	i.o = Options{Harness: harness.NativeCopilot, Token: true, ProviderURL: "http://127.0.0.1:18800", Progress: io.Discard}
	if err := i.prepareHarness(t.Context()); err != nil {
		t.Fatal(err)
	}
	i.o = Options{Harness: harness.NativeCopilot, SignIn: true, Progress: io.Discard}
	if err := i.prepareHarness(t.Context()); err != nil || i.selected.ProviderURL != "http://127.0.0.1:18800" {
		t.Fatalf("private endpoint: %+v %v", *i.selected, err)
	}
}
