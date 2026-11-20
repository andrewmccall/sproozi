// Package routetest checks what every composed gateway route promises
// (ADR 0070): a valid admission, a refused and recorded request without its
// capability, a served request that spends a one-unit budget unless the route
// is free by a recorded decision, and model authorities served only from the
// provider table. Both gateway compositions run it from their tests, with
// handlers built on Client.
package routetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// MCPServer is the named MCP server MCPRegistry registers, with one tool.
const (
	MCPServer     = "docs"
	mcpCapability = api.CapabilityKind("mcp." + MCPServer)
	mcpTool       = "search"
	mcpEndpoint   = "https://docs.mcp.example/mcp"
	// Repository, Namespace and EgressProfile are what Grant permits.
	Repository    = "owner/repo"
	Namespace     = "demo"
	EgressProfile = "registry"
	// EgressHost is the destination EgressProfile approves.
	EgressHost = "registry.example"
	// ProviderCredential is the harness's own credential on each model request.
	ProviderCredential = "sk-workload-fixture-credential"
	packageWheel       = "demo_tool-1.0.0-py3-none-any.whl"
)

// Free lists the handlers that charge nothing by decision, with the record
// that decided it. A served request on any other route must spend.
var Free = map[string]string{
	"*model.Relay": "ADR 0047: the sign-in and account relays charge no budget",
}

// FreePaths lists free requests on a route that otherwise spends.
var FreePaths = map[string]string{
	"/mcp/sproozi": "ADR 0025: the control connection needs no capability and is unmetered",
}

