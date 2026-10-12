package docker_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/session"
)

const trustedProviderKey = "trusted-provider-key"

func inspectionCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.openai.com", "api.anthropic.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestLocalGatewayUsesProviderCredentialsAndDurableAllocation(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) { testLocalGatewayAllocation(t, kind) })
	}
}

func testLocalGatewayAllocation(t *testing.T, kind harness.NativeKind) {
	t.Helper()
	authority, requestBody, responseBody := "api.openai.com", `{"model":"fixture","input":"controlled turn","max_output_tokens":1}`, `{"usage":{"total_tokens":1}}`
	auth := modelauth.Authenticator(modelauth.APIKey{Key: trustedProviderKey})
	if kind == harness.NativeClaude {
		authority = "api.anthropic.com"
		requestBody = `{"model":"fixture","messages":[{"role":"user","content":"controlled turn"}],"max_tokens":1}`
		responseBody = `{"type":"message","id":"msg-local","model":"fixture","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":1}}`
		auth = modelauth.AnthropicAPIKey{Key: trustedProviderKey}
	}
	f := newOwner(t)
	f.launch.Harness = kind
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 1}
	f.launch.Allowances[modelCapability] = session.Allowance{Units: session.Limit{Bounded: true, Value: 1}}
	var err error
	f.owner, err = gatewaydocker.Open(f.config)
	if err != nil {
		t.Fatal(err)
	}
	token := f.initialized(t)
	if err = f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		validAuth := r.Header.Get("Authorization") == "Bearer trusted-provider-key"
		if kind == harness.NativeClaude {
			validAuth = r.Header.Get("x-api-key") == trustedProviderKey && r.Header.Get("Authorization") == ""
		}
		if !validAuth || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("gateway forwarded worker credentials or omitted provider authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()
	inspection := inspectionCertificate(t)
	proxy, err := gatewaydocker.NewGateway(f.owner, gatewaydocker.GatewayConfig{Harness: kind, Certificates: map[string]tls.Certificate{authority: inspection}, Model: modelgateway.HandlerConfig{UpstreamURL: upstream.URL, ProviderAuth: auth, HTTPClient: upstream.Client()}, AuditLogger: audit.NewLogger(io.Discard)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(proxy)
	defer server.Close()
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("sproozi", token)
	cert, err := x509.ParseCertificate(inspection.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(cert)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	endpoint := "https://" + authority + "/v1/responses"
	if kind == harness.NativeClaude {
		endpoint = "https://" + authority + "/v1/messages?beta=true"
	}
	for _, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		r, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer worker-placeholder")
		r.Header.Set("x-api-key", "worker-placeholder")
		if kind == harness.NativeClaude {
			r.Header.Set("Anthropic-Version", "2023-06-01")
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("status=%d want=%d", response.StatusCode, want)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("forwarded %d requests", calls.Load())
	}
	if err = f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	usage, err := f.owner.Seal(t.Context(), f.launch, f.instance)
	if err != nil || usage[modelCapability].Units != 1 {
		t.Fatalf("sealed actual provider usage=%+v err=%v", usage, err)
	}
}
