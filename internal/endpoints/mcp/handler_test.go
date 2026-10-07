package mcp_test

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	remote "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

const (
	docsCapability    = "mcp.docs"
	docsName          = "docs"
	inventoryName     = "inventory"
	fixtureVersion    = "test"
	collectionField   = "collection"
	testQuery         = "DNS"
	privateCollection = "private"
	docsPath          = "/mcp/docs"
	otherCollection   = "other-team"
	searchTool        = "search"
)

const (
	approvedCollection = "operations"
	queryField         = "query"
	clientName         = "ordinary-client"
	browserOrigin      = "https://browser.example"
	providerDiagnostic = "provider diagnostic"
)

const (
	inventoryTool   = "list_assets"
	schemaTypeField = "type"
	schemaRefField  = "$ref"
)

const (
	tokenFileField       = "bearerTokenFile"
	registryServersField = "servers"
)

const registryURLField = "url"

const fixtureHost = "sproozi-gateway.sproozi-system.svc"

type docsArguments struct {
	Collection string `json:"collection"`
	Query      string `json:"query"`
}
type inventoryArguments struct {
	Tenant string `json:"tenant"`
	Kind   string `json:"kind"`
}
type recordedAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (a *recordedAudit) Log(event audit.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
}
func (a *recordedAudit) data() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, _ := json.Marshal(a.events)
	return string(raw)
}

type fixture struct {
	client        *http.Client
	otherClient   *http.Client
	identity      *gateway.RunIdentity
	otherIdentity *gateway.RunIdentity
	revoked       atomic.Bool
	calls         atomic.Int32
	requests      atomic.Int32
	deletes       atomic.Int32
	registry      []byte
	audit         recordedAudit
	block         atomic.Bool
	toolError     atomic.Bool
	largeResponse atomic.Bool
	docsServer    *sdk.Server
	deleted       chan struct{}
	started       chan struct{}
	cancelled     chan struct{}
}