// Recorder keeps every audit event the composed handlers write.
type Recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *Recorder) Log(e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Take returns and clears the events written so far.
func (r *Recorder) Take() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// Client answers every upstream a composition reaches, in process: the
// Kubernetes API, git, PyPI, the egress destination, each model provider and
// the named MCP server.
func Client(t testing.TB) *http.Client {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: MCPServer, Version: "test"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: mcpTool, Description: "Search"},
		func(context.Context, *sdk.CallToolRequest, struct {
			Query string `json:"query"`
		}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "found"}}}, nil, nil
		})
	mcp := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		switch host := r.URL.Hostname(); {
		case host == "docs.mcp.example":
			mcp.ServeHTTP(recorder, r)
		case host == "kubernetes.default.svc" || strings.HasPrefix(host, "kubernetes."):
			reply(recorder, "application/json", `{"kind":"PodList","items":[]}`)
		case host == "github.com" || host == "api.github.com":
			reply(recorder, "application/x-git-upload-pack-advertisement", "001e# service=git-upload-pack\n0000")
		case host == "pypi.org" || host == "files.pythonhosted.org":
			recorder.WriteHeader(http.StatusNotFound)
		case host == EgressHost:
			reply(recorder, "text/plain", "ok")
		case strings.HasSuffix(r.URL.Path, "/messages"):
			reply(recorder, "application/json", `{"type":"message","role":"assistant","id":"m","model":"claude-fixture",`+
				`"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			reply(recorder, "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\","+
				"\"usage\":{\"total_tokens\":1,\"input_tokens\":0,\"output_tokens\":1}}}\n\n")
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			reply(recorder, "application/json", `{"id":"c","object":"chat.completion","choices":[],`+
				`"usage":{"prompt_tokens":0,"completion_tokens":1,"total_tokens":1}}`)
		default:
			recorder.WriteHeader(http.StatusTeapot)
		}
		response := recorder.Result()
		response.Request = r
		return response, nil
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer func() { _ = r.Body.Close() }()
	}
	return f(r)
}

func reply(w http.ResponseWriter, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	_, _ = io.WriteString(w, body)
}

// MCPRegistry registers MCPServer at the endpoint Client serves.
func MCPRegistry(t testing.TB) *mcpgateway.Registry {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "docs.token")
	if err := os.WriteFile(token, []byte("docs-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"servers": map[string]any{
		MCPServer: map[string]string{"url": mcpEndpoint, "bearerTokenFile": token}}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := mcpgateway.LoadRegistry(data, dir)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// TokenProvider is a GitHub App token provider for the composed GitHub handler.
type TokenProvider struct{}

func (TokenProvider) Token(context.Context) (string, error) { return "ghs_fixture", nil }

var allCapabilities = []api.CapabilityKind{api.CapabilityKubernetesRead, api.CapabilityGitHubPullRequest,
	api.CapabilityPackagesInstall, api.CapabilityNetworkEgress, api.CapabilityModelInference, mcpCapability}

// Grant permits every capability the composed routes serve except without,
// and gives budgeted a one-unit budget.
func Grant(uid string, without, budgeted api.CapabilityKind) *gateway.ExecutionGrant {
	capabilities := slices.DeleteFunc(slices.Clone(allCapabilities), func(c api.CapabilityKind) bool { return c == without })
	g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents",
		Name: "run", UID: uid},
		Policy: gateway.PolicySnapshot{Name: "contract-policy", Spec: api.AgentPolicySpec{
			AllowedCapabilities: slices.Clone(allCapabilities),
			KubernetesRead: api.KubernetesReadScope{Namespaces: []string{Namespace},
				Resources: []api.KubernetesReadResource{api.KubernetesReadPods}},
			GitHubPullRequest: api.GitHubPullRequestScope{Repositories: []string{Repository}},
			PackagesInstall: api.PackagesInstallScope{Ecosystems: []api.PackageEcosystem{api.PackageEcosystemPyPI},
				MaxDownloadBytes: 1024, Artifacts: []api.PackageArtifact{{Name: "demo-tool", Version: "1.0.0",
					Filename: packageWheel, SHA256: strings.Repeat("a", 64)}}},
			EgressProfiles: []string{EgressProfile},
			MCPServers:     map[string]api.MCPServerScope{MCPServer: {Tools: map[string]api.MCPToolScope{mcpTool: {}}}},
		}},
		Template:     gateway.TemplateSnapshot{Name: "contract-template", Spec: api.AgentTemplateSpec{EgressProfiles: []string{EgressProfile}}},
		Capabilities: capabilities}
	if budgeted != "" {
		g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{budgeted: {MaxUnits: 1}}
	}
	return g
}

// request is one served request for a route.
type request struct {
	method, path, body string
	capability         api.CapabilityKind
}

// servedRequest is the request that a route's handler serves, by handler type
// and authority. A new handler type must be added here, which decides how it
// is served and checks that it spends.
func servedRequest(authority string, route gateway.Route) (request, bool) {
	host := strings.TrimSuffix(authority, ":443")
	switch fmt.Sprintf("%T", route.Handler) {
	case "*kubernetes.Handler":
		return request{method: http.MethodGet, path: "/api/v1/namespaces/" + Namespace + "/pods"}, true
	case "*github.Handler":
		return request{method: http.MethodGet, path: "/" + Repository + ".git/info/refs?service=git-upload-pack"}, true
	case "*packages.Handler":
		if host == "files.pythonhosted.org" {
			return request{method: http.MethodGet, path: "/packages/f/" + packageWheel}, true
		}
		return request{method: http.MethodGet, path: "/simple/demo-tool/"}, true
	case "*destination.Handler":
		return request{method: http.MethodGet, path: "/package"}, true
	case "*model.Handler":
		if authority == modelgateway.AnthropicAuthority {
			// An image by URL leaves the call unsizable, so it reserves what
			// remains of a one-unit budget instead of its size.
			return request{method: http.MethodPost, path: "/v1/messages", body: `{"model":"claude-fixture","max_tokens":1,` +
				`"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]}]}`}, true
		}
		return request{method: http.MethodPost, path: "/v1/chat/completions",
			body: `{"model":"gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`}, true
	case "*model.copilot":
		return request{method: http.MethodPost, path: "/chat/completions",
			body: `{"model":"gpt-fixture","messages":[{"role":"user","content":"hi"}]}`}, true
	case "*model.chatGPTBackend":
		return request{method: http.MethodPost, path: "/backend-api/codex/responses",
			body: `{"model":"gpt-fixture","stream":true,"store":false}`}, true
	case "http.HandlerFunc":
		if route.Admission == gateway.AdmitSelected {
			return request{method: http.MethodPost, path: "/mcp/" + MCPServer, capability: mcpCapability,
				body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + mcpTool + `","arguments":{"query":"q"}}}`}, true
		}
	}
	return request{}, false
}

