package kubernetes_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

const (
	mcpNamespace      = "demo"
	namespaceArgument = "namespace"
	podsResourceCase  = "pods resource"
	mcpURL            = "https://kubernetes.default.svc/mcp"
	podTool           = "kubernetes_list_pods"
	podList           = `{"kind":"PodList","items":[{"metadata":{"name":"example"}}]}`
)

type mcpFixture struct {
	client         *http.Client
	identity       *gateway.RunIdentity
	calls          atomic.Int32
	revoked        atomic.Bool
	upstreamStatus atomic.Int32
	upstreamBody   string
	auditData      bytes.Buffer
	identities     map[string]*gateway.RunIdentity
	upstreamHook   func(http.ResponseWriter, *http.Request)
}

func newMCPFixture(t *testing.T, configure func(*gateway.RunIdentity)) *mcpFixture {
	t.Helper()
	f := &mcpFixture{upstreamBody: podList}
	f.upstreamStatus.Store(http.StatusOK)
	f.identity = &gateway.RunIdentity{
		Run: &sprooziv1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{UID: types.UID("run-uid")},
			Spec: sprooziv1alpha1.AgentRunSpec{Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead}}},
		Policy: &sprooziv1alpha1.AgentPolicy{Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
			KubernetesRead: sprooziv1alpha1.KubernetesReadScope{Namespaces: []string{mcpNamespace},
				Resources: []sprooziv1alpha1.KubernetesReadResource{sprooziv1alpha1.KubernetesReadPods}},
		}},
	}
	if configure != nil {
		configure(f.identity)
	}
	f.identities = map[string]*gateway.RunIdentity{"run-token": f.identity}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.upstreamHook != nil {
			f.upstreamHook(w, r)
			return
		}
		if r.Method != http.MethodGet ||
			(r.URL.Path != "/api/v1/namespaces/demo/pods" && r.URL.Path != "/api/v1/namespaces/other/pods") {
			t.Errorf("unexpected upstream operation: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("workload credentials reached upstream")
		}
		w.WriteHeader(int(f.upstreamStatus.Load()))
		_, _ = io.WriteString(w, f.upstreamBody)
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := kubernetesgateway.NewHandler(kubernetesgateway.HandlerConfig{
		Upstream: u, Client: upstream.Client(), AuditLogger: audit.NewLogger(&f.auditData),
	})
	dispatcher := gateway.Dispatcher{kubernetesgateway.Authority: {
		Capability: string(sprooziv1alpha1.CapabilityKubernetesRead), Tier: audit.TierSemantic,
		Handler: handler, Budgets: budget.NewTracker(),
	}}
	cert := mcpCertificate(t)
	proxyHandler, err := proxytransport.New(proxytransport.Config{
		Certificates:     map[string]tls.Certificate{"kubernetes.default.svc": cert},
		AllowedAuthority: dispatcher.Allows,
		Authenticate: func(_ context.Context, token string) (*gateway.RunIdentity, error) {
			identity, ok := f.identities[token]
			if !ok {
				return nil, gateway.ErrTokenInvalid
			}
			return identity, nil
		},
		Revalidate: func(context.Context, *gateway.RunIdentity) error {
			if f.revoked.Load() {
				return gateway.ErrRunNotRunning
			}
			return nil
		},
		Handler: dispatcher, RevocationInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewUnstartedServer(proxyHandler)
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("sproozi", "run-token")
	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())
	targetCert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(targetCert)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	f.client = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	return f
}

func mcpCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"kubernetes.default.svc"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (f *mcpFixture) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "sproozi-test", Version: "v1"}, nil).Connect(
		t.Context(), &mcp.StreamableClientTransport{Endpoint: mcpURL, HTTPClient: f.client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestMCPClientOverAuthenticatedProxy(t *testing.T) {
	f := newMCPFixture(t, nil)
	session := f.connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != podTool {
		t.Fatalf("discovery: tools=%v error=%v", tools, err)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
	if err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != podList {
		t.Fatalf("allowed call: result=%v error=%v", result, err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", f.calls.Load())
	}
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"outside namespace", podTool, map[string]any{namespaceArgument: "kube-system"}},
		{"path injection", podTool, map[string]any{namespaceArgument: "demo/../../secrets"}},
		{"missing namespace", podTool, map[string]any{}},
		{"wrong argument type", podTool, map[string]any{namespaceArgument: 42}},
		{"extra argument", podTool, map[string]any{namespaceArgument: mcpNamespace, "resource": "secrets"}},
		{"unknown tool", "kubernetes_delete_pods", map[string]any{namespaceArgument: mcpNamespace}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err == nil && !result.IsError {
				t.Fatalf("forged call succeeded: %v", result)
			}
			if f.calls.Load() != 1 {
				t.Fatal("denied request reached upstream")
			}
		})
	}
	// Reuse the same MCP client and its cached discovery after cancellation.
	f.revoked.Store(true)
	result, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
	if err == nil && !result.IsError {
		t.Fatal("revoked session retained authority")
	}
	if f.calls.Load() != 1 {
		t.Fatal("revoked request reached upstream")
	}
	decoder := json.NewDecoder(&f.auditData)
	var events []audit.Event
	for decoder.More() {
		var event audit.Event
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 2 || !events[0].Allowed || events[1].Allowed {
		t.Fatalf("expected allowed and namespace-denied audit events: %v", events)
	}
	for _, event := range events {
		if event.SemanticLevel != audit.TierSemantic || event.RunUID != "run-uid" || event.Capability != "kubernetes.read" {
			t.Fatalf("incorrect attribution or tier: %v", event)
		}
	}
}

func TestMCPDiscoveryAndCapabilityDenials(t *testing.T) {
	for _, remove := range []string{"requested capability", "policy capability", podsResourceCase} {
		t.Run(remove, func(t *testing.T) {
			f := newMCPFixture(t, func(id *gateway.RunIdentity) {
				switch remove {
				case "requested capability":
					id.Run.Spec.Capabilities = nil
				case "policy capability":
					id.Policy.Spec.AllowedCapabilities = nil
				case podsResourceCase:
					id.Policy.Spec.KubernetesRead.Resources = nil
				}
			})
			if remove == podsResourceCase {
				session := f.connect(t)
				tools, err := session.ListTools(t.Context(), nil)
				if err != nil || len(tools.Tools) != 0 {
					t.Fatalf("ungranted tool exposed: %v %v", tools, err)
				}
				result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
				if err == nil && !result.IsError {
					t.Fatal("undiscovered tool succeeded")
				}
			} else {
				resp := f.post(t, mcpURL, `{}`, nil)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status=%d", resp.StatusCode)
				}
			}
			if f.calls.Load() != 0 {
				t.Fatal("denied request reached upstream")
			}
		})
	}
}

func TestMCPAndRESTShareBudget(t *testing.T) {
	f := newMCPFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
			sprooziv1alpha1.CapabilityKubernetesRead: {MaxUnits: 1},
		}
	})
	session := f.connect(t)
	resp, err := f.client.Get("https://kubernetes.default.svc/api/v1/namespaces/demo/pods")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("REST status=%d", resp.StatusCode)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
	if err == nil && !result.IsError {
		t.Fatal("MCP bypassed exhausted REST budget")
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls=%d", f.calls.Load())
	}
}

