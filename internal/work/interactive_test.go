//nolint:goconst // Test identity values remain literal inputs.
package work

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

func interactiveRequest() Submit {
	return Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Mode: ModeInteractive}
}

func openOwner(t *testing.T, c Config) *Owner {
	t.Helper()
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	return o
}

// startInteractive submits an interactive session and waits until it runs.
func startInteractive(t *testing.T, o *Owner, q Submit) View {
	t.Helper()
	v, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != ModeInteractive {
		t.Fatalf("submission lost its mode: %+v", v)
	}
	return waitPhase(t, o, v.ID, phaseActive)
}

func code(err error) profile.Code {
	if err == nil {
		return ""
	}
	return OutcomeCode(err)
}

// A Claude Code executor runs interactive sessions; its tasks stay refused
// until the supervisor binds Claude task completion.
func TestClaudeExecutorRunsInteractiveSessionsAndRefusesTasks(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.Harness = harness.NativeClaude
	o := openOwner(t, c)

	q := interactiveRequest()
	q.Harness = harness.NativeClaude
	v := startInteractive(t, o, q)
	if v.Harness != harness.NativeClaude {
		t.Fatalf("interactive session lost its harness: %+v", v)
	}
	p.mu.Lock()
	launched := p.launches[0]
	p.mu.Unlock()
	if launched.Harness != harness.NativeClaude || launched.Task != "" || launched.ThreadID != "" {
		t.Fatalf("Claude first visit launched as %+v", launched)
	}

	task := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeClaude,
		Executor: executorKubernetes, Task: fixtureTask}
	if _, err := o.Submit(t.Context(), task); code(err) != profile.CodeUnsupported {
		t.Fatalf("Claude task accepted: %v", err)
	}
	if _, err := o.Submit(t.Context(), interactiveRequest()); code(err) != profile.CodeUnsupported {
		t.Fatalf("Codex session accepted by a Claude executor: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.launches) != 1 {
		t.Fatalf("refused submissions launched: %d", len(p.launches))
	}
}

// Only executors for Codex and Claude Code are configurable.
func TestConfigAcceptsCodexAndClaudeExecutorsOnly(t *testing.T) {
	t.Parallel()
	c := fixtureConfig(t, filepath.Join(t.TempDir(), "repository"), &fixturePlacement{})
	c.RetainedBytes = 2 * c.MaxCheckpointBytes
	for kind, valid := range map[harness.NativeKind]bool{harness.NativeCodex: true, harness.NativeClaude: true,
		"opencode": false, harness.NativeCopilot: false, "": false} {
		c.Harness = kind
		if err := c.validate(); (err == nil) != valid {
			t.Errorf("harness %q: %v", kind, err)
		}
	}
}

