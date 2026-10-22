package docker_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/session"
)

const (
	claudeSignIn      = "sk-ant-oat01-local-harness-own-sign-in"
	anthropicHostname = "api.anthropic.com"
)

// Claude Code signed in itself: the local gateway forwards its credential,
// relays its token refresh, and refuses that credential anywhere else.
func TestLocalGatewayForwardsClaudesOwnSignIn(t *testing.T) {
	f := newOwner(t)
	f.launch.Harness = harness.NativeClaude
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 100}
	f.launch.Allowances[modelCapability] = session.Allowance{Units: session.Limit{Bounded: true, Value: 100}}
	var err error
	if f.owner, err = gatewaydocker.Open(f.config); err != nil {
		t.Fatal(err)
	}
	token := f.initialized(t)
	if err = f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+claudeSignIn || r.Header.Get("x-api-key") != "" {
			t.Errorf("the provider got authorization=%q x-api-key=%q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","id":"msg-local","model":"fixture","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer model.Close()
	signIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"sk-ant-oat01-refreshed"}`)
	}))
	defer signIn.Close()
	inspection := inspectionCertificate(t)
	proxy, err := gatewaydocker.NewGateway(f.owner, gatewaydocker.GatewayConfig{Harness: harness.NativeClaude,
		Certificates: map[string]tls.Certificate{anthropicHostname: inspection, "platform.claude.com": inspection, controlHost: inspection},
		MCPAuthority: controlAuthority, AuditLogger: audit.NewLogger(io.Discard),
		Model:  modelgateway.HandlerConfig{UpstreamURL: model.URL, ProviderAuth: modelauth.Workload{}, HTTPClient: model.Client()},
		SignIn: modelgateway.NewAnthropicSignInHandler(signIn.URL, signIn.Client(), audit.NewLogger(io.Discard))})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(proxy)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	proxyURL.User = url.UserPassword("sproozi", token)
	cert, _ := x509.ParseCertificate(inspection.Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(cert)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	call := func(method, target, body string, header map[string]string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, target, strings.NewReader(body))
		for k, v := range header {
			r.Header.Set(k, v)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	messages := call(http.MethodPost, "https://"+anthropicHostname+"/v1/messages",
		`{"model":"fixture","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`,
		map[string]string{"Authorization": "Bearer " + claudeSignIn, "Anthropic-Version": "2023-06-01", "Anthropic-Beta": "oauth-2025-04-20"})
	if messages.StatusCode != http.StatusOK {
		t.Fatalf("model call with the harness's own sign-in: %d", messages.StatusCode)
	}
	refresh := call(http.MethodPost, "https://platform.claude.com/v1/oauth/token", `{"grant_type":"refresh_token"}`,
		map[string]string{"Content-Type": "application/json"})
	if body, _ := io.ReadAll(refresh.Body); refresh.StatusCode != http.StatusOK || !strings.Contains(string(body), "refreshed") {
		t.Fatalf("sign-in refresh: %d %s", refresh.StatusCode, body)
	}
	leaked := call(http.MethodGet, "https://"+controlHost+"/mcp/sproozi?note="+claudeSignIn, "", nil)
	if body, _ := io.ReadAll(leaked.Body); leaked.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "credential_leak_denied") {
		t.Fatalf("the sign-in reached another route: %d %s", leaked.StatusCode, body)
	}
}