func (f *mcpFixture) post(t *testing.T, target, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestMCPBoundaryRejectsUnsupportedRequests(t *testing.T) {
	f := newMCPFixture(t, nil)
	for _, tc := range []struct {
		name, target, body string
		headers            map[string]string
	}{
		{"origin", mcpURL, `{}`, map[string]string{"Origin": "https://attacker.test"}},
		{"query", mcpURL + "?namespace=kube-system", `{}`, nil},
		{"oversized", mcpURL, strings.Repeat(" ", 65<<10), nil},
		{"malformed JSON", mcpURL, `{"jsonrpc":`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.post(t, tc.target, tc.body, tc.headers)
			if resp.StatusCode < 400 {
				t.Fatalf("status=%d", resp.StatusCode)
			}
		})
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
}

func TestMCPRedactsUpstreamFailures(t *testing.T) {
	f := newMCPFixture(t, nil)
	f.upstreamStatus.Store(http.StatusInternalServerError)
	f.upstreamBody = "sensitive-upstream-error"
	result, err := f.connect(t).CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || strings.Contains(fmt.Sprint(result), f.upstreamBody) {
		t.Fatalf("unsafe result: %v", result)
	}
	data, err := json.Marshal(result)
	if err != nil || strings.Contains(string(data), f.upstreamBody) {
		t.Fatalf("error content leaked: %s %v", data, err)
	}
}

func TestMCPBoundsResponse(t *testing.T) {
	f := newMCPFixture(t, nil)
	f.upstreamBody = strings.Repeat("x", (8<<20)+1)
	result, err := f.connect(t).CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
	if err != nil || !result.IsError {
		t.Fatalf("unbounded response: %v %v", result, err)
	}
}

func TestMCPRevocationCancelsInflightUpstream(t *testing.T) {
	f := newMCPFixture(t, nil)
	started, stopped := make(chan struct{}), make(chan struct{})
	f.upstreamHook = func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-time.After(5 * time.Second):
		}
	}
	session := f.connect(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = session.CallTool(t.Context(), &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	f.revoked.Store(true)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not cancel upstream work")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked MCP call did not finish")
	}
}