func newFixture(t *testing.T, change func(*gateway.RunIdentity)) *fixture {
	t.Helper()
	f := &fixture{started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1), deleted: make(chan struct{}, 64)}
	f.identity = &gateway.RunIdentity{Run: &api.AgentRun{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("remote-run")},
		Spec:       api.AgentRunSpec{Capabilities: []api.CapabilityKind{docsCapability, "mcp.inventory"}},
	}, Policy: &api.AgentPolicy{Spec: api.AgentPolicySpec{
		AllowedCapabilities: []api.CapabilityKind{docsCapability, "mcp.inventory"},
		MCPServers: map[string]api.MCPServerScope{
			docsName:      {Tools: map[string]api.MCPToolScope{searchTool: {Arguments: &runtime.RawExtension{Raw: []byte(`{"type":"object","required":["collection"],"properties":{"collection":{"const":"operations"}}}`)}}}},
			inventoryName: {Tools: map[string]api.MCPToolScope{inventoryTool: {Arguments: &runtime.RawExtension{Raw: []byte(`{"type":"object","required":["tenant"],"properties":{"tenant":{"const":"home-ops"}}}`)}}}},
		},
	}}}
	if change != nil {
		change(f.identity)
	}
	f.otherIdentity = &gateway.RunIdentity{Run: f.identity.Run.DeepCopy(), Policy: f.identity.Policy.DeepCopy()}
	f.otherIdentity.Run.UID = "second-run"
	f.otherIdentity.Policy.Spec.MCPServers[docsName] = api.MCPServerScope{Tools: map[string]api.MCPToolScope{searchTool: {Arguments: &runtime.RawExtension{Raw: []byte(`{"required":["collection"],"properties":{"collection":{"const":"other-team"}}}`)}}}}
	registry := map[string]any{}
	credentialDir := t.TempDir()
	tokenFiles := make([]string, 0, 2)
	roots := x509.NewCertPool()
	for _, name := range []string{docsName, inventoryName} {
		server := sdk.NewServer(&sdk.Implementation{Name: name, Version: fixtureVersion}, nil)
		if name == docsName {
			f.docsServer = server
			sdk.AddTool(server, &sdk.Tool{Name: searchTool, Description: "Search a collection", InputSchema: map[string]any{
				schemaTypeField: "object", "$defs": map[string]any{"text": map[string]any{schemaTypeField: "string"}},
				"required": []string{collectionField, queryField}, "properties": map[string]any{
					collectionField: map[string]any{schemaRefField: "#/$defs/text"}, queryField: map[string]any{schemaRefField: "#/$defs/text"},
				},
			}},
				func(ctx context.Context, _ *sdk.CallToolRequest, args docsArguments) (*sdk.CallToolResult, any, error) {
					f.calls.Add(1)
					if f.toolError.Load() {
						return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "docs-credential in provider diagnostic"}}}, nil, nil
					}
					if f.largeResponse.Load() {
						return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.Repeat("x", (8<<20)+1)}}}, nil, nil
					}
					if f.block.Load() {
						f.started <- struct{}{}
						select {
						case <-ctx.Done():
							f.cancelled <- struct{}{}
							return nil, nil, ctx.Err()
						case <-t.Context().Done():
							// A failed assertion must not strand fixture cleanup.
							return nil, nil, t.Context().Err()
						}
					}
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: args.Collection + ": outage-guide for " + args.Query}}}, nil, nil
				})
		} else {
			sdk.AddTool(server, &sdk.Tool{Name: inventoryTool},
				func(_ context.Context, _ *sdk.CallToolRequest, args inventoryArguments) (*sdk.CallToolResult, any, error) {
					f.calls.Add(1)
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: args.Tenant + ": rack-a-01 " + args.Kind}}}, nil, nil
				})
		}
		sdk.AddTool(server, &sdk.Tool{Name: "delete_everything"},
			func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
				f.calls.Add(1)
				return &sdk.CallToolResult{}, nil, nil
			})
		// Stateful official servers require private SDK session IDs. They are
		// created and deleted independently for each mediated HTTP request.
		endpoint := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.requests.Add(1)
			if r.Method == http.MethodDelete {
				f.deletes.Add(1)
				f.deleted <- struct{}{}
			}
			if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer "+name+"-credential" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
				t.Error("upstream request did not retain its exact isolated provider identity")
			}
			endpoint.ServeHTTP(w, r)
		}))
		t.Cleanup(upstream.Close)
		roots.AddCert(upstream.Certificate())
		tokenFile := filepath.Join(credentialDir, name+".token")
		if err := os.WriteFile(tokenFile, []byte(name+"-credential"), 0600); err != nil {
			t.Fatal(err)
		}
		tokenFiles = append(tokenFiles, tokenFile)
		registry[name] = map[string]string{registryURLField: upstream.URL + "/mcp", tokenFileField: tokenFile}
	}
	f.registry, _ = json.Marshal(map[string]any{registryServersField: registry})
	registrations, err := remote.LoadRegistry(f.registry, credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	// Rotation cannot mix a new credential with the old startup registration.
	for _, path := range tokenFiles {
		if err := os.WriteFile(path, []byte("rotated-credential"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	upstreamTransport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(upstreamTransport.CloseIdleConnections)
	handler := remote.NewHandler(remote.HandlerConfig{Registry: registrations, HTTPClient: &http.Client{Transport: upstreamTransport}, Budgets: budget.NewTracker(), AuditLogger: &f.audit})
	dispatch := gateway.Dispatcher{fixtureHost + ":8443": {Tier: audit.TierProtocol, Handler: handler}}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{fixtureHost}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	proxyHandler, err := proxytransport.New(proxytransport.Config{
		Certificates: map[string]tls.Certificate{fixtureHost: cert}, AllowedAuthority: dispatch.Allows,
		Authenticate: func(_ context.Context, token string) (*gateway.RunIdentity, error) {
			if token == "second-token" {
				return f.otherIdentity, nil
			}
			return f.identity, nil
		},
		Revalidate: func(context.Context, *gateway.RunIdentity) error {
			if f.revoked.Load() {
				return gateway.ErrRunNotRunning
			}
			return nil
		},
		Handler: dispatch, RevocationInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewUnstartedServer(proxyHandler)
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("sproozi", "run-token")
	roots.AddCert(proxy.Certificate())
	target, _ := x509.ParseCertificate(der)
	roots.AddCert(target)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	f.client = &http.Client{Transport: transport, Timeout: 10 * time.Second}
	secondURL := *proxyURL
	secondURL.User = url.UserPassword("sproozi", "second-token")
	secondTransport := transport.Clone()
	secondTransport.Proxy = http.ProxyURL(&secondURL)
	t.Cleanup(secondTransport.CloseIdleConnections)
	f.otherClient = &http.Client{Transport: secondTransport, Timeout: 10 * time.Second}
	return f
}

func (f *fixture) connect(t *testing.T, server string) *sdk.ClientSession {
	t.Helper()
	session, err := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: fixtureVersion}, nil).Connect(
		t.Context(), &sdk.StreamableClientTransport{Endpoint: "https://" + fixtureHost + ":8443/mcp/" + server, HTTPClient: f.client, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestConfiguredServersUseDiscoveredSchemasAndPolicyScopes(t *testing.T) {
	f := newFixture(t, nil)
	for _, tc := range []struct {
		server, tool, text string
		args               map[string]any
	}{
		{docsName, searchTool, "operations: outage-guide for DNS", map[string]any{collectionField: approvedCollection, queryField: testQuery}},
		{inventoryName, inventoryTool, "home-ops: rack-a-01 server", map[string]any{"tenant": "home-ops", "kind": "server"}},
	} {
		t.Run(tc.server, func(t *testing.T) {
			session := f.connect(t, tc.server)
			tools, err := session.ListTools(t.Context(), nil)
			if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != tc.tool {
				t.Fatalf("filtered discovery failed: %v %v", tools, err)
			}
			result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].(*sdk.TextContent).Text != tc.text {
				t.Fatalf("configured call: %v %v", result, err)
			}
		})
	}
	if f.calls.Load() != 2 || f.deletes.Load() == 0 {
		t.Fatalf("calls=%d session deletes=%d", f.calls.Load(), f.deletes.Load())
	}
	data := f.audit.data()
	if !strings.Contains(data, `"semanticLevel":"protocol"`) || !strings.Contains(data, `"capability":"mcp.docs"`) || strings.Contains(data, "credential") || strings.Contains(data, "outage-guide") || strings.Contains(data, testQuery) {
		t.Fatalf("missing or sensitive audit: %s", data)
	}
}

func TestMCPScopesAndBudgetsCannotBeBypassedByCachedDiscoveryOrReconnect(t *testing.T) {
	f := newFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{docsCapability: {MaxUnits: 1}}
	})
	session := f.connect(t, docsName)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{searchTool, map[string]any{collectionField: privateCollection, queryField: testQuery}},
		{searchTool, map[string]any{collectionField: approvedCollection, queryField: 42}},
		{searchTool, map[string]any{collectionField: approvedCollection}},
		{"delete_everything", map[string]any{}},
	} {
		result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err == nil && !result.IsError {
			t.Fatalf("unauthorized tool call succeeded: %v", tc)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatalf("denials reached tools: %d", f.calls.Load())
	}
	session = f.connect(t, docsName) // HTTP permission errors may close an SDK session.
	args := map[string]any{collectionField: approvedCollection, queryField: testQuery}
	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: args})
	if err != nil || result.IsError || result.Content[0].(*sdk.TextContent).Text != "operations: outage-guide for DNS" {
		t.Fatalf("first budgeted result: %v %v", result, err)
	}
	second := f.connect(t, docsName)
	result, err = second.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: args})
	if err == nil && !result.IsError {
		t.Fatal("reconnect reset call budget")
	}
	if f.calls.Load() != 1 {
		t.Fatalf("budget allowed excess calls: %d", f.calls.Load())
	}
}

