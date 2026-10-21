package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/mcppolicy"
)

const maxResponseBytes = 8 << 20
const discoverMethod = "server/discover"

type HandlerConfig struct {
	Registry    *Registry
	HTTPClient  *http.Client
	Budgets     *budget.Tracker
	AuditLogger audit.Writer
}

type Handler struct {
	config    HandlerConfig
	transport http.RoundTripper
}

func NewHandler(config HandlerConfig) *Handler {
	var transport = http.DefaultTransport
	if config.HTTPClient != nil && config.HTTPClient.Transport != nil {
		transport = config.HTTPClient.Transport
	}
	if base, ok := transport.(*http.Transport); ok {
		copy := base.Clone()
		copy.Proxy = nil
		transport = copy
	}
	return &Handler{config: config, transport: transport}
}

// ServeHTTP exposes only policy-approved tool calls. Upstream session IDs,
// cookies, credentials and server-initiated authority never reach the workload.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := gateway.IdentityFromContext(r.Context())
	if !ok {
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/mcp/")
	capability := api.CapabilityKind("mcp." + name)
	event := gateway.AuditEvent(r.Context(), "mcp.request")
	event.Capability, event.Service = string(capability), "mcp"
	if _, valid := capability.MCPServerName(); !valid || r.URL.Path != "/mcp/"+name ||
		r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Origin")) != 0 {
		// Do not audit unvalidated path data, which might contain a secret.
		event.Capability = ""
		h.deny(w, event, http.StatusForbidden, "unsupported MCP request")
		return
	}
	registration, registered := h.config.Registry.servers[name]
	if !registered {
		event.Capability = ""
		h.deny(w, event, http.StatusForbidden, fmt.Sprintf("MCP server %q is not registered in the gateway's MCP "+
			"registry; add it to servers.json and restart the gateway", name))
		return
	}
	if !identity.Permits(capability) {
		h.deny(w, event, http.StatusForbidden, "MCP capability denied")
		return
	}
	if r.Method != http.MethodPost {
		h.deny(w, event, http.StatusMethodNotAllowed, "MCP POST required")
		return
	}
	rules, err := mcppolicy.CompileScope(identity.Policy.Spec.MCPServers[name])
	if err != nil {
		h.deny(w, event, http.StatusForbidden, "invalid MCP tool scope")
		return
	}
	// Parse the bounded envelope once for ambiguity checks; the SDK still owns
	// JSON-RPC validation and method dispatch.
	body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		h.deny(w, event, http.StatusRequestEntityTooLarge, "MCP request exceeded bounds")
		return
	}
	if _, err := mcppolicy.DecodeObject(body); err != nil {
		h.deny(w, event, http.StatusBadRequest, "invalid MCP request JSON")
		return
	}
	message, err := jsonrpc.DecodeMessage(body)
	request, supported := message.(*jsonrpc.Request)
	if err != nil || !supported || !supportedRequest(request) {
		h.deny(w, event, http.StatusBadRequest, "unsupported MCP request envelope")
		return
	}
	var params struct {
		Name string `json:"name"`
	}
	if len(request.Params) != 0 && json.Unmarshal(request.Params, &params) != nil {
		h.deny(w, event, http.StatusBadRequest, "invalid MCP request envelope")
		return
	}
	if request.Method == "tools/call" {
		if _, permitted := rules[params.Name]; !permitted {
			event.Operation = "mcp.tools.call"
			h.deny(w, event, http.StatusForbidden, "MCP tool denied")
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	limits, _, err := identity.BudgetLimits(capability)
	if err != nil {
		h.deny(w, event, http.StatusForbidden, "MCP allocation denied")
		return
	}
	if limits.MaxCostMicros > 0 {
		h.deny(w, event, http.StatusServiceUnavailable, "MCP budget accounting unavailable")
		return
	}
	meter, err := identity.Meter(r.Context(), h.config.Budgets, capability)
	if err != nil {
		h.deny(w, event, http.StatusServiceUnavailable, "MCP budget accounting unavailable")
		return
	}
	session, err := h.connect(ctx, registration)
	if err != nil {
		h.deny(w, event, http.StatusBadGateway, "MCP upstream connection failed")
		return
	}
	defer func() { _ = session.Close() }()
	server, err := h.prepare(ctx, session, rules, event, meter)
	if err != nil {
		// prepare's errors are fixed text or upstream tool names, quoted and
		// bounded, so they can name the fix without echoing upstream content.
		h.deny(w, event, http.StatusBadGateway, fmt.Sprintf("MCP server %q tools unavailable or unsupported: %v", name, err))
		return
	}
	sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 64 << 10, DisableLocalhostProtection: true,
	}).ServeHTTP(w, r.WithContext(ctx))
}