// Check runs the route contract over every composed route and path route.
// selected is the tracker named MCP charges, which its route need not carry.
func Check(t *testing.T, routes gateway.Dispatcher, events *Recorder, selected *budget.Tracker) {
	t.Helper()
	if err := routes.Validate(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, authority := range slices.Sorted(maps.Keys(routes)) {
		owner := routes[authority]
		check := func(name, prefix string, route gateway.Route) {
			if route.Handler == nil {
				return
			}
			n++
			uid := fmt.Sprintf("contract-%d", n)
			free, isFree := Free[fmt.Sprintf("%T", route.Handler)]
			served, known := servedRequest(authority, route)
			if !known && !isFree {
				t.Errorf("%s: handler %T has no served request in routetest; add one, or list it as free with its record",
					name, route.Handler)
				return
			}
			capability := route.Capability
			if route.Admission == gateway.AdmitSelected {
				capability = served.capability
			}
			if route.Admission == gateway.AdmitSelected && capability == "" {
				// Only a handler that selects and checks a capability may be
				// declared AdmitSelected; routetest must know how.
				t.Errorf("%s: AdmitSelected route with handler %T has no selected request in routetest", name, route.Handler)
				return
			}
			refusedPath := served.path
			if refusedPath == "" {
				// A clean path under the prefix selects the path route.
				refusedPath = prefix + "fixture"
			}
			refusedWithoutCapability(t, name, routes, authority, refusedPath, served, capability, uid, events)
			if isFree {
				t.Logf("%s: free: %s", name, free)
				return
			}
			tracker := route.Budgets
			if route.Admission == gateway.AdmitSelected {
				tracker = selected
				// Discovery and the control connection are declared
				// unrecorded, so the dispatcher records no omission for them.
				for _, path := range append(slices.Sorted(maps.Keys(FreePaths)), "/mcp/"+MCPServer) {
					discovery := request{method: http.MethodPost, path: path, body: `{"jsonrpc":"2.0","id":1,"method":"initialize",` +
						`"params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"contract","version":"1"}}}`}
					if response := serve(routes, authority, discovery, Grant(uid+"-discovery", "", "")); response.Code != http.StatusOK {
						t.Errorf("%s%s: initialize got %d: %s", name, path, response.Code, response.Body)
					}
					noOmission(t, name+path, events.Take())
				}
			}
			spends(t, name, routes, authority, served, capability, uid, tracker, events)
		}
		check(authority, "/", owner)
		for _, p := range owner.Paths {
			check(authority+p.Prefix, p.Prefix, p.Route)
		}
	}
}

func serve(routes gateway.Dispatcher, authority string, r request, grant *gateway.ExecutionGrant) *httptest.ResponseRecorder {
	req := httptest.NewRequest(r.method, "https://"+strings.TrimSuffix(authority, ":443")+r.path, strings.NewReader(r.body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+ProviderCredential)
	req.Header.Set("ChatGPT-Account-Id", "acct")
	req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), grant)))
	return response
}

func refusedWithoutCapability(t *testing.T, name string, routes gateway.Dispatcher, authority, path string, served request,
	capability api.CapabilityKind, uid string, events *Recorder,
) {
	t.Helper()
	served.path = path
	response := serve(routes, authority, served, Grant(uid+"-refused", capability, ""))
	recorded := events.Take()
	noOmission(t, name, recorded)
	if response.Code != http.StatusForbidden {
		t.Errorf("%s: a run without %s got %d, want 403: %s", name, capability, response.Code, response.Body)
	}
	if !slices.ContainsFunc(recorded, func(e audit.Event) bool {
		return !e.Allowed && e.Capability == string(capability) && e.DenyReason != ""
	}) {
		t.Errorf("%s: refusing a run without %s recorded %+v", name, capability, recorded)
	}
}

// edgeRequests are malformed or unusual requests a sandbox can send to any
// route: none may leave the dispatcher to record an omission.
var edgeRequests = []struct{ method, path, body string }{
	{http.MethodGet, "/", ""},
	{http.MethodPost, "/", "{"},
	{http.MethodDelete, "/anything?x=1", ""},
	{http.MethodPost, "/mcp/unregistered", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`},
	{http.MethodPost, "/mcp/" + MCPServer, "not json"},
	{http.MethodPost, "/mcp/" + MCPServer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
		`"capabilities":{},"clientInfo":{"name":"edges","version":"1"}}}`},
	{http.MethodGet, "/mcp/" + MCPServer, ""},
	{http.MethodPost, "/mcp/sproozi", "not json"},
	{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
	{http.MethodPost, "/mcp", "not json"},
	{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"kubernetes_list_pods","arguments":{"namespace":"Bad_Name"}}}`},
}

