package home

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"

	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
)

const fixtureCertificatePEM = "CERTIFICATE"

func composedConfig(t *testing.T) Config {
	t.Helper()
	root := testutil.PrivateDir(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: []string{inspectionHost, "kubernetes.default.svc", "github.com", "api.github.com"}, IPAddresses: []net.IP{net.ParseIP("192.168.65.254")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	cert, err := x509.CreateCertificate(rand.Reader, leaf, parsed, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"ca.pem": pem.EncodeToMemory(&pem.Block{Type: fixtureCertificatePEM, Bytes: der}), "gateway.crt": pem.EncodeToMemory(&pem.Block{Type: fixtureCertificatePEM, Bytes: cert}), "gateway.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), "provider-token": []byte("public-fixture-placeholder")}
	for name, bytes := range files {
		if err = testutil.WriteFile(filepath.Join(root, name), bytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	limits := runtimedocker.ResourceLimits{MemoryBytes: 64 << 20, CPUMilli: 500, PIDs: 64}
	image := "sha256:" + strings.Repeat("a", 64)
	socket := filepath.Join(root, "docker.sock")
	if runtime.GOOS == goosWindows {
		socket = "npipe:////./pipe/sproozi-fixture"
	}
	return Config{Directory: filepath.Join(root, "home"), DockerBinary: binary, DockerSocket: socket, DockerConfig: filepath.Join(root, "docker-cli"), MaxDuration: "1h", Model: "fixture", Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 10}}}, Local: runtimedocker.PlacementConfig{Harness: harness.NativeCodex, Name: testLocal, NativeImageID: image, HelperImageID: image, AnchorImageID: image, NetworkName: "bridge-fixture", GatewayIPv4: "192.168.65.254", GatewayPort: 18443, Instructions: testInstructions, InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(testInstructions))), AgentLimits: limits, HelperLimits: limits, AnchorLimits: limits, VolumeBytes: 64 << 20, MaxCheckpointBytes: testLimit, RetainedBytes: 32 << 20}, Gateway: GatewayConfig{Listen: "0.0.0.0:18443", CertificateFile: filepath.Join(root, "gateway.crt"), KeyFile: filepath.Join(root, "gateway.key"), TrustBundleFile: filepath.Join(root, "ca.pem"), ProviderURL: "http://127.0.0.1:18765", ProviderTokenFile: filepath.Join(root, "provider-token")}}
}