// The SDK still owns JSON-RPC decoding and dispatch. This request-local tool
// adapter admits its supported calls and notifications before expensive setup;
// retained downstream subscriptions are outside its lifetime and authority.
func supportedRequest(request *jsonrpc.Request) bool {
	switch request.Method {
	case discoverMethod, "initialize", "tools/list", "tools/call", "ping":
		return request.IsCall()
	case "notifications/initialized", "notifications/cancelled":
		return !request.IsCall()
	default:
		return false
	}
}

func (h *Handler) prepare(ctx context.Context, upstream *sdk.ClientSession,
	rules map[string]*mcppolicy.Constraint, event audit.Event, meter *budget.Meter,
) (*sdk.Server, error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := sdk.NewServer(&sdk.Implementation{Name: "sproozi-mcp", Version: "v0.1.0"}, &sdk.ServerOptions{
		Logger: logger, Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
	})
	seen := map[string]bool{}
	var offered []string
	cursor := ""
	for range 8 {
		tools, err := upstream.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("MCP discovery failed")
		}
		for _, tool := range tools.Tools {
			if seen[tool.Name] || len(seen) >= 256 {
				return nil, fmt.Errorf("MCP catalogue exceeded bounds or contains duplicate names")
			}
			seen[tool.Name] = true
			offered = append(offered, tool.Name)
			constraint, permitted := rules[tool.Name]
			if !permitted {
				continue
			}
			schema, resolved, err := compileInput(tool.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("tool %q: %w", tool.Name, err)
			}
			name := tool.Name
			// Copy only supported, non-authoritative discovery metadata. Do not
			// inherit remote parameter/header annotations or execution controls.
			description := tool.Description
			if constraint != nil {
				description += "\nAdditional argument restrictions enforced by Sproozi: " + string(constraint.Schema())
			}
			server.AddTool(&sdk.Tool{Name: name, Description: description, InputSchema: schema},
				func(callCtx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					workCtx, cancel := context.WithCancel(ctx)
					stop := context.AfterFunc(callCtx, cancel)
					defer stop()
					defer cancel()
					callEvent := event
					callEvent.Operation, callEvent.Resource = "mcp.tools.call", name
					defer func() {
						if h.config.AuditLogger != nil {
							h.config.AuditLogger.Log(callEvent)
						}
					}()
					arguments := req.Params.Arguments
					if len(arguments) == 0 {
						arguments = json.RawMessage(`{}`)
					}
					value, err := mcppolicy.DecodeObject(arguments)
					if err != nil || resolved.Validate(value) != nil || (constraint != nil && constraint.Validate(value) != nil) {
						callEvent.DenyReason = "MCP arguments denied"
						return nil, fmt.Errorf("MCP arguments denied")
					}
					if workCtx.Err() != nil || meter.SpendRequest(workCtx) != nil {
						callEvent.DenyReason = "MCP authority or budget unavailable"
						return nil, fmt.Errorf("MCP authority or budget unavailable")
					}
					// Use the parsed, validated value, never raw request headers or
					// metadata. Disable transport retries to avoid duplicate work.
					result, err := upstream.CallTool(workCtx, &sdk.CallToolParams{Name: name, Arguments: value})
					if err != nil {
						callEvent.DenyReason = "MCP upstream call failed"
						return nil, fmt.Errorf("MCP upstream call failed")
					}
					if result.IsError {
						callEvent.DenyReason = "MCP upstream tool failed"
						return nil, fmt.Errorf("MCP upstream tool failed")
					}
					callEvent.Allowed = true
					return &sdk.CallToolResult{Content: result.Content, StructuredContent: result.StructuredContent, IsError: result.IsError}, nil
				})
		}
		if tools.NextCursor == "" {
			if missing := missingTools(rules, seen); len(missing) > 0 {
				return nil, missingToolsError{missing: missing, offered: offered}
			}
			return server, nil
		}
		cursor = tools.NextCursor
	}
	return nil, fmt.Errorf("MCP pagination exceeded bounds")
}

