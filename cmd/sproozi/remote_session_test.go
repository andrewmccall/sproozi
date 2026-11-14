//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

const remoteTestID = "4215d7d1-68ff-47ed-9dad-dbf1c8317d38"

// ownerCall is one request the fake executor received.
type ownerCall struct {
	action string
	body   []byte
	// frames are an attachment's input frames, after its envelope.
	frames string
}

// fakeExecutor is a remote executor's owner API: each action answers from
// its queue of replies, or with the session's view.
type fakeExecutor struct {
	mu      sync.Mutex
	calls   []ownerCall
	view    work.View
	replies map[string][]func(http.ResponseWriter)
}

func (f *fakeExecutor) reply(action string, answer func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[action] = append(f.replies[action], answer)
}

func (f *fakeExecutor) answerView(v work.View) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func answerStatus(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func (f *fakeExecutor) recorded(action string) []ownerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ownerCall
	for _, c := range f.calls {
		if c.action == action {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeExecutor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/work/")
	if action == "attach" {
		f.attach(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, ownerCall{action: action, body: body})
	var answer func(http.ResponseWriter)
	if queue := f.replies[action]; len(queue) > 0 {
		answer, f.replies[action] = queue[0], queue[1:]
	}
	view := f.view
	f.mu.Unlock()
	if answer == nil {
		answer = f.answerView(view)
		if action == "list" {
			answer = func(w http.ResponseWriter) { _ = json.NewEncoder(w).Encode([]work.View{view}) }
		}
	}
	answer(w)
}

// attach admits the attachment, acknowledges each write as a native
// terminal does and echoes it as output, until the client ends its input.
func (f *fakeExecutor) attach(w http.ResponseWriter, r *http.Request) {
	reader := bufio.NewReader(r.Body)
	envelope, err := profile.EnvelopeLine(reader)
	if err != nil || !profile.FullDuplex(w) {
		http.Error(w, "bad attachment", http.StatusBadRequest)
		return
	}
	output, err := profile.OpenTerminalOutput(w)
	if err != nil {
		return
	}
	_, _ = io.WriteString(output, "{\"op\":\"attach\",\"state\":\"active\"}\n")
	var frames strings.Builder
	scanner := bufio.NewScanner(profile.NewFrameReader(reader))
	for scanner.Scan() {
		frames.WriteString(scanner.Text() + "\n")
		var frame struct {
			Op   string `json:"op"`
			Data []byte `json:"data"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &frame)
		if frame.Op == "write" {
			ack, _ := json.Marshal(map[string]any{"op": "write", "written": len(frame.Data)})
			echo, _ := json.Marshal(map[string]any{"op": "output", "data": frame.Data})
			// The echo comes first, so it is written before the acknowledgement
			// that lets a detaching client end the attachment.
			_, _ = output.Write(append(append(echo, '\n'), append(ack, '\n')...))
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, ownerCall{action: "attach", body: envelope, frames: frames.String()})
	f.mu.Unlock()
}

// serveFakeExecutor replaces the remote seam with an executor holding view.
func serveFakeExecutor(t *testing.T, view work.View) *fakeExecutor {
	t.Helper()
	f := &fakeExecutor{view: view, replies: map[string][]func(http.ResponseWriter){}}
	server := httptest.NewUnstartedServer(f)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	previous := resolveRemote
	resolveRemote = func(context.Context, string, string) (remoteTarget, error) {
		client, err := profile.Connect(profile.Connection{Origin: server.URL, Token: strings.Repeat("t", 32),
			Roots: roots, MaxCheckpointBytes: 1 << 20})
		if err != nil {
			return remoteTarget{}, err
		}
		return remoteTarget{Name: testExecutor, Client: work.ClientFor(client), MaxCheckpointBytes: 1 << 20}, nil
	}
	t.Cleanup(func() { resolveRemote = previous })
	// Remote commands read no installation on this machine.
	t.Setenv("SPROOZI_ROOT", filepath.Join(t.TempDir(), "absent"))
	return f
}

func interactiveView[P ~string](phase P) work.View {
	return work.View{ID: remoteTestID, ExecutionID: remoteTestID, AllocationID: uuid.NewString(),
		Mode: work.ModeInteractive, Harness: "codex", Phase: work.Phase(phase)}
}

// A --remote run without a task is an interactive session; one with a task
// stays a task, and combinations that mix them are refused.
func TestRemoteRunWithoutATaskIsInteractive(t *testing.T) {
	q, err := parse([]string{cmdRun, "--remote", "--repository", "sproozi"})
	if err != nil || q.task != "" || q.path != "" || !q.remote {
		t.Fatalf("interactive run: %+v %v", q, err)
	}
	q, err = parse([]string{cmdRun, "--remote", "--project", "sproozi-dev", "repo", "-d", "--include-ignored", ".env"})
	if wd, _ := os.Getwd(); err != nil || q.path != filepath.Join(wd, "repo") || !q.detach ||
		!slices.Equal(q.includeIgnored, []string{".env"}) {
		t.Fatalf("upload run: %+v %v", q, err)
	}
	if _, err = parse([]string{cmdResume, "abc", "--remote", "--max-duration", deadline.NoneArg, "-d"}); err != nil {
		t.Fatalf("remote resume: %v", err)
	}
	t.Setenv("SPROOZI_ROOT", filepath.Join(t.TempDir(), "absent"))
	for want, args := range map[string][]string{
		"a --remote run needs --repository NAME or --project ID": {"run", "--remote"},
		"a --remote task works in the cluster's approved repository; leave out PATH": {
			"run", "--remote", "--repository", "r", "repo", "--task", "x"},
		"--detach applies to an interactive --remote run": {"run", "--remote", "--repository", "r", "--task", "x", "-d"},
		"--include-ignored applies only to a PATH this command uploads": {
			"run", "--remote", "--repository", "r", "--include-ignored", ".env"},
		"choose PATH or --workspace-from": {"run", "--remote", "--repository", "r", "repo", "--workspace-from",
			remoteTestID, "--checkpoint-digest", "sha256:x", "--revision", "abc"},
		"a session in a cluster takes no agent arguments": {"resume", "abc", "--remote", "--", "-x"},
		"--login applies to a session on this machine":    {"run", "--remote", "--repository", "r", "--login", "x"},
	} {
		code, _, stderr := runCLI(args...)
		if code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("%v exited %d without %q: %q", args, code, want, stderr)
		}
	}
}

// Ctrl-] ends a remote attachment and leaves the session running: the
// executor received the typed input and no stop or cancellation.
func TestRemoteAttachDetachesOnCtrlBracketAndLeavesTheSessionRunning(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseRunning))
	target, err := resolveRemote(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	q := &options{remote: true}
	var output bytes.Buffer
	sizes := make(chan home.TerminalSize, 1)
	sizes <- home.TerminalSize{Rows: 24, Columns: 80}
	attached := false
	connect := q.remoteAttachment(target, remoteTestID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = connect(ctx, strings.NewReader("hi\x1dignored"), &output, sizes, func() { attached = true })
	if err != nil || !attached {
		t.Fatalf("attachment ended with %v (attached %v)", err, attached)
	}
	until := time.Now().Add(5 * time.Second)
	for len(f.recorded("attach")) == 0 && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := f.recorded("attach")
	if len(calls) != 1 || string(calls[0].body) != `{"id":"`+remoteTestID+"\"}\n" ||
		!strings.Contains(calls[0].frames, `{"op":"write","data":"aGk="}`) || strings.Contains(calls[0].frames, "aWdub3Jl") {
		t.Fatalf("executor received %+v", calls)
	}
	if output.String() != "hi" {
		t.Fatalf("terminal output %q", output.String())
	}
	if len(f.recorded("stop")) != 0 || len(f.recorded("cancel")) != 0 {
		t.Fatal("detaching stopped the session")
	}
	if detachedLine(remoteTestID, q.remoteFlags()) != "Left session "+remoteTestID+
		". Back to it: sproozi attach 4215d7d1 --remote   Stop it: sproozi stop 4215d7d1 --remote   "+
		"List: sproozi ls" {
		t.Fatalf("detached line %q", detachedLine(remoteTestID, q.remoteFlags()))
	}
}

// A remote session is attached only once it runs; one that stopped is named
// with the command that continues it.
func TestRemoteAttachWaitsForTheSessionToStart(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseRunning))
	f.reply("info", f.answerView(interactiveView(phaseStarting)))
	f.reply("info", f.answerView(interactiveView(phaseActivating)))
	target, err := resolveRemote(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	previous := remoteStartPoll
	remoteStartPoll = time.Millisecond
	t.Cleanup(func() { remoteStartPoll = previous })
	q := &options{remote: true, cmd: command{name: cmdAttach}}
	stderr := captureStderr(t, func() {
		if err = q.waitActive(t.Context(), target, remoteTestID); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(stderr, "Waiting for session 4215d7d1 to start") || len(f.recorded("info")) != 3 {
		t.Fatalf("waited %d times: %q", len(f.recorded("info")), stderr)
	}
	stopped := interactiveView(work.PhaseRetained)
	f.reply("info", f.answerView(stopped))
	err = q.waitActive(t.Context(), target, remoteTestID)
	want := `session ` + remoteTestID + ` is stopped; "sproozi resume 4215d7d1 --remote" continues it`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("stopped session: %v", err)
	}
}

// stop waits for the agent's turn: while the executor answers stopping, the
// CLI says what it waits for and repeats the same operation.
func TestRemoteStopShowsTheWaitingNoticeAndRepeatsTheSameOperation(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseRunning))
	waiting := interactiveView(phaseStopping)
	waiting.Problem = "the stop is waiting for the agent's current turn"
	f.reply("stop", f.answerView(waiting))
	f.reply("stop", f.answerView(waiting))
	f.reply("stop", f.answerView(interactiveView(work.PhaseRetained)))
	previous := remoteStopRetry
	remoteStopRetry = time.Millisecond
	t.Cleanup(func() { remoteStopRetry = previous })
	var code int
	var out, errOut string
	stderr := captureStderr(t, func() { code, out, errOut = runCLI("stop", "4215", "--remote") })
	if code != 0 || !strings.Contains(out, "Stopped session "+remoteTestID+"; its work is kept.") ||
		!strings.Contains(out, "sproozi resume 4215d7d1 --remote") {
		t.Fatalf("stop exited %d: %s %s", code, out, errOut)
	}
	if strings.Count(stderr, "Waiting for the agent's current turn in session 4215d7d1 to finish; "+
		"run \"sproozi cancel 4215d7d1 --remote\" in another terminal to stop it now instead.") != 1 {
		t.Fatalf("notice %q", stderr)
	}
	stops := f.recorded("stop")
	operations := make([]string, 0, len(stops))
	for _, c := range stops {
		var r struct {
			ID          string `json:"id"`
			OperationID string `json:"operationId"`
		}
		if err := json.Unmarshal(c.body, &r); err != nil || r.ID != remoteTestID {
			t.Fatalf("stop body %s", c.body)
		}
		operations = append(operations, r.OperationID)
	}
	if len(operations) != 3 || !work.CanonicalID(operations[0]) || operations[1] != operations[0] ||
		operations[2] != operations[0] {
		t.Fatalf("stop operations %v", operations)
	}
}

// Each resume is a new operation; a repeated one names its operation.
func TestRemoteResumeUsesANewOperation(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(work.PhaseRetained))
	resumed := interactiveView(phaseStarting)
	resumed.ExecutionID = uuid.NewString()
	f.reply("resume", f.answerView(resumed))
	f.reply("resume", f.answerView(resumed))
	for range 2 {
		if code, out, errOut := runCLI("resume", "4215", "--remote", "-d", "--max-duration", "2h"); code != 0 ||
			!strings.Contains(out, "Resumed session "+remoteTestID) ||
			!strings.Contains(out, "Connect with: sproozi attach 4215d7d1 --remote") {
			t.Fatalf("resume exited %d: %s %s", code, out, errOut)
		}
	}
	resumes := f.recorded("resume")
	operations := make([]string, 0, len(resumes))
	for _, c := range resumes {
		var r struct {
			ID          string `json:"id"`
			OperationID string `json:"operationId"`
			MaxDuration string `json:"maxDuration"`
		}
		if err := json.Unmarshal(c.body, &r); err != nil || r.ID != remoteTestID || r.MaxDuration != "2h" {
			t.Fatalf("resume body %s", c.body)
		}
		operations = append(operations, r.OperationID)
	}
	if len(operations) != 2 || operations[0] == operations[1] || !work.CanonicalID(operations[0]) ||
		operations[0] == remoteTestID {
		t.Fatalf("resume operations %v", operations)
	}
	// A dropped answer names the operation to repeat.
	f.reply("resume", answerStatus(http.StatusServiceUnavailable, `{"error":"unavailable"}`))
	op := uuid.NewString()
	code, _, errOut := runCLI("resume", "4215", "--remote", "-d", "--operation", op)
	if code != 1 || !strings.Contains(errOut, "repeat the command with --operation "+op) {
		t.Fatalf("lost resume %d: %s", code, errOut)
	}
}

// gitRepository makes a committed repository at a new directory, with Git
// reading no configuration of the developer's.
func gitRepository(t *testing.T, commit bool) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if commit {
		run("add", "README.md", ".gitignore")
		run("commit", "--quiet", "-m", "first")
	}
	return dir
}

// run --remote PATH captures the repository as a local run does and uploads
// it; the repository still names what the session belongs to.
func TestRemoteRunUploadsTheLocalRepository(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseStarting))
	dir := gitRepository(t, true)
	code, out, errOut := runCLI("run", "--remote", "--repository", "sproozi", dir, "-d", "--include-ignored", ".env",
		"--max-duration", deadline.NoneArg)
	if code != 0 || !strings.Contains(out, "Started session "+remoteTestID) {
		t.Fatalf("run exited %d: %s %s", code, out, errOut)
	}
	runs := f.recorded("run")
	if len(runs) != 1 {
		t.Fatalf("submitted %d runs", len(runs))
	}
	var submitted work.Submit
	if err := json.Unmarshal(runs[0].body, &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.Mode != work.ModeInteractive || submitted.Task != "" || submitted.Repository != "sproozi" ||
		submitted.MaxDuration != deadline.NoneArg || len(submitted.Workspace) == 0 || !work.CanonicalID(submitted.RequestID) {
		t.Fatalf("submission %+v", submitted)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := workspace.Restore(t.Context(), submitted.Workspace, restored, 1<<20); err != nil {
		t.Fatalf("upload is not a valid capture: %v", err)
	}
	for _, name := range []string{"README.md", ".env"} {
		if _, err := os.Stat(filepath.Join(restored, name)); err != nil {
			t.Fatalf("upload lost %s: %v", name, err)
		}
	}
}

// A repository a local run refuses is refused before anything is sent.
func TestRemoteRunRefusesAnUnsupportedRepository(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseStarting))
	dir := gitRepository(t, false)
	code, _, errOut := runCLI("run", "--remote", "--repository", "sproozi", dir)
	if code != 1 || !strings.Contains(errOut, "the repository has no commits; make a first commit in "+dir) {
		t.Fatalf("unborn repository: %d %s", code, errOut)
	}
	code, _, errOut = runCLI("run", "--remote", "--repository", "sproozi", t.TempDir())
	if code != 1 || !strings.Contains(errOut, "ordinary Git directory required") {
		t.Fatalf("plain directory: %d %s", code, errOut)
	}
	if len(f.recorded("run")) != 0 {
		t.Fatal("an unsupported repository was submitted")
	}
}