func TestMCPRevocationCancelsRemoteToolAndDeletesItsSession(t *testing.T) {
	f := newFixture(t, nil)
	session := f.connect(t, docsName)
	f.block.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: map[string]any{collectionField: approvedCollection, queryField: testQuery}})
		done <- err
	}()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream tool did not start")
	}
	for len(f.deleted) > 0 {
		<-f.deleted
	}
	f.revoked.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked client retained call authority")
	}
	select {
	case <-f.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream tool outlived revocation")
	}
	select {
	case <-f.deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked request retained its upstream session")
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls=%d", f.calls.Load())
	}
}

func TestMCPRequestBoundaryRejectsForgeryBeforeUpstreamContact(t *testing.T) {
	f := newFixture(t, func(id *gateway.RunIdentity) { id.Run.Spec.Capabilities = []api.CapabilityKind{docsCapability} })
	for _, tc := range []struct {
		path, body string
		headers    map[string]string
		status     int
	}{
		{"/mcp/inventory", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil, 403},
		{"/mcp/docs?secret=private", `{}`, nil, 403},
		{docsPath, `{}`, map[string]string{"Origin": browserOrigin}, 403},
		{docsPath, `{"method":"ping","method":"tools/call"}`, nil, 400},
		{docsPath, strings.Repeat("a", (64<<10)+1), nil, 413},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+fixtureHost+":8443"+tc.path, bytes.NewBufferString(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range tc.headers {
			req.Header.Set(key, value)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("status=%d wanted=%d", resp.StatusCode, tc.status)
		}
	}
	if f.requests.Load() != 0 {
		t.Fatalf("forgery reached upstream: %d", f.requests.Load())
	}
}

func TestRegistryRejectsUnsafeConfiguration(t *testing.T) {
	for _, raw := range []string{
		`{"servers":{"docs":{"url":"http://unsafe.example/mcp"}}}`,
		`{"servers":{"docs":{"url":"https://user:secret@unsafe.example/mcp"}}}`,
		`{"servers":{"docs":{"url":"https://safe.example/mcp?key=secret"}}}`,
		`{"servers":{"docs":{"url":"https://safe.example/mcp","headers":{"Authorization":"secret"}}}}`,
		`{"servers":{"docs":{"url":"https://safe.example/mcp","url":"https://other.example/mcp"}}}`,
	} {
		if _, err := remote.LoadRegistry([]byte(raw), remote.CredentialDirectory); err == nil {
			t.Fatal("unsafe registration accepted")
		}
	}
	registry, err := remote.LoadRegistry([]byte(`{"servers":{"docs":{"url":"https://safe.example/mcp"}}}`), remote.CredentialDirectory)
	if err != nil || fmt.Sprint(registry.Authorities()) != "[safe.example:443]" {
		t.Fatalf("registered authority: %v %v", registry, err)
	}
}

func TestRunsSharingProviderRetainDistinctScopesAndBudgets(t *testing.T) {
	f := newFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{docsCapability: {MaxUnits: 1}}
	})
	first := f.connect(t, docsName)
	second, err := sdk.NewClient(&sdk.Implementation{Name: "second-client", Version: fixtureVersion}, nil).Connect(t.Context(),
		&sdk.StreamableClientTransport{Endpoint: "https://" + fixtureHost + ":8443/mcp/docs", HTTPClient: f.otherClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	for _, tc := range []struct {
		session    *sdk.ClientSession
		collection string
		allowed    bool
	}{
		{first, approvedCollection, true}, {second, approvedCollection, false}, {second, otherCollection, true}, {first, otherCollection, false},
		{first, approvedCollection, false}, {second, otherCollection, false},
	} {
		result, err := tc.session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: map[string]any{collectionField: tc.collection, queryField: testQuery}})
		allowed := err == nil && !result.IsError
		if allowed != tc.allowed {
			t.Fatalf("collection=%s allowed=%v result=%v err=%v", tc.collection, allowed, result, err)
		}
	}
	if f.calls.Load() != 2 {
		t.Fatalf("cross-run leakage or shared budget: calls=%d", f.calls.Load())
	}
}