// missingToolsError reports approved tools the server does not offer. The
// whole server fails closed, as ADR 0001 decided, so a misspelt or renamed
// tool is found on the first call rather than missing silently from the
// agent's list; the message names the fix.
type missingToolsError struct{ missing, offered []string }

func (e missingToolsError) Error() string {
	return fmt.Sprintf("the policy approves %s, which the server does not offer; it offers %s. "+
		"Correct the tool names in the AgentPolicy's mcpServers scope", quoteNames(e.missing), quoteNames(e.offered))
}

func missingTools(rules map[string]*mcppolicy.Constraint, seen map[string]bool) []string {
	var missing []string
	for name := range rules {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// quoteNames lists at most 32 names, quoted so control characters in an
// upstream tool name cannot forge log or message structure.
func quoteNames(names []string) string {
	if len(names) == 0 {
		return "no tools"
	}
	sorted := slices.Sorted(slices.Values(names))
	extra := ""
	if len(sorted) > 32 {
		extra = fmt.Sprintf(" and %d more", len(sorted)-32)
		sorted = sorted[:32]
	}
	quoted := make([]string, len(sorted))
	for i, name := range sorted {
		if len(name) > 128 {
			name = name[:128] + "…"
		}
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, ", ") + extra
}

func compileInput(input any) (*jsonschema.Schema, *jsonschema.Resolved, error) {
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > 32<<10 || input == nil {
		return nil, nil, fmt.Errorf("invalid MCP input schema")
	}
	if _, err := mcppolicy.DecodeObject(raw); err != nil {
		return nil, nil, fmt.Errorf("ambiguous MCP input schema")
	}
	var remote jsonschema.Schema
	if err := json.Unmarshal(raw, &remote); err != nil || remote.Type != "object" {
		return nil, nil, fmt.Errorf("MCP input schema must describe an object")
	}
	if err := mcppolicy.CheckSchema(&remote); err != nil {
		return nil, nil, err
	}
	resolved, err := remote.Resolve(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("unsupported MCP input schema")
	}
	return &remote, resolved, nil
}

func (h *Handler) connect(ctx context.Context, registration registration) (*sdk.ClientSession, error) {
	client := &http.Client{Timeout: 30 * time.Second,
		Transport:     credentialTransport{base: h.transport, registration: registration},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Connect and discovery share the overall request deadline, so negotiation
	// cannot extend setup by consuming several independent HTTP timeouts. The
	// SDK detaches its connection reader internally; cancellation notifications
	// still have that independent reader until our owned session is closed.
	return sdk.NewClient(&sdk.Implementation{Name: "sproozi-mcp", Version: "v0.1.0"}, &sdk.ClientOptions{Logger: logger}).Connect(
		ctx, &sdk.StreamableClientTransport{Endpoint: registration.endpoint.String(), HTTPClient: client,
			DisableStandaloneSSE: true, MaxRetries: -1}, nil)
}

type credentialTransport struct {
	base         http.RoundTripper
	registration registration
}

func (t credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != t.registration.endpoint.String() {
		return nil, fmt.Errorf("MCP upstream destination changed")
	}
	r = r.Clone(r.Context())
	r.Header.Del("Authorization")
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Cookie")
	if t.registration.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.registration.token)
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, fmt.Errorf("MCP upstream transport failed")
	}
	response.Body = &boundedBody{ReadCloser: response.Body, remaining: maxResponseBytes}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	if n > b.remaining {
		return 0, fmt.Errorf("MCP response exceeded bounds")
	}
	b.remaining -= n
	return n, err
}

func (h *Handler) deny(w http.ResponseWriter, event audit.Event, status int, reason string) {
	event.DenyReason = reason
	if h.config.AuditLogger != nil {
		h.config.AuditLogger.Log(event)
	}
	http.Error(w, reason, status)
}
