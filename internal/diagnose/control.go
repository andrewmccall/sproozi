package diagnose

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// ControlPath is where the gateway serves the Sproozi control connection on
// its own authority, beside named MCP connections.
const ControlPath = "/mcp/" + api.ControlMCPServer

const (
	defaultLookback = 5 * time.Minute
	maxLookback     = time.Hour
	maxResults      = 20
)

type Status string

const (
	// StatusDenied means the gateway recorded a matching denial for this execution.
	StatusDenied Status = "denied"
	// StatusNotObserved means no matching gateway decision was retained.
	StatusNotObserved Status = "not_observed"
)

const notObservedNote = "The gateway retained no matching denial for this execution. " +
	"Traffic blocked before the gateway, such as a direct connection that bypasses the proxy, " +
	"leaves no record, so this is not evidence of either a policy denial or an allowed request."

type Arguments struct {
	Destination     string `json:"destination,omitempty" jsonschema:"Host, or part of a host or resource, to match; omit to list recent denials"`
	LookbackSeconds int    `json:"lookback_seconds,omitempty" jsonschema:"How far back to look, 1 to 3600 seconds; default 300"`
	DecisionID      string `json:"decision_id,omitempty" jsonschema:"Exact decision_id from an earlier diagnose result"`
}

type Decision struct {
	DecisionID  string    `json:"decision_id"`
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	Capability  string    `json:"capability,omitempty"`
	Enforcement string    `json:"enforcement,omitempty"`
	Operation   string    `json:"operation"`
	Resource    string    `json:"resource,omitempty"`
	Reason      string    `json:"reason"`
	Policy      string    `json:"policy,omitempty"`
}

type Result struct {
	Status    Status     `json:"status"`
	Decisions []Decision `json:"decisions"`
	Note      string     `json:"note,omitempty"`
}

// NewControl serves the Sproozi control connection at ControlPath and passes
// every other request to next. It needs no capability: an execution can read
// only its own retained denials, selected by its authenticated identity.
func NewControl(journal *Journal, next http.Handler) http.Handler {
	server := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "sproozi", Version: "v0.1.0"}, nil)
		mcp.AddTool(server, &mcp.Tool{
			Name: "diagnose",
			Description: "Explain a failed external request: returns this execution's recent Sproozi gateway denials. " +
				"Read-only. A denial cannot be bypassed with another tool, transport or credential.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, args Arguments) (*mcp.CallToolResult, any, error) {
			identity, _ := gateway.IdentityFromContext(ctx)
			result := journal.diagnose(identity.Execution.UID, args, time.Now().UTC())
			data, err := json.Marshal(result)
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
		})
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 64 << 10,
		// proxytransport binds CONNECT, SNI and Host; the local address is the proxy's.
		DisableLocalhostProtection: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ControlPath {
			next.ServeHTTP(w, r)
			return
		}
		if identity, ok := gateway.IdentityFromContext(r.Context()); !ok || identity.Execution.UID == "" {
			http.Error(w, "gateway identity required", http.StatusUnauthorized)
			return
		}
		if len(r.Header.Values("Origin")) != 0 || r.URL.RawQuery != "" || r.URL.RawPath != "" {
			http.Error(w, "unsupported MCP request", http.StatusForbidden)
			return
		}
		server.ServeHTTP(w, r)
	})
}

func (j *Journal) diagnose(runUID string, args Arguments, now time.Time) Result {
	lookback := defaultLookback
	if args.LookbackSeconds > 0 {
		lookback = min(time.Duration(args.LookbackSeconds)*time.Second, maxLookback)
	}
	var found []Decision
	if args.DecisionID != "" {
		if d, ok := j.Lookup(runUID, args.DecisionID); ok {
			found = append(found, publicDecision(d))
		}
	} else {
		destination := strings.ToLower(strings.TrimSpace(args.Destination))
		for _, d := range j.Denials(runUID, now.Add(-lookback), now) {
			if len(found) == maxResults {
				break
			}
			if destination == "" || strings.Contains(strings.ToLower(d.Event.Operation), destination) ||
				strings.Contains(strings.ToLower(d.Event.Resource), destination) {
				found = append(found, publicDecision(d))
			}
		}
	}
	if len(found) == 0 {
		return Result{Status: StatusNotObserved, Decisions: []Decision{}, Note: notObservedNote}
	}
	return Result{Status: StatusDenied, Decisions: found}
}

func publicDecision(d Entry) Decision {
	e := d.Event
	return Decision{DecisionID: d.ID, Timestamp: e.Timestamp, Source: "gateway", Capability: e.Capability,
		Enforcement: string(e.SemanticLevel), Operation: e.Operation, Resource: e.Resource,
		Reason: e.DenyReason, Policy: e.PolicyName}
}