// An interactive run that does not attach still names its session.
func TestRemoteRunWithoutATerminalDoesNotAttach(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseStarting))
	var code int
	var out string
	stderr := captureStderr(t, func() { code, out, _ = runCLI("run", "--remote", "--repository", "sproozi") })
	if code != 0 || !strings.Contains(out, "Started session "+remoteTestID) ||
		!strings.Contains(stderr, "did not attach; run \"sproozi attach 4215d7d1 --remote\" from a terminal") {
		t.Fatalf("run exited %d: %s %s", code, out, stderr)
	}
	if len(f.recorded("attach")) != 0 {
		t.Fatal("attached without a terminal")
	}
}

// An executor older than interactive sessions refuses them; the CLI says to
// update it, not to retry.
func TestOldExecutorNamesTheUpdate(t *testing.T) {
	f := serveFakeExecutor(t, interactiveView(phaseRunning))
	f.reply("run", answerStatus(http.StatusBadRequest, `{"error":"invalid_request"}`))
	code, _, errOut := runCLI("run", "--remote", "--repository", "sproozi", "-d")
	if code != 1 || !strings.Contains(errOut, "update the executor") || strings.Contains(errOut, "--operation") {
		t.Fatalf("old executor run: %d %s", code, errOut)
	}
	f.reply("stop", answerStatus(http.StatusNotFound, "404 page not found"))
	code, _, errOut = runCLI("stop", remoteTestID, "--remote")
	if code != 1 || !strings.Contains(errOut, "older than remote stop") ||
		!strings.Contains(errOut, "update the executor") {
		t.Fatalf("old executor stop: %d %s", code, errOut)
	}
}

// A task takes no stop, resume or attach; the CLI says how a task ends.
func TestRemoteTaskRefusesInteractiveActions(t *testing.T) {
	task := interactiveView(phaseRunning)
	task.Mode = ""
	f := serveFakeExecutor(t, task)
	f.reply("stop", answerStatus(http.StatusUnprocessableEntity, `{"error":"unsupported"}`))
	code, _, errOut := runCLI("stop", remoteTestID, "--remote")
	if code != 1 || !strings.Contains(errOut, "is a task") {
		t.Fatalf("task stop: %d %s", code, errOut)
	}
}

// A remote status names an interactive session and its agent's failed exit.
func TestRemoteStatusShowsModeAndExit(t *testing.T) {
	failed := interactiveView(work.PhaseFailed)
	failed.Exit = &session.Exit{Code: 2, Output: "bad flag"}
	serveFakeExecutor(t, failed)
	code, out, errOut := runCLI("status", remoteTestID, "--remote")
	if code != 0 || !strings.Contains(out, "Mode       interactive") || !strings.Contains(out, "Exited     code 2") ||
		!strings.Contains(out, "bad flag") {
		t.Fatalf("status exited %d: %s %s", code, out, errOut)
	}
}
