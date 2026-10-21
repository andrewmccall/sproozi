package profile

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

const testToken = "fixed-profile-principal-token-32-bytes-or-longer"
const testCapability = "model.inference"
const testThread = "01a126e8-288d-7350-9b52-627e16048343"
const testMax = int64(8 << 20)
const testIgnoredFile = "needed.bin"
const testPrincipal = "developer"
const fixtureTypeKey = "type"

type retainedBackend struct {
	state        session.Observation
	data         []byte
	digest       string
	prepareCalls int
	failure      error
	unsealed     bool
}

func (p *retainedBackend) Prepare(_ context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	if p.failure != nil {
		return session.Incarnation{}, p.failure
	}
	if !bytes.Equal(data, p.data) {
		return session.Incarnation{}, errors.New("artifact mismatch")
	}
	if p.state.State != "" {
		if !sameLaunch(l, p.state.Launch) {
			return session.Incarnation{}, &Rejection{Code: CodeConflict}
		}
		return p.state.Incarnation, nil
	}
	p.prepareCalls++
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	p.state = session.Observation{Launch: l, Incarnation: i, State: session.PlacementPrepared}
	return i, nil
}
func (p *retainedBackend) Activate(_ context.Context, l session.Launch, i session.Incarnation) error {
	if !sameLaunch(l, p.state.Launch) || i != p.state.Incarnation {
		return &Rejection{Code: CodeConflict}
	}
	p.state.State = session.PlacementActive
	return nil
}
func (p *retainedBackend) Stop(_ context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	if !sameLaunch(l, p.state.Launch) || i != p.state.Incarnation {
		return session.Stopped{}, &Rejection{Code: CodeConflict}
	}
	if p.unsealed {
		return session.Stopped{Launch: l, Incarnation: i, ActualStopped: true}, fmt.Errorf("%w: ledger unavailable", session.ErrUsageUnsealed)
	}
	s := session.Stopped{Launch: l, Incarnation: i, ActualStopped: true, Checkpoint: p.data, CheckpointDigest: p.digest, Receipt: &session.Receipt{Launch: l, Incarnation: i, Usage: map[string]budget.TrustedUsage{testCapability: {Units: 2, CostMicros: 3}}}}
	p.state.State = session.PlacementStopped
	p.state.Stopped = &s
	return s, nil
}
func (p *retainedBackend) Inspect(_ context.Context, l session.Launch) (session.Observation, error) {
	if p.failure != nil {
		return session.Observation{}, p.failure
	}
	if p.state.State == "" {
		return session.Observation{Launch: l, State: session.PlacementAbsent}, nil
	}
	return p.state, nil
}
func (p *retainedBackend) Abort(_ context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	if !sameLaunch(l, p.state.Launch) || i != p.state.Incarnation {
		return session.Observation{}, &Rejection{Code: CodeConflict}
	}
	p.state.State = session.PlacementStopped
	p.state.Stopped = nil
	return p.state, nil
}

func artifact(t *testing.T) ([]byte, string) {
	return artifactKind(t, harness.NativeCodex)
}

// builtArtifact is one native kind's stopped checkpoint fixture.
type builtArtifact struct {
	once   sync.Once
	built  bool
	data   []byte
	digest string
}

// artifacts caches each native kind's fixture checkpoint. Its Git capture is
// slow, so each kind is built once per test binary.
var artifacts sync.Map // harness.NativeKind -> *builtArtifact

func artifactKind(t *testing.T, kind harness.NativeKind) ([]byte, string) {
	t.Helper()
	value, _ := artifacts.LoadOrStore(kind, &builtArtifact{})
	a := value.(*builtArtifact)
	a.once.Do(func() {
		a.data, a.digest = buildArtifactKind(t, kind)
		a.built = true
	})
	if !a.built {
		t.Fatalf("%s fixture checkpoint failed to build in another test", kind)
	}
	return bytes.Clone(a.data), a.digest
}

