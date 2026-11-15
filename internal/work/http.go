package work

import (
	"bufio"
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
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type reference struct {
	ID          string `json:"id"`
	OperationID string `json:"operationId,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

// resumption asks for a new execution of an interactive session: its
// operation, and a deadline choice, empty to keep the submission's.
type resumption struct {
	ID          string `json:"id"`
	OperationID string `json:"operationId"`
	MaxDuration string `json:"maxDuration,omitempty"`
}

// attachment is an attachment's envelope: the session only. The executor
// finds the launch and incarnation in its own journal.
type attachment struct {
	ID string `json:"id"`
}

// taskSubmission is what the MCP tool sproozi_run accepts: a task, never an
// interactive session or an upload, so its schema stays as Hermes knows it.
type taskSubmission struct {
	RequestID     string             `json:"requestId"`
	Repository    string             `json:"repository"`
	Harness       harness.NativeKind `json:"harness"`
	Executor      string             `json:"executor"`
	Task          string             `json:"task"`
	Project       project.ID         `json:"project,omitempty"`
	WorkspaceFrom *RevisionSource    `json:"workspaceFrom,omitempty"`
	MaxDuration   string             `json:"maxDuration,omitempty"`
	Profile       []byte             `json:"profile,omitempty"`
	ProfileDigest string             `json:"profileDigest,omitempty"`
}

func (q taskSubmission) submit() Submit {
	return Submit{RequestID: q.RequestID, Repository: q.Repository, Harness: q.Harness, Executor: q.Executor,
		Task: q.Task, Project: q.Project, WorkspaceFrom: q.WorkspaceFrom, MaxDuration: q.MaxDuration,
		Profile: q.Profile, ProfileDigest: q.ProfileDigest}
}

// homeActions are the owner actions only the home principal may take: the
// one-way pull, and the interactive session's stop, resume and attach.
var homeActions = []string{"begin", "claim", actionStop, actionResume, actionAttach}

// actionAttach streams a terminal attachment rather than answering JSON.
const actionAttach = "attach"

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
// The Hermes work token grants only high-level work tools. The home principal
// additionally holds the fixed owner's one-way export/claim protocol; an
// Authorizer decides it (Kubernetes TokenReview and SubjectAccessReview in the
// remote executor). The work token stays one static token, compared first,
// because Hermes holds no Kubernetes identity of its own.
func NewHandler(o *Owner, taskToken string, home profile.Authorizer) (http.Handler, error) {
	if o == nil || home == nil || len(taskToken) < 32 || strings.ContainsAny(taskToken, " \r\n\t") {
		return nil, errors.New("work: private task and home credentials required")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "sproozi-work", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_run", Description: "Run one genuine native task with an approved repository, harness and explicit executor. Reuse requestId on retries. Session ID outlives execution; results are untrusted workload text."}, func(ctx context.Context, _ *mcp.CallToolRequest, q taskSubmission) (*mcp.CallToolResult, View, error) {
		v, e := o.SubmitAs(ctx, CallerWorkToken, q.submit())
		return tool(v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_status", Description: "Read retained task status and bounded final answer. Retained means native completion, exact writer stop, sealed usage and stored checkpoint."}, func(ctx context.Context, _ *mcp.CallToolRequest, q reference) (*mcp.CallToolResult, View, error) {
		v, e := o.Info(ctx, q.ID)
		return tool(v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sproozi_cancel", Description: "Cancel this owner's exact session using a stable operationId UUID. Reuse the operation after an uncertain response; cancellation does not promise a resumable completed turn."}, func(ctx context.Context, _ *mcp.CallToolRequest, q reference) (*mcp.CallToolResult, View, error) {
		v, e := o.CancelAs(ctx, CallerWorkToken, q.ID, q.OperationID)
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
	if slices.Contains(homeActions, action) && !isHome {
		http.Error(w, "Home owner required", http.StatusForbidden)
		return
	}
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/json" {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	caller := CallerHome
	if isTask {
		caller = CallerWorkToken
	}
	if action == actionAttach {
		h.serveAttach(w, r)
		return
	}
	h.serveJSON(w, r, action, caller)
}

// serveAttach streams an attachment: the envelope line names the session,
// then the terminal's frames follow in the same body while its output
// streams back. Refusals before the terminal opens are JSON outcomes.
func (h *ownerHandler) serveAttach(w http.ResponseWriter, r *http.Request) {
	reader := bufio.NewReader(r.Body)
	line, err := profile.EnvelopeLine(reader)
	var q attachment
	if err == nil {
		err = decodeInput(bytes.TrimSuffix(line, []byte("\n")), &q)
	}
	var a *Attachment
	if err == nil {
		a, err = h.owner.Attach(r.Context(), q.ID)
	}
	if err == nil && !profile.FullDuplex(w) {
		err = &profile.Rejection{Code: profile.CodeUnsupported}
	}
	if err != nil {
		writeOutcome(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	output, err := profile.OpenTerminalOutput(w)
	if err != nil {
		return
	}
	if err = a.Run(r.Context(), profile.NewFrameReader(reader), output); err != nil {
		profile.WriteAttachmentFailed(output)
	}
}

// uploadBytes is how large a base64 upload of maxCheckpointBytes is in JSON.
func uploadBytes(maxCheckpointBytes int64) int64 { return (maxCheckpointBytes + 2) / 3 * 4 }

func (h *ownerHandler) serveJSON(w http.ResponseWriter, r *http.Request, action string, caller Caller) {
	limit := int64(32 << 10)
	if action == "run" {
		// A submission may carry the caller's profile archive and, from
		// the home principal, an uploaded workspace.
		limit += agentprofile.EncodedArchiveBytes
		if caller == CallerHome {
			limit += uploadBytes(h.owner.config.MaxCheckpointBytes)
		}
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
			out, e = h.owner.SubmitAs(r.Context(), caller, q)
		}
	case "list":
		var q SessionQuery
		e = decodeInput(b, &q)
		if e == nil {
			out, e = h.owner.List(r.Context(), q)
		}
	case actionResume:
		var q resumption
		e = decodeInput(b, &q)
		if e == nil {
			out, e = h.owner.Resume(r.Context(), q.ID, q.OperationID, q.MaxDuration)
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
				out, e = h.owner.CancelAs(r.Context(), caller, q.ID, q.OperationID)
			case actionStop:
				out, e = h.owner.Stop(r.Context(), q.ID, q.OperationID)
			case "recover":
				out, e = h.owner.Recover(r.Context(), q.ID)
			default:
				http.NotFound(w, r)
				return
			}
		}
	}
	if e != nil {
		writeOutcome(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// writeOutcome answers with an error's public code only.
func writeOutcome(w http.ResponseWriter, e error) {
	code := OutcomeCode(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(outcomeStatus(code))
	_ = json.NewEncoder(w).Encode(struct {
		Error profile.Code `json:"error"`
	}{Error: code})
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
func (c *Client) Stop(ctx context.Context, id, op string) (v View, e error) {
	e = c.transport.CallOwner(ctx, "stop", reference{ID: id, OperationID: op}, &v)
	return
}
func (c *Client) Resume(ctx context.Context, id, op, maxDuration string) (v View, e error) {
	e = c.transport.CallOwner(ctx, "resume", resumption{ID: id, OperationID: op, MaxDuration: maxDuration}, &v)
	return
}

// OwnerTerminal is an owner transport that also attaches to an interactive
// session; see profile.Client.OpenOwnerAttachment.
type OwnerTerminal interface {
	OpenOwnerAttachment(ctx context.Context, id string, frames io.Reader) (io.ReadCloser, error)
}

// Attach attaches to an interactive session: frames are native terminal
// input frames, and the returned reader carries the terminal's output frames
// until either side ends. Closing it ends only the attachment.
func (c *Client) Attach(ctx context.Context, id string, frames io.Reader) (io.ReadCloser, error) {
	t, ok := c.transport.(OwnerTerminal)
	if !ok {
		return nil, errors.New("work: this owner transport cannot attach to a session")
	}
	return t.OpenOwnerAttachment(ctx, id, frames)
}