func TestProductionCompositionTakesExclusiveOwnershipWithoutLaunching(t *testing.T) {
	c := composedConfig(t)
	s, err := Open(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := "home: another home daemon is using directory " + c.Directory +
		"; stop it first (\"sproozi serve\" holds it until it exits)"
	if _, err = Open(c, io.Discard); err == nil || err.Error() != want {
		t.Fatalf("second home writer: got %v, want %q", err, want)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.gateway.Handler == nil || reopened.local == nil || reopened.coordinator == nil {
		t.Fatal("production adapters absent")
	}
}

// A refused placement settings change reaches the caller as an error; it once
// left a typed-nil placement that panicked in Close and hid the reason.
func TestChangedPlacementSettingsReturnTheReasonInsteadOfPanicking(t *testing.T) {
	c := composedConfig(t)
	s, err := Open(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(c.Directory, "docker", strings.Repeat("e", 64)+".json")
	if err = testutil.WriteFile(record, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Local.NativeImageID = "sha256:" + strings.Repeat("f", 64)
	_, err = Open(c, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "local placement settings changed (local.nativeImageID ") {
		t.Fatalf("got %v", err)
	}
}

func TestGatewayTLSRejectsDifferentDockerEndpoint(t *testing.T) {
	c := composedConfig(t)
	c.Local.GatewayIPv4 = "192.168.65.253"
	if _, _, _, err := gatewayTLS(c); err == nil {
		t.Fatal("gateway endpoint lacked matching IP SAN")
	}
}

func TestKubernetesConfigKeepsCredentialInGatewayAndRejectsUnsafeScopes(t *testing.T) {
	c := composedConfig(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gateway-only-kube-token" {
			t.Error("gateway credential missing")
		}
		_, _ = io.WriteString(w, "provider reached with verified TLS")
	}))
	defer upstream.Close()
	directory := filepath.Dir(c.Gateway.ProviderTokenFile)
	token, ca := filepath.Join(directory, "kube-token"), filepath.Join(directory, "kube-ca")
	if err := testutil.WriteFile(token, []byte("gateway-only-kube-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: fixtureCertificatePEM, Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c.Gateway.Kubernetes = &KubernetesConfig{UpstreamURL: upstream.URL, TokenFile: token, CAFile: ca, Scope: api.KubernetesReadScope{Namespaces: []string{"ops"}, Resources: []api.KubernetesReadResource{api.KubernetesReadPods, api.KubernetesReadPodLogs}}}
	c.Allowances["kubernetes.read"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 10}}
	policy, err := approvedPolicy(c)
	if err != nil || len(policy.KubernetesRead.Resources) != 2 || len(policy.AllowedCapabilities) != 2 {
		t.Fatalf("approved native scope=%+v err=%v", policy, err)
	}
	certificate, _, _, err := gatewayTLS(c)
	if err != nil {
		t.Fatal(err)
	}
	target := &gatewaydocker.GatewayConfig{Certificates: map[string]tls.Certificate{}}
	if err := nativeServices(c, certificate, target); err != nil {
		t.Fatal(err)
	}
	response, err := target.Kubernetes.Client.Get(upstream.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "provider reached with verified TLS" {
		t.Fatalf("native upstream=%q", body)
	}
	c.Gateway.Kubernetes.Scope.Resources = []api.KubernetesReadResource{"secrets"}
	if _, err := approvedPolicy(c); err == nil {
		t.Fatal("sensitive resource admitted into global native scope")
	}
	c.Gateway.Kubernetes.Scope.Resources = []api.KubernetesReadResource{api.KubernetesReadPods}
	if err := makeTestPathPublic(token); err != nil {
		t.Fatal(err)
	}
	if err := nativeServices(c, certificate, target); err == nil {
		t.Fatal("world-readable gateway credential accepted")
	}
}

func TestGlobalNativeAllowanceRequiresConfiguredService(t *testing.T) {
	c := composedConfig(t)
	c.Allowances["kubernetes.read"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 1}}
	if _, err := approvedPolicy(c); err == nil {
		t.Fatal("capability enabled without trusted upstream configuration")
	}
}

func TestGatewayTLSNamesTheUnreadableFile(t *testing.T) {
	path := filepath.Join(testutil.PrivateDir(t), "not-pem")
	if err := testutil.WriteFile(path, []byte("not PEM"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, set := range map[string]func(*GatewayConfig){
		"gateway.certificateFile": func(g *GatewayConfig) { g.CertificateFile = path },
		"gateway.keyFile":         func(g *GatewayConfig) { g.KeyFile = path },
		"gateway.trustBundleFile": func(g *GatewayConfig) { g.TrustBundleFile = path },
	} {
		broken := composedConfig(t)
		set(&broken.Gateway)
		if _, _, _, err := gatewayTLS(broken); err == nil || !strings.Contains(err.Error(), key+" "+path+": ") {
			t.Errorf("%s: %v", key, err)
		}
	}
}

// Each listener or certificate fault names its own setting.
func TestGatewayTLSNamesTheFailedSetting(t *testing.T) {
	for want, change := range map[string]func(*Config){
		`home: local.gatewayIPv4 "0.0.0.0" is not a specific IPv4 address`: func(c *Config) { c.Local.GatewayIPv4 = "0.0.0.0" },
		`home: gateway.listen "18443" is not HOST:PORT; use 192.168.65.254:18443 or 0.0.0.0:18443`: func(c *Config) {
			c.Gateway.Listen = "18443"
		},
		`home: gateway.listen "0.0.0.0:9" uses port 9, but local.gatewayPort is 18443; make them equal`: func(c *Config) {
			c.Gateway.Listen = "0.0.0.0:9"
		},
		`home: gateway.listen "127.0.0.1:18443" listens on 127.0.0.1, which containers cannot reach; ` +
			`use local.gatewayIPv4 192.168.65.254 or 0.0.0.0`: func(c *Config) { c.Gateway.Listen = "127.0.0.1:18443" },
		"does not name local.gatewayIPv4 192.168.65.253; issue it with that address as an IP subject alternative name": func(c *Config) {
			c.Local.GatewayIPv4, c.Gateway.Listen = "192.168.65.253", "192.168.65.253:18443"
		},
		"does not name api.anthropic.com, the inspection host for local.harness claude-code; " +
			"issue it with that name as a DNS subject alternative name": func(c *Config) { c.Local.Harness = harness.NativeClaude },
	} {
		c := composedConfig(t)
		change(&c)
		if _, _, _, err := gatewayTLS(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q", err, want)
		}
	}
}

type imageFixture struct {
	id        string
	err       error
	noNetwork bool
}

func (f imageFixture) ImageID(context.Context, string) (string, error) { return f.id, f.err }

func (f imageFixture) NetworkExists(context.Context, string) (bool, error) { return !f.noNetwork, nil }

// CheckDocker names the Docker setting that keeps home from reaching its engine.
func TestCheckDockerNamesTheUnusableSetting(t *testing.T) {
	c := composedConfig(t)
	if runtime.GOOS != goosWindows {
		if err := testutil.WriteFile(c.DockerSocket, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	image := c.Local.NativeImageID
	for _, tc := range []struct {
		change func(*Config)
		images imageFixture
		want   string
	}{
		{nil, imageFixture{id: image}, ""},
		{func(c *Config) { c.DockerBinary = filepath.Join(c.Directory, "absent") }, imageFixture{id: image},
			"home: dockerBinary " + filepath.Join(c.Directory, "absent") + " is not a file; set it to the Docker CLI's absolute path"},
		{nil, imageFixture{err: errors.New("exit status 1")}, "home: dockerBinary " + c.DockerBinary +
			" could not inspect local.nativeImageID " + image + " through dockerSocket " + c.DockerSocket +
			" (exit status 1); check that Docker is running, that dockerBinary is the Docker CLI and that the image is loaded"},
		{nil, imageFixture{id: "sha256:" + strings.Repeat("b", 64)}, "home: Docker resolved local.nativeImageID " + image},
		{nil, imageFixture{id: image, noNetwork: true}, "home: local.networkName " + strconv.Quote(c.Local.NetworkName) +
			" does not exist; create it with \"docker network create " + c.Local.NetworkName + "\""},
	} {
		fixed := c
		if tc.change != nil {
			tc.change(&fixed)
		}
		err := (&Service{config: fixed, images: tc.images}).CheckDocker(t.Context())
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)) {
			t.Errorf("got %v, want %q", err, tc.want)
		}
	}
	if runtime.GOOS != goosWindows {
		missing := c
		missing.DockerSocket = filepath.Join(filepath.Dir(c.DockerSocket), "absent.sock")
		err := (&Service{config: missing, images: imageFixture{id: image}}).CheckDocker(t.Context())
		if err == nil || err.Error() != "home: dockerSocket "+missing.DockerSocket+" does not exist; start Docker, "+
			"or set dockerSocket to the engine's Unix socket" {
			t.Errorf("missing socket: %v", err)
		}
	}
}

// A shared gateway certificate names its key and why home still requires it
// to be private, and keeps the privatefs error for the CLI's fix.
func TestSharedCertificateNamesItsSetting(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("file modes do not grant access on Windows")
	}
	c := composedConfig(t)
	if err := os.Chmod(c.Gateway.CertificateFile, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(c, io.Discard)
	want := "home: gateway.certificateFile " + c.Gateway.CertificateFile + " failed the private-file check; " +
		"home applies one owner-only check (yours, not a link, no access for any other account) to every file it reads, including public certificates"
	var unsafe *privatefs.UnsafePathError
	if err == nil || err.Error() != want || !errors.As(err, &unsafe) || unsafe.Path != c.Gateway.CertificateFile {
		t.Fatalf("got %v, want %q", err, want)
	}
}
