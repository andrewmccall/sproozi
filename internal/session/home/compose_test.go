package home

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/harness"

	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
)

const fixtureCertificatePEM = "CERTIFICATE"

func composedConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
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
		if err = os.WriteFile(filepath.Join(root, name), bytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	limits := runtimedocker.ResourceLimits{MemoryBytes: 64 << 20, CPUMilli: 500, PIDs: 64}
	image := "sha256:" + strings.Repeat("a", 64)
	return Config{Directory: filepath.Join(root, "home"), DockerBinary: binary, DockerSocket: filepath.Join(root, "docker.sock"), DockerConfig: filepath.Join(root, "docker-cli"), MaxDuration: "1h", Model: "fixture", Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 10}}}, Local: runtimedocker.PlacementConfig{Harness: harness.NativeCodex, Name: testLocal, NativeImageID: image, HelperImageID: image, AnchorImageID: image, NetworkName: "bridge-fixture", GatewayIPv4: "192.168.65.254", GatewayPort: 18443, Instructions: testInstructions, InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(testInstructions))), AgentLimits: limits, HelperLimits: limits, AnchorLimits: limits, VolumeBytes: 64 << 20, MaxCheckpointBytes: testLimit, RetainedBytes: 32 << 20}, Gateway: GatewayConfig{Listen: "0.0.0.0:18443", CertificateFile: filepath.Join(root, "gateway.crt"), KeyFile: filepath.Join(root, "gateway.key"), TrustBundleFile: filepath.Join(root, "ca.pem"), ProviderURL: "http://127.0.0.1:18765", ProviderTokenFile: filepath.Join(root, "provider-token")}}
}

func TestProductionCompositionTakesExclusiveOwnershipWithoutLaunching(t *testing.T) {
	c := composedConfig(t)
	s, err := Open(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Open(c, io.Discard); err == nil {
		t.Fatal("second home writer admitted")
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
	if err := os.WriteFile(token, []byte("gateway-only-kube-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: fixtureCertificatePEM, Bytes: upstream.Certificate().Raw}), 0600); err != nil {
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
	if err := os.Chmod(token, 0644); err != nil {
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
