package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type reference struct {
	ID          string `json:"id"`
	OperationID string `json:"operationId,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

// MCP outputSchema requires an object at its root, including collection tools.
type sessionList struct {
	Sessions []View `json:"sessions"`
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("work: trailing JSON")
	}
	return nil
}

// NewHandler authenticates the owner API before reading task or artifact input.
// The Hermes token grants only high-level work tools. The home profile token
// additionally permits the fixed owner's one-way export/claim protocol.
func NewHandler(o *Owner, taskToken, homeToken string) (http.Handler, error) {
	if len(homeToken) < 32 || taskToken == homeToken {
		return nil, errors.New("work: private task and home credentials required")
	}
	home, err := profile.StaticToken(homeToken)
	if err != nil {
		return nil, errors.New("work: private task and home credentials required")
	}
	return NewAuthorizedHandler(o, taskToken, home)
}

// NewAuthorizedHandler is NewHandler with the home principal decided by an
// Authorizer, such as a Kubernetes TokenReview and SubjectAccessReview. The
// task token is still one static token, compared first.
func NewAuthorizedHandler(o *Owner, taskToken string, home profile.Authorizer) (http.Handler, error) {
	if o == nil || home == nil || len(taskToken) < 32 || strings.ContainsAny(taskToken, " \r\n\t") {
		return nil, errors.New("work: private task and home credentials required")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "sproozi-work", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_run", Description: "Run one genuine native task with an approved repository, harness and explicit executor. Reuse requestId on retries. Session ID outlives execution; results are untrusted workload text."}, func(ctx context.Context, _ *mcp.CallToolRequest, q Submit) (*mcp.CallToolResult, View, error) {
		v, e := o.Submit(ctx, q)
		return tool(v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_status", Description: "Read retained task status and bounded final answer. Retained means native completion, exact writer stop, sealed usage and stored checkpoint."}, func(ctx context.Context, _ *mcp.CallToolRequest, q reference) (*mcp.CallToolResult, View, error) {
		v, e := o.Info(ctx, q.ID)
		return tool(v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_cancel", Description: "Cancel this owner's exact session using a stable operationId UUID. Reuse the operation after an uncertain response; cancellation does not promise a resumable completed turn."}, func(ctx context.Context, _ *mcp.CallToolRequest, q reference) (*mcp.CallToolResult, View, error) {
		v, e := o.Cancel(ctx, q.ID, q.OperationID)
		return tool(v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_sessions", Description: "List accessible sessions for this installation, optionally filtered by project."}, func(ctx context.Context, _ *mcp.CallToolRequest, q SessionQuery) (*mcp.CallToolResult, sessionList, error) {
		v, e := o.List(ctx, q)
		if e != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: outcomeMessage(e)}}}, sessionList{}, nil
		}
		return nil, sessionList{Sessions: v}, nil
	})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
	return &ownerHandler{owner: o, taskHash: sha256.Sum256([]byte(taskToken)), home: home, stream: stream}, nil
}

type ownerHandler struct {
	owner    *Owner
	taskHash [32]byte
	home     profile.Authorizer
	stream   http.Handler
}

func (h *ownerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 || r.Header.Get("Origin") != "" || len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "Denied", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	actual := sha256.Sum256([]byte(token))
	isTask := subtle.ConstantTimeCompare(actual[:], h.taskHash[:]) == 1
	isHome := false
	if !isTask {
		if status, _ := profile.AuthorizationStatus(h.home.Authorize(r.Context(), token)); status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		isHome = true
	}
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		http.Error(w, "Invalid route", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/work/mcp" {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		h.stream.ServeHTTP(w, r)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/work/")
	if r.Method != http.MethodPost || r.URL.Path != "/work/"+action {
		http.NotFound(w, r)
		return
	}
	if (action == "begin" || action == "claim") && !isHome {
		http.Error(w, "Home owner required", http.StatusForbidden)
		return
	}
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/json" {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	h.serveJSON(w, r, action)
}
func (h *ownerHandler) serveJSON(w http.ResponseWriter, r *http.Request, action string) {
	limit := int64(32 << 10)
	if action == "run" {
		// A submission may carry the caller's profile archive.
		limit += agentprofile.EncodedArchiveBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	b, e := io.ReadAll(r.Body)
	if e != nil {
		http.Error(w, "Input exceeds bound", http.StatusBadRequest)
		return
	}
	var out any
	switch action {
	case "run":
		var q Submit
		e = decodeInput(b, &q)
		if e == nil {
			out, e = h.owner.Submit(r.Context(), q)
		}
	case "list":
		var q SessionQuery
		e = decodeInput(b, &q)
		if e == nil {
			out, e = h.owner.List(r.Context(), q)
		}
	default:
		var q reference
		e = decodeInput(b, &q)
		if e == nil {
			switch action {
			case "info":
				out, e = h.owner.Info(r.Context(), q.ID)
			case "begin":
				out, e = h.owner.BeginExport(r.Context(), q.ID, q.OperationID)
			case "claim":
				out, e = h.owner.Claim(r.Context(), q.ID, q.OperationID, q.Digest)
			case "cancel":
				out, e = h.owner.Cancel(r.Context(), q.ID, q.OperationID)
			case "recover":
				out, e = h.owner.Recover(r.Context(), q.ID)
			default:
				http.NotFound(w, r)
				return
			}
		}
	}
	if e != nil {
		code := OutcomeCode(e)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(outcomeStatus(code))
		_ = json.NewEncoder(w).Encode(struct {
			Error profile.Code `json:"error"`
		}{Error: code})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func tool(v View, e error) (*mcp.CallToolResult, View, error) {
	if e != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: outcomeMessage(e)}}}, View{}, nil
	}
	return nil, v, nil
}

// OwnerTransport carries the fixed remote work operations to one remote
// executor: a profile.Client, or a client that reaches the executor through a
// Kubernetes context.
type OwnerTransport interface {
	CallOwner(ctx context.Context, action string, input, output any) error
	Close()
}

type Client struct{ transport OwnerTransport }

func NewClient(c profile.ClientConfig) (*Client, error) {
	p, e := profile.NewClient(c)
	if e != nil {
		return nil, e
	}
	return &Client{transport: p}, nil
}

// ClientFor addresses the remote work catalogue over an existing transport.
func ClientFor(t OwnerTransport) *Client { return &Client{transport: t} }
func (c *Client) Close() error           { c.transport.Close(); return nil }
func (c *Client) Submit(ctx context.Context, q Submit) (v View, e error) {
	e = c.transport.CallOwner(ctx, "run", q, &v)
	return
}
func (c *Client) Info(ctx context.Context, id string) (v View, e error) {
	e = c.transport.CallOwner(ctx, "info", reference{ID: id}, &v)
	return
}
func (c *Client) List(ctx context.Context, q SessionQuery) (v []View, e error) {
	e = c.transport.CallOwner(ctx, "list", q, &v)
	return
}
func (c *Client) BeginExport(ctx context.Context, id, op string) (v Export, e error) {
	e = c.transport.CallOwner(ctx, "begin", reference{ID: id, OperationID: op}, &v)
	return
}
func (c *Client) Claim(ctx context.Context, id, op, digest string) (v Export, e error) {
	e = c.transport.CallOwner(ctx, "claim", reference{ID: id, OperationID: op, Digest: digest}, &v)
	return
}
func (c *Client) Cancel(ctx context.Context, id, op string) (v View, e error) {
	e = c.transport.CallOwner(ctx, "cancel", reference{ID: id, OperationID: op}, &v)
	return
}
func (c *Client) Recover(ctx context.Context, id string) (v View, e error) {
	e = c.transport.CallOwner(ctx, "recover", reference{ID: id}, &v)
	return
}