func buildArtifactKind(t *testing.T, kind harness.NativeKind) ([]byte, string) {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid"}
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Git: %v %s", err, data)
		}
	}
	run("init", "--quiet", "--template=", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "note"), []byte("checkpoint\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "Fixture")
	if kind == harness.NativeCodex {
		state := harness.CodexState{ThreadID: testThread, Path: "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + testThread + ".jsonl", Rollout: []byte(`{"type":"session_meta","payload":{"id":"01a126e8-288d-7350-9b52-627e16048343","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture"}}
`)}
		if err := harness.RestoreCodexState(home, state, testMax); err != nil {
			t.Fatal(err)
		}
	} else {
		userID, assistantID := "12345678-1234-1234-1234-123456789abc", "22345678-1234-1234-1234-123456789abc"
		user, _ := json.Marshal(map[string]any{fixtureTypeKey: "user", "sessionId": testThread,
			"version": "2.1.27", "uuid": userID, "parentUuid": nil, "isSidechain": false,
			"cwd": repo, "message": map[string]any{"role": "user", "content": "Fixture question"}})
		assistant, _ := json.Marshal(map[string]any{fixtureTypeKey: "assistant", "sessionId": testThread,
			"version": "2.1.27", "uuid": assistantID, "parentUuid": userID, "isSidechain": false,
			"cwd": repo, "message": map[string]any{"role": "assistant",
				"content": []map[string]any{{fixtureTypeKey: "text", "text": "Claude fixture complete"}}}})
		path, err := harness.ClaudeProjectPath(repo, testThread)
		if err != nil {
			t.Fatal(err)
		}
		state := harness.ClaudeState{ThreadID: testThread, Workspace: repo, Path: path,
			Transcript: append(append(append(user, '\n'), assistant...), '\n')}
		if err := harness.RestoreClaudeState(home, repo, state, testMax); err != nil {
			t.Fatal(err)
		}
	}
	data, meta, err := checkpoint.CaptureStopped(context.Background(), kind, home, repo, testThread, nil, testMax)
	if err != nil {
		t.Fatal(err)
	}
	return data, meta.SHA256
}
func launch(digest string) session.Launch {
	return session.Launch{Harness: harness.NativeCodex, ExecutionID: uuid.NewString(), OperationID: uuid.NewString(), AllocationID: uuid.NewString(), ThreadID: testThread, Epoch: 2, Placement: "remote", InstructionDigest: strings.Repeat("a", 64), CheckpointDigest: digest, Deadline: time.Now().Add(30 * time.Minute).UTC(), Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 20}, CostMicros: session.Limit{Bounded: true, Value: 100}}}}
}

// tokenAuthorizer accepts exactly one bearer token as the home principal.
type tokenAuthorizer string

func (a tokenAuthorizer) Authorize(_ context.Context, token string) error {
	if token != string(a) {
		return ErrUnauthenticated
	}
	return nil
}

// testClient connects to origin with the test token, trusting the CA file
// when one is named and the system roots otherwise.
func testClient(origin, caFile string) (*Client, error) {
	var roots *x509.CertPool
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		roots = x509.NewCertPool()
		roots.AppendCertsFromPEM(data)
	}
	return Connect(Connection{Origin: origin, Token: testToken, Roots: roots, MaxCheckpointBytes: testMax})
}
func serve(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	s.StartTLS()
	t.Cleanup(s.Close)
	name := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(name, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return s, name
}
func fixture(t *testing.T) (*Client, *retainedBackend, *httptest.Server, session.Launch, []byte) {
	t.Helper()
	data, digest := artifact(t)
	backend := &retainedBackend{data: data, digest: digest}
	l := launch(digest)
	h, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken), Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour, MaxCheckpointBytes: testMax, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	s, ca := serve(t, h)
	c, err := testClient(s.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, backend, s, l, data
}

func TestIgnoredSelectionCrossesTLSAndBindsReplay(t *testing.T) {
	t.Parallel()
	c, _, _, l, data := fixture(t)
	l.IncludeIgnored = []string{testIgnoredFile}
	i, err := c.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	changed := l
	changed.IncludeIgnored = []string{"other.bin"}
	if _, err = c.Prepare(t.Context(), changed, data); err == nil {
		t.Fatal("TLS operation retry changed ignored selection")
	}
	changed.IncludeIgnored = []string{".git/config"}
	if _, err = c.Prepare(t.Context(), changed, data); err == nil {
		t.Fatal("TLS profile accepted Git control path")
	}
	if err = c.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	stopped, err := c.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stopped.Launch.IncludeIgnored, ",") != testIgnoredFile || strings.Join(stopped.Receipt.Launch.IncludeIgnored, ",") != testIgnoredFile {
		t.Fatal("TLS response or trusted receipt omitted approved selection")
	}
}

func TestPlacementActionsPreserveIdentityAndSealedCheckpoint(t *testing.T) {
	t.Parallel()
	c, b, _, l, data := fixture(t)
	ctx := context.Background()
	o, err := c.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementAbsent || !sameLaunch(l, o.Launch) {
		t.Fatalf("inspect absent: %+v %v", o, err)
	}
	i, err := c.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Prepare(ctx, l, data); err != nil || got != i || b.prepareCalls != 1 {
		t.Fatalf("prepare replay: %+v %v", got, err)
	}
	if err = c.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	o, err = c.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementActive || o.Incarnation != i {
		t.Fatalf("active: %+v %v", o, err)
	}
	s, err := c.Stop(ctx, l, i)
	if err != nil || !s.ActualStopped || !bytes.Equal(s.Checkpoint, data) || s.CheckpointDigest != l.CheckpointDigest || s.Receipt.Usage[testCapability].Units != 2 {
		t.Fatalf("sealed checkpoint: %+v %v", s, err)
	}
	o, err = c.Inspect(ctx, l)
	if err != nil || o.Stopped == nil || !bytes.Equal(o.Stopped.Checkpoint, data) {
		t.Fatalf("retained stopped: %+v %v", o, err)
	}
	o, err = c.Abort(ctx, l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped != nil {
		t.Fatalf("abort preserves unknown usage: %+v %v", o, err)
	}
}
func remoteCode(t *testing.T, err error, code Code, status int) {
	t.Helper()
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != code || remote.Status != status {
		t.Fatalf("want %s/%d; got %v", code, status, err)
	}
}
func TestUnknownAndSemanticFailureStayDistinct(t *testing.T) {
	t.Parallel()
	c, b, _, l, _ := fixture(t)
	ctx := context.Background()
	b.failure = &Rejection{Code: CodeConflict}
	_, err := c.Inspect(ctx, l)
	remoteCode(t, err, CodeConflict, 409)
	b.failure = errors.New("secret backend address unavailable")
	_, err = c.Inspect(ctx, l)
	remoteCode(t, err, CodeUnavailable, 503)
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("backend detail leaked")
	}
	b.failure = nil
	b.state = session.Observation{Launch: l, State: session.PlacementUnknown}
	o, err := c.Inspect(ctx, l)
	if err != nil || o.State != session.PlacementUnknown {
		t.Fatalf("unknown: %+v %v", o, err)
	}
	for _, incarnation := range []session.Incarnation{{}, {ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}} {
		b.state = session.Observation{Launch: l, Incarnation: incarnation, State: session.PlacementPreparing}
		o, err = c.Inspect(ctx, l)
		if err != nil || o.State != session.PlacementPreparing || o.Incarnation != incarnation || o.Stopped != nil {
			t.Fatalf("incomplete preparation observation: %+v %v", o, err)
		}
	}
}

func TestStopCanSealExpiredOriginalLaunch(t *testing.T) {
	t.Parallel()
	c, b, _, l, data := fixture(t)
	l.Deadline = time.Now().Add(-time.Second).UTC()
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	b.state = session.Observation{Launch: l, Incarnation: i, State: session.PlacementActive}
	s, err := c.Stop(context.Background(), l, i)
	if err != nil || !s.ActualStopped || !bytes.Equal(s.Checkpoint, data) {
		t.Fatalf("expired source sealing: %+v %v", s, err)
	}
}

// The transport must keep the Placement.Stop contract: a fenced writer with
// unsealed usage arrives as a stopped result plus ErrUsageUnsealed, never as an
// unknown stop or as a receipt.
func TestStopReportsFencedWriterWithUnsealedUsage(t *testing.T) {
	t.Parallel()
	c, b, _, l, _ := fixture(t)
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	b.state = session.Observation{Launch: l, Incarnation: i, State: session.PlacementActive}
	b.unsealed = true
	s, err := c.Stop(context.Background(), l, i)
	if !errors.Is(err, session.ErrUsageUnsealed) {
		t.Fatalf("unsealed stop error = %v, want ErrUsageUnsealed", err)
	}
	if !s.ActualStopped || s.Receipt != nil || s.Incarnation != i || len(s.Checkpoint) != 0 {
		t.Fatalf("unsealed stop result = %+v, want fenced writer without receipt or checkpoint", s)
	}
	b.unsealed = false
	if s, err = c.Stop(context.Background(), l, i); err != nil || s.Receipt == nil {
		t.Fatalf("retried stop did not seal: %+v %v", s, err)
	}
}

func TestAdmissionRejectsImmutableIntentWidening(t *testing.T) {
	t.Parallel()
	c, b, _, original, data := fixture(t)
	cases := map[string]func(*session.Launch){
		"units": func(l *session.Launch) {
			a := l.Allowances[testCapability]
			a.Units.Value = 21
			l.Allowances[testCapability] = a
		},
		"unbounded": func(l *session.Launch) {
			a := l.Allowances[testCapability]
			a.Units = session.Limit{}
			l.Allowances[testCapability] = a
		},
		"exhausted": func(l *session.Launch) {
			a := l.Allowances[testCapability]
			a.Units.Value = 0
			l.Allowances[testCapability] = a
		},
		"instruction": func(l *session.Launch) { l.InstructionDigest = strings.Repeat("b", 64) },
		"horizon":     func(l *session.Launch) { l.Deadline = time.Now().Add(2 * time.Hour) },
		"capability":  func(l *session.Launch) { l.Allowances["shell.exec"] = session.Allowance{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			l := original
			l.Allowances = map[string]session.Allowance{testCapability: original.Allowances[testCapability]}
			change(&l)
			_, err := c.Prepare(context.Background(), l, data)
			remoteCode(t, err, CodeDenied, 403)
		})
	}
	l := original
	l.Deadline = time.Now().Add(-time.Second)
	_, err := c.Prepare(context.Background(), l, data)
	remoteCode(t, err, CodeExpired, 410)
	l = original
	l.CheckpointDigest = strings.Repeat("b", 64)
	if _, err = c.Prepare(context.Background(), l, data); err == nil {
		t.Fatal("client accepted false digest")
	}
	l = original
	l.ThreadID = uuid.NewString()
	if _, err = c.Prepare(context.Background(), l, data); err == nil {
		t.Fatal("client accepted different native UUID")
	}
	if b.prepareCalls != 0 {
		t.Fatal("invalid request reached placement")
	}
}

func raw(t *testing.T, s *httptest.Server, path string, data []byte, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(headerContentType, jsonMedia)
	r.Header.Set(headerAuthorization, bearerPrefix+testToken)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body, resp.Header
}
func TestHTTPBoundaryRejectsMalformedAndBrowserRequests(t *testing.T) {
	t.Parallel()
	_, b, s, l, data := fixture(t)
	valid, err := json.Marshal(request{Version: protocolVersion, Launch: l, Checkpoint: data})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		body    []byte
		headers map[string]string
		path    string
		status  int
	}{
		{name: "token", body: valid, headers: map[string]string{headerAuthorization: "Bearer wrong"}, status: 401},
		{name: "origin", body: valid, headers: map[string]string{"Origin": "https://example.com"}, status: 403},
		{name: "cookie", body: valid, headers: map[string]string{"Cookie": "x=y"}, status: 403},
		{name: "unknown", body: bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"extra":true`), 1), status: 400},
		{name: "duplicate", body: bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"Version":1`), 1), status: 400},
		{name: "trailing", body: append(bytes.Clone(valid), []byte(` {}`)...), status: 400},
		{name: "digest", body: bytes.Replace(valid, []byte(l.CheckpointDigest), []byte(strings.Repeat("b", 64)), 1), status: 400},
		{name: "query", body: valid, path: "/v1/prepare?mode=other", status: 404},
		{name: "exec", body: valid, path: "/v1/exec", status: 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/v1/prepare"
			}
			status, body, headers := raw(t, s, path, tc.body, tc.headers)
			if status != tc.status {
				t.Fatalf("HTTP %d: %s", status, body)
			}
			if headers.Get("Cache-Control") != "no-store" || headers.Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("unsafe response headers")
			}
		})
	}
	limit, _ := bodyLimit(testMax)
	status, _, _ := raw(t, s, "/v1/prepare", bytes.Repeat([]byte("x"), int(limit)+1), nil)
	if status != 413 {
		t.Fatalf("oversize HTTP %d", status)
	}
	if b.prepareCalls != 0 {
		t.Fatal("rejected request reached backend")
	}
}

func TestClientRequiresVerifiedTLSAndFixedOrigin(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?q=1", "https://example.com/#fragment"} {
		if _, err := testClient(origin, ""); err == nil {
			t.Fatalf("accepted %s", origin)
		}
	}
	c, _, s, l, _ := fixture(t)
	untrusted, err := testClient(s.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	if _, err = untrusted.Inspect(context.Background(), l); err == nil {
		t.Fatal("accepted untrusted certificate")
	}
	transport := c.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS13 || c.http.Jar != nil {
		t.Fatal("transport lost fixed trust boundary")
	}
	oldTLS := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, RootCAs: transport.TLSClientConfig.RootCAs}}}
	defer oldTLS.CloseIdleConnections()
	if resp, err := oldTLS.Get(s.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server negotiated obsolete TLS")
	}
}

func TestClientRejectsRedirectAndInvalidBackendResults(t *testing.T) {
	t.Parallel()
	l := launch(strings.Repeat("c", 64))
	targetCalls := 0
	target, _ := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalls++; w.WriteHeader(200) }))
	redirect, ca := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/inspect", http.StatusTemporaryRedirect)
	}))
	c, err := testClient(redirect.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Inspect(context.Background(), l); err == nil || targetCalls != 0 {
		t.Fatal("redirect followed or accepted")
	}
	cases := map[string]func() []byte{
		"wrong intent": func() []byte {
			other := l
			other.AllocationID = uuid.NewString()
			data, _ := json.Marshal(response{Version: 1, Observation: &observation{Launch: other, State: session.PlacementAbsent}})
			return data
		},
		"unknown state": func() []byte {
			data, _ := json.Marshal(response{Version: 1, Observation: &observation{Launch: l, State: "gone"}})
			return data
		},
		"duplicate": func() []byte { return []byte(`{"version":1,"Version":1}`) },
		"oversize":  func() []byte { limit, _ := bodyLimit(testMax); return bytes.Repeat([]byte("x"), int(limit)+1) },
	}
	for name, encode := range cases {
		t.Run(name, func(t *testing.T) {
			s, ca := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(headerContentType, jsonMedia)
				_, _ = w.Write(encode())
			}))
			bad, err := testClient(s.URL, ca)
			if err != nil {
				t.Fatal(err)
			}
			defer bad.Close()
			if _, err = bad.Inspect(context.Background(), l); err == nil {
				t.Fatal("invalid result became trusted absence")
			}
		})
	}
}

func TestProfileExplicitClaudeKindAndCheckpointCompatibility(t *testing.T) {
	t.Parallel()
	data, digest := artifactKind(t, harness.NativeClaude)
	l := launch(digest)
	l.Harness = harness.NativeClaude
	backend := &retainedBackend{data: data, digest: digest}
	// Keep the existing public HTTPS client path: no backend-specific wire API.
	h, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken),
		Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances,
		MaxDuration: time.Hour, MaxCheckpointBytes: testMax, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	s, ca := serve(t, h)
	c, err := testClient(s.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	i, err := c.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	stopped, err := c.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := checkpoint.Inspect(stopped.Checkpoint, testMax)
	if err != nil || meta.Harness != harness.NativeClaude {
		t.Fatalf("Claude checkpoint response: %+v %v", meta, err)
	}
	changed := l
	changed.Harness = harness.NativeCodex
	if _, err = c.Inspect(t.Context(), changed); err == nil {
		t.Fatal("fixed Claude profile accepted another native kind")
	}
	codex, otherDigest := artifact(t)
	foreign := l
	foreign.OperationID, foreign.AllocationID = uuid.NewString(), uuid.NewString()
	foreign.CheckpointDigest = otherDigest
	if _, err = c.Prepare(t.Context(), foreign, codex); err == nil {
		t.Fatal("Claude launch accepted Codex native state")
	}
	foreign.Harness = ""
	if _, err = c.Prepare(t.Context(), foreign, codex); err == nil {
		t.Fatal("empty harness silently selected Codex")
	}
}

type profileStore struct {
	stored map[string][]byte
}

func (s *profileStore) StoreProfile(_ context.Context, digest string, archive []byte) error {
	s.stored[digest] = archive
	return nil
}

// A move carries the archive its launch names; the executor stores it only
// after checking it, and refuses when it keeps no profiles.
func TestPrepareCarriesTheNamedProfileArchive(t *testing.T) {
	t.Parallel()
	data, digest := artifact(t)
	archive, profileDigest, err := agentprofile.Profile{Instructions: "Profile instructions"}.Archive()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := agentprofile.Profile{Instructions: "Other instructions"}.Archive()
	if err != nil {
		t.Fatal(err)
	}
	l := launch(digest)
	l.ProfileDigest = profileDigest
	connect := func(store ProfileStore) *Client {
		h, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken), Placement: l.Placement,
			InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour, MaxCheckpointBytes: testMax,
			Backend: &retainedBackend{data: data, digest: digest}, Profiles: store})
		if err != nil {
			t.Fatal(err)
		}
		s, ca := serve(t, h)
		c, err := testClient(s.URL, ca)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	store := &profileStore{stored: map[string][]byte{}}
	c := connect(store)
	if _, err = c.Prepare(t.Context(), l, data); err == nil {
		t.Fatal("launch named a profile the client cannot supply")
	}
	c.SetProfileSource(func(string) ([]byte, error) { return other, nil })
	if _, err = c.Prepare(t.Context(), l, data); err == nil || len(store.stored) != 0 {
		t.Fatalf("mismatched archive sent: %v", err)
	}
	c.SetProfileSource(func(d string) ([]byte, error) {
		if d != profileDigest {
			t.Fatalf("source asked for %s", d)
		}
		return archive, nil
	})
	if _, err = c.Prepare(t.Context(), l, data); err != nil {
		t.Fatal(err)
	}
	if string(store.stored[profileDigest]) != string(archive) {
		t.Fatal("executor did not retain the profile")
	}
	unsupported := connect(nil)
	unsupported.SetProfileSource(func(string) ([]byte, error) { return archive, nil })
	var remote *RemoteError
	if _, err = unsupported.Prepare(t.Context(), l, data); !errors.As(err, &remote) || remote.Code != CodeUnsupported {
		t.Fatalf("executor without profiles = %v", err)
	}
}
