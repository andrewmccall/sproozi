package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/mcppolicy"
	"github.com/andrewmccall/sproozi/internal/telemetry"
)

var operationDuration, _ = telemetry.Meter().Float64Histogram("mcp.client.operation.duration",
	metric.WithUnit("s"), metric.WithDescription("Duration of MCP tool calls the gateway made upstream"),
	metric.WithExplicitBucketBoundaries(0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300))

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
		h.deny(w, event, http.StatusForbidden, "MCP server is not registered")
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
	// The method is one of the supported names, never arbitrary workload text.
	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String(telemetry.MCPMethodName, request.Method))
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
		h.deny(w, event, http.StatusBadGateway, "MCP upstream tools unavailable or unsupported")
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
	selected := 0
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
			constraint, permitted := rules[tool.Name]
			if !permitted {
				continue
			}
			schema, resolved, err := compileInput(tool.InputSchema)
			if err != nil {
				return nil, err
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
					// The tool name comes from the approved upstream catalogue.
					start := time.Now()
					attrs := []attribute.KeyValue{attribute.String(telemetry.MCPMethodName, "tools/call"),
						attribute.String(telemetry.GenAIToolName, name), attribute.String(telemetry.Capability, event.Capability)}
					spanCtx, span := telemetry.Tracer().Start(ctx, "tools/call "+name, trace.WithSpanKind(trace.SpanKindClient),
						trace.WithAttributes(append(attrs, attribute.String(telemetry.GenAIOperationName, "execute_tool"))...))
					callEvent := event.WithContext(spanCtx)
					callEvent.Operation, callEvent.Resource = "mcp.tools.call", name
					defer func() {
						if h.config.AuditLogger != nil {
							h.config.AuditLogger.Log(callEvent)
						}
						if !callEvent.Allowed {
							attrs = append(attrs, attribute.String(telemetry.ErrorType, callEvent.DenyReason))
							span.SetStatus(codes.Error, callEvent.DenyReason)
						}
						span.End()
						operationDuration.Record(spanCtx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
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
			selected++
		}
		if tools.NextCursor == "" {
			if selected != len(rules) {
				return nil, fmt.Errorf("approved MCP tool missing")
			}
			return server, nil
		}
		cursor = tools.NextCursor
	}
	return nil, fmt.Errorf("MCP pagination exceeded bounds")
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