// An interactive session has no completion driver: it runs until its owner
// stops it, and the stop keeps its conversation.
func TestInteractiveSessionRunsUntilItsOwnerStopsIt(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.retry = backoff{first: time.Millisecond, limit: 2 * time.Millisecond}
	o := openOwner(t, c)
	q := interactiveRequest()
	v := startInteractive(t, o, q)
	if running := waitDriver(t, o, v.ID); running.Phase != phaseActive {
		t.Fatalf("an interactive session stopped by itself: %+v", running)
	}
	p.mu.Lock()
	stops, task := p.stops, p.launches[0].Task
	p.mu.Unlock()
	if stops != 0 || task != "" {
		t.Fatalf("an interactive session was driven as a task: stops=%d task=%q", stops, task)
	}
	op := uuid.NewString()
	stopped, err := o.Stop(t.Context(), v.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Phase != PhaseRetained || stopped.CheckpointDigest != p.meta.SHA256 || stopped.Result != "" || stopped.Exit != nil {
		t.Fatalf("stopped interactive session: %+v", stopped)
	}
	again, err := o.Stop(t.Context(), v.ID, op)
	if err != nil || again.Phase != PhaseRetained {
		t.Fatalf("stop replay: %+v %v", again, err)
	}
	if _, err = o.Stop(t.Context(), v.ID, q.RequestID); code(err) != profile.CodeConflict {
		t.Fatalf("the submission's request reused as a stop: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stops != 1 {
		t.Fatalf("stop replay stopped again: %d", p.stops)
	}
}

// A harness that exited with a failure leaves the session failed, with its
// exit.
func TestInteractiveSessionWhoseHarnessFailedStopsAsFailed(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	p.mu.Lock()
	p.exited = map[string]bool{v.AllocationID: true}
	p.exit = &session.Exit{Code: 2, Cause: harness.NativeExitSelf, Output: "boom"}
	p.mu.Unlock()
	stopped, err := o.Stop(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Phase != PhaseFailed || stopped.Exit == nil || stopped.Exit.Code != 2 || stopped.Exit.Output != "boom" {
		t.Fatalf("failed exit not reported: %+v", stopped)
	}
}

// The exit sweep stops a session whose harness ended on its own, through the
// ordinary stop, and reports its exit.
func TestExitSweepStopsAnInteractiveSessionWhoseHarnessEnded(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.exitInterval = 5 * time.Millisecond
	o := openOwner(t, c)
	task := submit(t, o)
	waitPhase(t, o, task.ID, phaseRetained)
	v := startInteractive(t, o, interactiveRequest())
	p.mu.Lock()
	p.exited = map[string]bool{v.AllocationID: true}
	p.exit = &session.Exit{Code: 1, Cause: harness.NativeExitSelf}
	p.mu.Unlock()
	failed := waitPhase(t, o, v.ID, PhaseFailed)
	if failed.Exit == nil || failed.Exit.Code != 1 || failed.CheckpointDigest != p.meta.SHA256 {
		t.Fatalf("exit not reported: %+v", failed)
	}
}

// An interactive session the coordinator could not settle waits for
// "recover"; the executor never retries it by itself.
func TestInteractiveSessionIsNotRecoveredAutomatically(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.retry = backoff{first: time.Millisecond, limit: 2 * time.Millisecond}
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	p.mu.Lock()
	p.missingFence = true
	p.mu.Unlock()
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err == nil {
		t.Fatal("an unproven stop succeeded")
	}
	time.Sleep(50 * time.Millisecond)
	if got := waitDriver(t, o, v.ID); got.Phase != phaseUnknownStop {
		t.Fatalf("phase=%s", got.Phase)
	}
	p.mu.Lock()
	stops := p.stops
	p.missingFence = false
	p.mu.Unlock()
	if stops != 1 {
		t.Fatalf("an interactive session was recovered automatically: %d stops", stops)
	}
	recovered, err := o.Recover(t.Context(), v.ID)
	if err != nil || recovered.Phase != PhaseRetained {
		t.Fatalf("recover: %+v %v", recovered, err)
	}
}

// Resume starts a new execution of the session from its checkpoint, with a
// fresh deadline and allowance, bound to its operation.
func TestResumeStartsAFreshExecutionOfTheSession(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	stopOp := uuid.NewString()
	if _, err := o.Stop(t.Context(), v.ID, stopOp); err != nil {
		t.Fatal(err)
	}
	first, err := c.Coordinator.Inspect(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.NewString()
	resumed, err := o.Resume(t.Context(), v.ID, op, "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != v.ID || resumed.ExecutionID == v.ID || !CanonicalID(resumed.ExecutionID) {
		t.Fatalf("resume did not start a new execution of the session: %+v", resumed)
	}
	execution, err := c.Coordinator.Inspect(resumed.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Session != v.ID || execution.Operations[0].ID != op || !execution.Deadline.After(first.Deadline) ||
		!maps.Equal(execution.Remaining, c.Allowances) || execution.ThreadID != testThread {
		t.Fatalf("resumed execution: %+v", execution)
	}
	if again, err := o.Resume(t.Context(), v.ID, op, ""); err != nil || again.ExecutionID != resumed.ExecutionID {
		t.Fatalf("resume replay: %+v %v", again, err)
	}
	active := waitPhase(t, o, v.ID, phaseActive)
	if active.ExecutionID != resumed.ExecutionID {
		t.Fatalf("status names another execution: %+v", active)
	}
	p.mu.Lock()
	launch := p.launches[len(p.launches)-1]
	p.mu.Unlock()
	if launch.ExecutionID != resumed.ExecutionID || launch.ThreadID != testThread || launch.CheckpointDigest != p.meta.SHA256 {
		t.Fatalf("resume did not continue the conversation: %+v", launch)
	}
	if _, err = o.Resume(t.Context(), v.ID, uuid.NewString(), ""); code(err) != profile.CodeConflict {
		t.Fatalf("a running session resumed again: %v", err)
	}
	if _, err = o.Resume(t.Context(), v.ID, stopOp, ""); code(err) != profile.CodeConflict {
		t.Fatalf("a stop's operation resumed the session: %v", err)
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openOwner(t, c)
	if again, err := reopened.Resume(t.Context(), v.ID, op, ""); err != nil || again.ExecutionID != resumed.ExecutionID {
		t.Fatalf("resume replay after a restart: %+v %v", again, err)
	}
	if stopped, err := reopened.Stop(t.Context(), v.ID, uuid.NewString()); err != nil || stopped.Phase != PhaseRetained ||
		stopped.ExecutionID != resumed.ExecutionID {
		t.Fatalf("stop of the resumed execution: %+v %v", stopped, err)
	}
}

// Resume takes a new deadline choice within the executor's cap; the owner's
// own work may have none when the executor sets no cap.
func TestResumeTakesAFreshDeadlineChoice(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.MaxDuration = 0
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if !v.Deadline.IsZero() {
		t.Fatalf("an uncapped owner session got a deadline: %v", v.Deadline)
	}
	resumed, err := o.Resume(t.Context(), v.ID, uuid.NewString(), "2h")
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(resumed.Deadline); until < time.Hour || until > 2*time.Hour {
		t.Fatalf("resume ignored its deadline choice: %v", resumed.Deadline)
	}

	capped, _ := fixture(t)
	owner := openOwner(t, capped)
	w := startInteractive(t, owner, interactiveRequest())
	if _, err = owner.Stop(t.Context(), w.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []string{deadline.NoneArg, "2h"} {
		if _, err = owner.Resume(t.Context(), w.ID, uuid.NewString(), choice); code(err) != profile.CodeDenied {
			t.Fatalf("resume beyond the executor's cap %q: %v", choice, err)
		}
	}
	if _, err = owner.Resume(t.Context(), w.ID, uuid.NewString(), "bogus"); code(err) != profile.CodeInvalid {
		t.Fatalf("invalid deadline choice: %v", err)
	}
}

// A pulled or cancelled session never resumes on the executor, and a task
// takes none of the interactive owner actions.
func TestResumeRefusesPulledAndCancelledSessions(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	pulled := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), pulled.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	transfer := uuid.NewString()
	if _, err := o.BeginExport(t.Context(), pulled.ID, transfer); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Resume(t.Context(), pulled.ID, uuid.NewString(), ""); code(err) != profile.CodeConflict {
		t.Fatalf("an exporting session resumed: %v", err)
	}
	if _, err := o.Claim(t.Context(), pulled.ID, transfer, p.meta.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Resume(t.Context(), pulled.ID, uuid.NewString(), ""); code(err) != profile.CodeConflict {
		t.Fatalf("a pulled session resumed: %v", err)
	}
	cancelled := startInteractive(t, o, interactiveRequest())
	if v, err := o.Cancel(t.Context(), cancelled.ID, uuid.NewString()); err != nil || v.Phase != phaseCancelled {
		t.Fatalf("cancel: %+v %v", v, err)
	}
	if _, err := o.Resume(t.Context(), cancelled.ID, uuid.NewString(), ""); code(err) != profile.CodeConflict {
		t.Fatalf("a cancelled session resumed: %v", err)
	}
	task := submit(t, o)
	waitPhase(t, o, task.ID, phaseRetained)
	if _, err := o.Resume(t.Context(), task.ID, uuid.NewString(), ""); code(err) != profile.CodeUnsupported {
		t.Fatalf("a task resumed as an interactive session: %v", err)
	}
	if _, err := o.Stop(t.Context(), task.ID, uuid.NewString()); code(err) != profile.CodeUnsupported {
		t.Fatalf("a task stopped by its owner: %v", err)
	}
	if _, err := o.Attach(t.Context(), task.ID); code(err) != profile.CodeUnsupported {
		t.Fatalf("a task attached: %v", err)
	}
}

// Only the home principal starts, cancels, stops, resumes or attaches to an
// interactive session. The work token keeps tasks.
func TestWorkTokenCannotUseInteractiveOwnerActions(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	if _, err := o.SubmitAs(t.Context(), CallerWorkToken, interactiveRequest()); code(err) != profile.CodeDenied {
		t.Fatalf("the work token started an interactive session: %v", err)
	}
	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.CancelAs(t.Context(), CallerWorkToken, v.ID, uuid.NewString()); code(err) != profile.CodeDenied {
		t.Fatalf("the work token cancelled an interactive session: %v", err)
	}
	token := strings.Repeat("t", 32)
	home := strings.Repeat("h", 32)
	h, err := NewHandler(o, token, homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	post := func(bearer, action, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/work/"+action, strings.NewReader(body))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	reference := `{"id":"` + v.ID + `","operationId":"` + uuid.NewString() + `"}`
	for _, action := range []string{"stop", "resume", "attach"} {
		if w := post(token, action, reference+"\n"); w.Code != http.StatusForbidden {
			t.Fatalf("the work token reached %s: %d %s", action, w.Code, w.Body.String())
		}
	}
	q, _ := json.Marshal(interactiveRequest())
	if w := post(token, "run", string(q)); w.Code != http.StatusForbidden {
		t.Fatalf("the work token started an interactive session over HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := post(token, "cancel", reference); w.Code != http.StatusForbidden {
		t.Fatalf("the work token cancelled an interactive session over HTTP: %d %s", w.Code, w.Body.String())
	}
	if w := post(home, "stop", reference); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"phase":"retained"`) {
		t.Fatalf("the home principal could not stop its session: %d %s", w.Code, w.Body.String())
	}
	resume := `{"id":"` + v.ID + `","operationId":"` + uuid.NewString() + `","maxDuration":"30s"}`
	if w := post(home, "resume", resume); w.Code != http.StatusOK {
		t.Fatalf("the home principal could not resume its session: %d %s", w.Code, w.Body.String())
	}
}

// attachServer serves the owner API over TLS 1.3, as the executor does, so
// an attachment streams in both directions.
func attachServer(t *testing.T, o *Owner, home string) *httptest.Server {
	t.Helper()
	h, err := NewHandler(o, strings.Repeat("t", 32), homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(h)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func attach(t *testing.T, server *httptest.Server, home, body string) (int, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/work/attach", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+home)
	r.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

// Attach names a session, never a launch: the executor finds the active
// visit in its own journal and carries the terminal's frames to it.
func TestAttachCarriesTerminalFramesToTheActiveVisit(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	home := strings.Repeat("h", 32)
	server := attachServer(t, o, home)
	frames := `{"op":"resize","rows":24,"columns":80}` + "\n" + `{"op":"write","data":"aGk="}` + "\n"
	status, output := attach(t, server, home, `{"id":"`+v.ID+`"}`+"\n"+frames)
	if status != http.StatusOK || output != frames {
		t.Fatalf("attachment: %d %q", status, output)
	}
	p.mu.Lock()
	attached := p.attached
	p.mu.Unlock()
	if len(attached) != 1 || string(attached[0]) != frames {
		t.Fatalf("the visit received %q", attached)
	}
	launch, err := json.Marshal(session.Launch{ExecutionID: v.ID})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"launch":    `{"id":"` + v.ID + `","launch":` + string(launch) + `}` + "\n",
		"malformed": `{"id":` + "\n",
		"unknown":   `{"id":"` + uuid.NewString() + `"}` + "\n",
	} {
		if status, output := attach(t, server, home, body); status != http.StatusBadRequest {
			t.Fatalf("%s envelope accepted: %d %s", name, status, output)
		}
	}
	status, output = attach(t, server, home, `{"id":"`+v.ID+`"}`+"\n"+`{"op":"configure","data":"aGk="}`+"\n")
	if status != http.StatusOK || !strings.Contains(output, "attachment unavailable") {
		t.Fatalf("a control frame reached the terminal: %d %q", status, output)
	}
	if _, err = o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if status, output = attach(t, server, home, `{"id":"`+v.ID+`"}`+"\n"); status != http.StatusConflict ||
		!strings.Contains(output, `"conflict"`) {
		t.Fatalf("attached to a stopped session: %d %s", status, output)
	}
}

// Attaching to a session whose harness has exited stops it and refuses.
func TestAttachRefusesASessionWhoseHarnessExited(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	p.mu.Lock()
	p.exited = map[string]bool{v.AllocationID: true}
	p.exit = &session.Exit{Code: 3, Cause: harness.NativeExitSelf}
	p.mu.Unlock()
	if _, err := o.Attach(t.Context(), v.ID); code(err) != profile.CodeConflict {
		t.Fatalf("attached to an exited harness: %v", err)
	}
	if got, err := o.Info(t.Context(), v.ID); err != nil || got.Phase != PhaseFailed {
		t.Fatalf("exited session: %+v %v", got, err)
	}
}

// symlinkArchive is a workspace archive whose first entry is a symbolic link
// rather than the manifest.
func symlinkArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// An uploaded workspace is validated like a moved checkpoint before anything
// is kept, and the session starts from it.
func TestInteractiveUploadIsValidatedLikeAMovedCheckpoint(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	upload, err := workspace.Capture(t.Context(), c.Repositories[repositorySproozi].Snapshot, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	original, err := workspace.Inspect(upload, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	q := interactiveRequest()
	q.Workspace = upload
	v := startInteractive(t, o, q)
	p.mu.Lock()
	input := p.inputs[v.AllocationID]
	p.mu.Unlock()
	if restored, err := workspace.Inspect(input, testLimit); err != nil || restored.Revision != original.Revision {
		t.Fatalf("the session did not start from its upload: %+v %v", restored, err)
	}
	if again, err := o.Submit(t.Context(), q); err != nil || again.ID != v.ID || again.AllocationID != v.AllocationID {
		t.Fatalf("upload replay: %+v %v", again, err)
	}
	for name, change := range map[string]func(*Submit){
		"oversize":  func(q *Submit) { q.Workspace = make([]byte, testLimit+1) },
		"malformed": func(q *Submit) { q.Workspace = []byte("not a workspace archive") },
		"symlink":   func(q *Submit) { q.Workspace = symlinkArchive(t) },
		"truncated": func(q *Submit) { q.Workspace = upload[:len(upload)/2] },
		"task":      func(q *Submit) { q.Mode, q.Task = "", fixtureTask },
		"revision": func(q *Submit) {
			q.WorkspaceFrom = &RevisionSource{SessionID: v.ID, Digest: strings.Repeat("0", 64), Revision: original.Revision}
		},
		"digest": func(q *Submit) { q.WorkspaceDigest = "sha256:" + strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := interactiveRequest()
			bad.Workspace = upload
			change(&bad)
			if _, err := o.Submit(t.Context(), bad); code(err) != profile.CodeInvalid {
				t.Fatalf("%s upload accepted: %v", name, err)
			}
		})
	}
	byToken := interactiveRequest()
	byToken.Workspace = upload
	if _, err = o.SubmitAs(t.Context(), CallerWorkToken, byToken); code(err) != profile.CodeDenied {
		t.Fatalf("the work token uploaded a workspace: %v", err)
	}
}

// The run route admits an upload up to the checkpoint limit, and only from
// the home principal; the work token keeps the task's smaller bound.
func TestRunBodyBoundsAnUpload(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.MaxCheckpointBytes = 1 << 20
	o := openOwner(t, c)
	token, home := strings.Repeat("t", 32), strings.Repeat("h", 32)
	h, err := NewHandler(o, token, homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	post := func(bearer string, size int) (int, string) {
		q := interactiveRequest()
		q.Workspace = make([]byte, size)
		body, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/work/run", bytes.NewReader(body))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	// Zero bytes are no archive: within the bound the upload reaches
	// validation; past it the route refuses the body before decoding it.
	if status, body := post(home, 1<<20); status != http.StatusBadRequest || body != `{"error":"invalid_request"}` {
		t.Fatalf("an upload at the limit: %d %s", status, body)
	}
	// One byte over the checkpoint limit still fits the body's slack for
	// the profile and the other fields; the owner refuses it.
	if status, body := post(home, 1<<20+1); status != http.StatusBadRequest || body != `{"error":"invalid_request"}` {
		t.Fatalf("an upload over the limit: %d %s", status, body)
	}
	if status, body := post(home, 1<<20+int(agentprofile.EncodedArchiveBytes)+32<<10); status != http.StatusBadRequest ||
		body != "Input exceeds bound" {
		t.Fatalf("an upload past the body bound: %d %s", status, body)
	}
	if status, body := post(token, 1<<20); status != http.StatusBadRequest || body != "Input exceeds bound" {
		t.Fatalf("the work token's bound admitted an upload: %d %s", status, body)
	}
	if list, err := o.List(t.Context(), SessionQuery{}); err != nil || len(list) != 0 {
		t.Fatalf("a refused upload kept a session: %+v %v", list, err)
	}
}

// The owner client attaches through its transport: the frames it sends reach
// the visit, and the visit's output comes back.
func TestClientAttachesThroughTheOwnerTransport(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	home := strings.Repeat("h", 32)
	server := attachServer(t, o, home)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport, err := profile.Connect(profile.Connection{Origin: server.URL, Token: home, Roots: roots,
		MaxCheckpointBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	client := ClientFor(transport)
	defer func() { _ = client.Close() }()
	frames := `{"op":"write","data":"aGk="}` + "\n"
	output, err := client.Attach(t.Context(), v.ID, strings.NewReader(frames))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	_ = output.Close()
	if err != nil || string(data) != frames {
		t.Fatalf("attachment: %q %v", data, err)
	}
	if _, err = ClientFor(noTerminal{}).Attach(t.Context(), v.ID, strings.NewReader("")); err == nil {
		t.Fatal("a transport without attachments attached")
	}
}

// noTerminal is an owner transport that cannot attach.
type noTerminal struct{}

func (noTerminal) CallOwner(context.Context, string, any, any) error { return nil }
func (noTerminal) Close()                                            {}