func TestMCPDoesNotShareRunAuthorityOrBudgets(t *testing.T) {
	f := newMCPFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
			sprooziv1alpha1.CapabilityKubernetesRead: {MaxUnits: 1},
		}
	})
	other := &gateway.RunIdentity{Run: f.identity.Run.DeepCopy(), Policy: f.identity.Policy.DeepCopy()}
	other.Run.UID = "other-run"
	other.Policy.Spec.KubernetesRead.Namespaces = []string{"other"}
	f.identities["other-token"] = other
	first := f.connect(t)
	args := &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: mcpNamespace}}
	result, err := first.CallTool(t.Context(), args)
	if err != nil || result.IsError {
		t.Fatalf("first run: %v %v", result, err)
	}
	// A forged MCP session ID cannot select another run. Only proxy identity can.
	f.client.Transport.(*http.Transport).CloseIdleConnections()
	transport := f.client.Transport.(*http.Transport).Clone()
	proxyURL, err := transport.Proxy(&http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	proxyCopy := *proxyURL
	proxyCopy.User = url.UserPassword("sproozi", "other-token")
	transport.Proxy = http.ProxyURL(&proxyCopy)
	t.Cleanup(transport.CloseIdleConnections)
	f.client = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	second := f.connect(t)
	result, err = second.CallTool(t.Context(), args)
	if err == nil && !result.IsError {
		t.Fatal("second run inherited first run's namespace authority")
	}
	otherArgs := &mcp.CallToolParams{Name: podTool, Arguments: map[string]any{namespaceArgument: "other"}}
	result, err = second.CallTool(t.Context(), otherArgs)
	if err != nil || result.IsError {
		t.Fatalf("second run inherited first budget: %v %v", result, err)
	}
	result, err = first.CallTool(t.Context(), args)
	if err == nil && !result.IsError {
		t.Fatal("first run escaped its exhausted budget")
	}
	if f.calls.Load() != 2 {
		t.Fatalf("calls=%d", f.calls.Load())
	}
	resp := f.post(t, mcpURL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kubernetes_list_pods","arguments":{"namespace":"demo"}}}`,
		map[string]string{"Mcp-Session-Id": "run-uid"})
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"isError":true`) || f.calls.Load() != 2 {
		t.Fatalf("session ID selected another run: %s", body)
	}
}

func TestMCPStripsWorkloadCredentials(t *testing.T) {
	f := newMCPFixture(t, nil)
	resp := f.post(t, mcpURL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kubernetes_list_pods","arguments":{"namespace":"demo"}}}`,
		map[string]string{"Authorization": "Bearer workload-secret", "Proxy-Authorization": "Bearer forged", "Cookie": "provider-secret"})
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || strings.Contains(string(data), `"isError":true`) || f.calls.Load() != 1 {
		t.Fatalf("call failed: %s %v", data, err)
	}
	if strings.Contains(f.auditData.String(), "secret") || strings.Contains(f.auditData.String(), podList) {
		t.Fatal("audit recorded workload credentials or Pod content")
	}
}