func TestLocalPolicyReferencesRemainRootedAfterSchemaIntersection(t *testing.T) {
	f := newFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.MCPServers[docsName] = api.MCPServerScope{Tools: map[string]api.MCPToolScope{searchTool: {Arguments: &runtime.RawExtension{Raw: []byte(`{"$defs":{"collection":{"const":"operations"}},"required":["collection"],"properties":{"collection":{"$ref":"#/$defs/collection"}}}`)}}}}
	})
	session := f.connect(t, docsName)
	for _, collection := range []string{approvedCollection, privateCollection} {
		result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: map[string]any{collectionField: collection, queryField: testQuery}})
		if (err == nil && !result.IsError) != (collection == approvedCollection) {
			t.Fatalf("local scope reference: %v %v", result, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("scope reference bypass: calls=%d", f.calls.Load())
	}
}

func TestPolicyDialectRetainsItsOwnArgumentMeaning(t *testing.T) {
	f := newFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.MCPServers[docsName] = api.MCPServerScope{Tools: map[string]api.MCPToolScope{searchTool: {Arguments: &runtime.RawExtension{Raw: []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"tenants":{"type":"array","items":[{"const":"approved"}],"additionalItems":false}}}`)}}}}
	})
	session := f.connect(t, docsName)
	for _, tc := range []struct {
		tenants []string
		allowed bool
	}{{[]string{"approved"}, true}, {[]string{privateCollection, "another-private"}, false}} {
		result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: map[string]any{collectionField: approvedCollection, queryField: testQuery, "tenants": tc.tenants}})
		if (err == nil && !result.IsError) != tc.allowed {
			t.Fatalf("policy dialect changed authorization: %v %v", result, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("draft-07 restriction bypassed: calls=%d", f.calls.Load())
	}
}

func TestProviderDiagnosticsNeverReachClientOrAudit(t *testing.T) {
	f := newFixture(t, nil)
	f.toolError.Store(true)
	session := f.connect(t, docsName)
	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: map[string]any{collectionField: approvedCollection, queryField: testQuery}})
	if err == nil && !result.IsError {
		t.Fatal("provider failure returned success")
	}
	output := fmt.Sprint(result, err) + f.audit.data()
	if strings.Contains(output, "docs-credential") || strings.Contains(output, providerDiagnostic) {
		t.Fatal("upstream failure leaked diagnostic content")
	}
	if f.calls.Load() != 1 {
		t.Fatal("provider failure test did not execute tool")
	}
}

func TestUnsupportedProviderSchemaFailsBeforeToolExecution(t *testing.T) {
	f := newFixture(t, nil)
	f.docsServer.AddTool(&sdk.Tool{Name: searchTool, InputSchema: map[string]any{schemaTypeField: "object", schemaRefField: "#"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			f.calls.Add(1)
			return &sdk.CallToolResult{}, nil
		})
	_, err := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: fixtureVersion}, nil).Connect(t.Context(),
		&sdk.StreamableClientTransport{Endpoint: "https://" + fixtureHost + ":8443/mcp/docs", HTTPClient: f.client, DisableStandaloneSSE: true}, nil)
	if err == nil {
		t.Fatal("cyclic provider schema accepted")
	}
	if f.calls.Load() != 0 {
		t.Fatal("unsupported provider schema reached tool execution")
	}
	if f.deletes.Load() == 0 {
		t.Fatal("schema failure retained its upstream session")
	}
}

func TestRegistryCredentialsStayWithinDedicatedSecretMount(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "gateway-token")
	inside := filepath.Join(root, "token")
	for _, path := range []string{outside, inside} {
		if err := os.WriteFile(path, []byte("provider-token"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	allowedLink := filepath.Join(root, "projected-token")
	if err := os.Symlink(inside, allowedLink); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path    string
		allowed bool
	}{{outside, false}, {escape, false}, {inside, true}, {allowedLink, true}} {
		raw, _ := json.Marshal(map[string]any{registryServersField: map[string]any{docsName: map[string]string{registryURLField: "https://safe.example/mcp", tokenFileField: tc.path}}})
		_, err := remote.LoadRegistry(raw, root)
		if (err == nil) != tc.allowed {
			t.Fatalf("credential containment allowed=%v err=%v", tc.allowed, err)
		}
	}
}

func TestRegistryCanonicalizesEquivalentProviderAuthorities(t *testing.T) {
	for _, endpoint := range []string{"https://BÜCHER.example.:0443/mcp", "https://xn--bcher-kva.example/mcp"} {
		raw, _ := json.Marshal(map[string]any{registryServersField: map[string]any{docsName: map[string]string{registryURLField: endpoint}}})
		registry, err := remote.LoadRegistry(raw, remote.CredentialDirectory)
		if err != nil || fmt.Sprint(registry.Authorities()) != "[xn--bcher-kva.example:443]" {
			t.Fatalf("provider alias reservation: %v %v", registry, err)
		}
	}
}

func TestOversizedProviderResultFailsAndRetainsAttemptBudget(t *testing.T) {
	f := newFixture(t, func(id *gateway.RunIdentity) {
		id.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{docsCapability: {MaxUnits: 1}}
	})
	session := f.connect(t, docsName)
	f.largeResponse.Store(true)
	arguments := map[string]any{collectionField: approvedCollection, queryField: testQuery}
	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: arguments})
	if err == nil && !result.IsError {
		t.Fatal("oversized provider result passed response bounds")
	}
	f.largeResponse.Store(false)
	second := f.connect(t, docsName)
	result, err = second.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: arguments})
	if err == nil && !result.IsError {
		t.Fatal("failed upstream attempt was not charged")
	}
	if f.calls.Load() != 1 {
		t.Fatalf("failed attempt reset budget: calls=%d", f.calls.Load())
	}
}