// Edges sends edgeRequests, and a named MCP tool call without the Accept
// header the protocol requires, to every route, and fails on any record the
// dispatcher wrote because a handler wrote none. It returns the unrecorded
// reasons the handlers declared.
func Edges(t *testing.T, routes gateway.Dispatcher, events *Recorder) map[gateway.UnrecordedReason]bool {
	t.Helper()
	declared := map[gateway.UnrecordedReason]bool{}
	observed := gateway.Dispatcher{}
	observe := func(h http.Handler) http.Handler {
		if h == nil {
			return nil
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r)
			if reason, ok, _ := gateway.DeclaredUnrecorded(r.Context()); ok {
				declared[reason] = true
			}
		})
	}
	for authority, route := range routes {
		route.Handler = observe(route.Handler)
		route.Paths = slices.Clone(route.Paths)
		for i := range route.Paths {
			route.Paths[i].Route.Handler = observe(route.Paths[i].Route.Handler)
		}
		observed[authority] = route
	}
	grant := Grant("edges", "", "")
	for _, authority := range slices.Sorted(maps.Keys(routes)) {
		requests := slices.Clone(edgeRequests)
		for _, p := range routes[authority].Paths {
			requests = append(requests, struct{ method, path, body string }{http.MethodGet, p.Prefix + "x?y=1", "{"})
		}
		for _, e := range requests {
			serve(observed, authority, request{method: e.method, path: e.path, body: e.body}, grant)
			noOmission(t, authority+" "+e.method+" "+e.path, events.Take())
		}
		noAccept := httptest.NewRequest(http.MethodPost, "https://"+strings.TrimSuffix(authority, ":443")+"/mcp/"+MCPServer,
			strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+mcpTool+`","arguments":{}}}`))
		noAccept.Header.Set("Content-Type", "application/json")
		observed.ServeHTTP(httptest.NewRecorder(), noAccept.WithContext(gateway.WithIdentity(noAccept.Context(), grant)))
		noOmission(t, authority+" tools/call without Accept", events.Take())
	}
	return declared
}

// noOmission fails when the dispatcher had to record a request its handler
// neither recorded nor declared unrecorded.
func noOmission(t *testing.T, name string, recorded []audit.Event) {
	t.Helper()
	for _, e := range recorded {
		if e.Outcome == audit.Unrecorded {
			t.Errorf("%s: the handler wrote no record for %s; record it with gateway.Record, or declare it with "+
				"gateway.Unrecorded", name, e.Operation)
		}
	}
}

func spends(t *testing.T, name string, routes gateway.Dispatcher, authority string, served request,
	capability api.CapabilityKind, uid string, tracker *budget.Tracker, events *Recorder,
) {
	t.Helper()
	response := serve(routes, authority, served, Grant(uid, "", capability))
	recorded := events.Take()
	noOmission(t, name, recorded)
	if !slices.ContainsFunc(recorded, func(e audit.Event) bool {
		return e.Allowed && e.Outcome == audit.Served && e.Capability == string(capability)
	}) {
		t.Errorf("%s: served request recorded no allowed %s event (status %d, %s): %+v", name, capability,
			response.Code, response.Body, recorded)
	}
	if tracker == nil {
		t.Errorf("%s: no budget tracker for %s", name, capability)
		return
	}
	state, err := tracker.State(uid + "/" + string(capability))
	if err != nil || state.UnitsUsed != 1 || state.ReservedUnits != 0 {
		t.Errorf("%s: a served request left a one-unit %s budget at %+v (%v); it must spend it, or be listed as free",
			name, capability, state, err)
	}
}

// ModelAuthorities returns the model.inference routes with a handler that
// the provider table does not name, by authority or authority and prefix.
func ModelAuthorities(routes gateway.Dispatcher) map[string]string {
	known := map[string]bool{}
	for _, name := range []string{modelgateway.ProviderOpenAI, modelgateway.ProviderAnthropic, modelgateway.ProviderCopilot} {
		provider, _ := modelgateway.ProviderFor(name)
		for _, m := range provider.Models {
			known[m.Authority] = true
		}
		known[provider.SignInAuthority+provider.SignInPrefix] = true
		for _, prefix := range provider.AccountPrefixes {
			known[provider.AccountAuthority+prefix] = true
		}
	}
	strays := map[string]string{}
	for authority, route := range routes {
		if route.Handler != nil && route.Capability == api.CapabilityModelInference && !known[authority] {
			strays[authority] = fmt.Sprintf("%T", route.Handler)
		}
		for _, p := range route.Paths {
			if p.Route.Handler != nil && p.Route.Capability == api.CapabilityModelInference && !known[authority+p.Prefix] {
				strays[authority+p.Prefix] = fmt.Sprintf("%T", p.Route.Handler)
			}
		}
	}
	return strays
}
