package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

var updateLifecycleRecord = flag.Bool("update-lifecycle-record", false,
	"rewrite "+lifecycleRecord+" from the supervisor's behaviour")

// lifecycleRecord holds what the supervisor did for every event in every
// state it can reach. It changes only with an intended behaviour change.
const lifecycleRecord = "testdata/supervisor-lifecycle.txt"

// recordedHarness stands in for every harness: a line typed into its
// terminal, such as Codex's quit, ends it with code 0, and SIGUSR1 makes it
// exit by itself with code 3. It prints only "ready", once its terminal no
// longer echoes input.
const recordedHarness = `stty -echo; trap 'exit 3' USR1; printf ready; read -r line </dev/tty & wait $!; exit 0`

// The recorded harness's shell, and the names of two events other tests also
// name.
const (
	recordedShell      = "/bin/sh"
	recordedTerminated = "terminated"
	recordedDeadline   = "deadline"
)

// lateDeadline is how long after a late world's supervisor starts its
// deadline passes. Every event before it takes far less.
const lateDeadline = 4 * time.Second

// errRecordedTurn is the error the recorded harness's observer gives while no
// turn has completed.
var errRecordedTurn = errors.New("no completed native turn")

// world is what stays fixed for a supervisor's whole life: its harness,
// whether its visit is a first one (no selected conversation), whether its
// execution has a deadline, and whether that deadline passes during the
// events (late), with the supervisor's deadline watcher running.
type world struct {
	kind                  harness.NativeKind
	first, deadline, late bool
}

func (w world) String() string {
	visit, until := "resumed visit", "no deadline"
	if w.first {
		visit = "first visit"
	}
	if w.deadline {
		until = "a deadline"
	}
	if w.late {
		until = "a deadline that passes"
	}
	return string(w.kind) + ", " + visit + ", " + until
}

// recordDeadline is far enough away that it passes only when an event says
// so.
var recordDeadline = time.Now().Add(time.Hour)

// mode is how the recorded harness and its observer behave during one event.
type mode struct {
	// completed: the observer sees a completed turn of selected.
	completed bool
	// conversation: a first visit has a conversation, so it is not turnless.
	conversation bool
	// onObserve runs once, at the first observation.
	onObserve func(r *recording)
	// finish replaces the adapter's own finish.
	finish func(r *recording, n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error)
	// unconfirmed: the forced stop's kill is never confirmed.
	unconfirmed bool
}

// recording is one supervisor driven along one path of events.
type recording struct {
	t     *testing.T
	w     world
	until time.Time
	s     *server
	n     *nativepty.Supervisor
	login *loginSync
	// watched is closed when the exit watcher returns.
	watched chan struct{}
	kills   atomic.Int32
	// mode is read by the exit watcher too, when its report looks for the
	// conversation.
	modeMu sync.Mutex
	mode   mode
	// sent is what reached the harness during a step, in order; a kill can
	// come from the terminal's own goroutine.
	sentMu sync.Mutex
	sent   []string
	killed bool
	// pending: a stopped quiesce reply has not been read yet, so the
	// handler has not concluded.
	pending     bool
	reports     *lockedBuffer
	diagnostics *lockedBuffer
	reported    int
	diagnosed   int
}

// recordings finds the recording of a supervisor or of its harness, for the
// adapters and forced stop the record installs.
var recordings sync.Map

func recordingOf(key any) *recording {
	r, ok := recordings.Load(key)
	if !ok {
		panic(fmt.Sprintf("no recording for %T", key))
	}
	return r.(*recording)
}

// config is the world's launch contract.
func (r *recording) config() config {
	c := config{Harness: r.w.kind, Instructions: instructions, InstructionDigest: digest(),
		ClientConfig: testClients, Env: []string{}, RolloutMaxBytes: 8192, Deadline: r.until}
	if !r.w.first {
		c.ThreadID = selected
	}
	return c
}

func newRecording(t *testing.T, w world, bootstrap bool) *recording {
	r := &recording{t: t, w: w, reports: &lockedBuffer{}, diagnostics: &lockedBuffer{}}
	switch {
	case w.late:
		r.until = time.Now().Add(lateDeadline)
	case w.deadline:
		r.until = recordDeadline
	}
	c := r.config()
	if bootstrap {
		c = config{Deadline: c.Deadline}
	}
	r.s = newRecordedServer(c, bootstrap)
	r.s.reports, r.s.diagnostics = r.reports, r.diagnostics
	recordings.Store(r.s, r)
	if w.late {
		startWatcher(r.s)
	}
	return r
}

// close stops what the recording started.
func (r *recording) close() {
	defer recordings.Delete(r.s)
	if r.n == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = r.n.Stop(ctx)
	r.settle()
	recordings.Delete(r.n)
}

// settle waits for the exit watcher once the harness has exited, so every
// step ends with the supervisor at rest.
func (r *recording) settle() {
	if r.n == nil {
		return
	}
	select {
	case <-r.n.Done():
	default:
		return
	}
	select {
	case <-r.watched:
	case <-time.After(10 * time.Second):
		r.t.Fatal("the exit watcher did not return")
	}
}

func (r *recording) note(what string) {
	r.sentMu.Lock()
	defer r.sentMu.Unlock()
	r.sent = append(r.sent, what)
}

// loginState says what the login sync records: how far it trusts the
// harness's login file, and whether its final save has run.
func (r *recording) loginState() string {
	if r.login == nil {
		return "no login"
	}
	trust := map[int32]string{loginRunning: "running", loginStopping: "stopping",
		loginForced: "forced"}[r.login.trust()]
	select {
	case <-r.login.quit:
		return "login " + trust + ", saved"
	default:
		return "login " + trust
	}
}

// recordedLogin is a signed-in sign-in sync whose store and harness file
// agree, as activate leaves it, with no periodic sync during a step. Its
// waits are long, so a save the machine's load delays still finishes before
// the step ends.
func recordedLogin(t *testing.T) *loginSync {
	dir := t.TempDir()
	l := &loginSync{stored: filepath.Join(dir, "store.json"), native: filepath.Join(dir, "auth.json"),
		interval: quietInterval, quit: make(chan struct{}), stopped: make(chan struct{}),
		diagnostics: &lockedBuffer{}, wait: 10 * time.Second}
	write(t, l.stored, refreshedR3)
	write(t, l.native, refreshedR3)
	if err := l.sync(); err != nil {
		t.Fatal(err)
	}
	l.settled()
	t.Cleanup(func() {
		l.stop()
		<-l.stopped
	})
	return l
}

// launch is a successful activate: the sign-in sync starts, the harness
// starts on activate's terminal and the exit watcher runs. Activation itself
// needs a container, so the reply is the one activate gives.
func (r *recording) launch() response {
	l := recordedLogin(r.t)
	launch := recordedTerminal(r.s, []string{"-c", recordedHarness},
		[]string{testPathEnvironment, testTerminalEnvironment}, l)
	launch.Dir, launch.Size = r.t.TempDir(), nativepty.Size{Rows: 24, Columns: 80}
	recordKill := launch.BeforeKill
	launch.BeforeKill = func() {
		recordKill()
		r.kills.Add(1)
		r.sentMu.Lock()
		defer r.sentMu.Unlock()
		if !r.killed {
			r.killed = true
			r.sent = append(r.sent, "kill ("+r.loginState()+")")
		}
	}
	n, err := nativepty.Start(launch)
	if err != nil {
		r.t.Fatal(err)
	}
	for !strings.Contains(string(n.Tail(64)), "ready") {
		select {
		case <-n.Done():
			r.t.Fatal("the recorded harness exited before it was ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The harness has a login of its own that no sync has copied yet.
	write(r.t, l.native, unsavedLogin)
	r.n, r.login, r.watched = n, l, make(chan struct{})
	recordings.Store(n, r)
	r.s.mu.Lock()
	r.s.login = l
	if r.w.kind == harness.NativeCopilot && r.w.first {
		// activate names a new Copilot conversation.
		r.s.launchID = selected
	}
	r.s.mu.Unlock()
	launched(r.s, n)
	go func() {
		r.s.watch(n, l.stop)
		close(r.watched)
	}()
	r.s.mu.Lock()
	thread := r.s.threadID
	r.s.mu.Unlock()
	return response{Op: harness.NativeActivate, State: harness.NativeActive, ThreadID: thread, PID: n.PID()}
}

func (r *recording) current() mode {
	r.modeMu.Lock()
	defer r.modeMu.Unlock()
	return r.mode
}

func (r *recording) setMode(m mode) {
	r.modeMu.Lock()
	defer r.modeMu.Unlock()
	r.mode = m
}

func (r *recording) observe() (string, string, error) {
	r.modeMu.Lock()
	f := r.mode.onObserve
	r.mode.onObserve = nil
	completed := r.mode.completed
	r.modeMu.Unlock()
	if f != nil {
		f(r)
	}
	if completed {
		return selected, completedTurnID, nil
	}
	return "", "", errRecordedTurn
}

func (r *recording) finish(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
	if finish := r.current().finish; finish != nil {
		return finish(r, n, ctx)
	}
	return r.ownFinish(n, ctx)
}

// recordedFinishes are the adapters' own finishes: Codex's quit, and the
// kill that ends Claude Code and Copilot.
var recordedFinishes = map[harness.NativeKind]func(*nativepty.Supervisor, context.Context) (nativepty.Exit, error){}

// realStopLimit bounds a recorded harness's real quit or kill. The record's
// quitWait and forcedStopWait are short so that a finish the harness
// ignores, or a kill never confirmed, ends quickly; a real quit spends 300 ms
// before its Enter and a reap up to 100 ms more, so on a loaded machine it
// can outlast them and would be recorded as forced. A real quit or kill
// therefore gets its own real bound, as in production, rather than the
// supervisor's short one. No recorded world has a deadline nearer than this
// bound while a real quit or kill runs.
const realStopLimit = 10 * time.Second

// realStop is ctx without its cancellation, bounded by realStopLimit.
func realStop(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), realStopLimit)
}

// ownFinish is the adapter's own finish.
func (r *recording) ownFinish(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
	r.note("finish (" + r.loginState() + ")")
	ctx, cancel := realStop(ctx)
	defer cancel()
	exit, err := recordedFinishes[r.w.kind](n, ctx)
	r.settle()
	return exit, err
}

func (r *recording) forceStop(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
	if r.current().unconfirmed {
		r.note("forced stop, never confirmed")
		<-ctx.Done()
		return nativepty.Exit{}, ctx.Err()
	}
	r.note("forced stop")
	ctx, cancel := realStop(ctx)
	defer cancel()
	exit, err := n.Stop(ctx)
	r.settle()
	return exit, err
}

// exits makes the harness exit by itself, with code 3.
func (r *recording) exits() {
	untilExited(r.t, r.n)
	r.settle()
}

// untilExited makes n's harness exit by itself, with code 3. The shell can
// lose a SIGUSR1 that arrives before its trap or its wait, so the signal is
// sent again every 50 ms until the harness has exited.
func untilExited(t *testing.T, n *nativepty.Supervisor) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	limit := time.After(10 * time.Second)
	for {
		_ = unix.Kill(n.PID(), unix.SIGUSR1)
		select {
		case <-n.Done():
			return
		case <-limit:
			t.Fatal("the harness did not exit")
		case <-ticker.C:
		}
	}
}

func (r *recording) alive() bool {
	if r.n == nil {
		return false
	}
	select {
	case <-r.n.Done():
		return false
	default:
		return true
	}
}

func (r *recording) concluded() bool {
	select {
	case <-r.s.done:
		return true
	default:
		return false
	}
}

// reply is a control reply as the client reads it, without the process ID.
func reply(q response) string {
	if q.PID != 0 {
		q.PID = 1
	}
	data, err := json.Marshal(q)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (r *recording) quiesce(m mode) string {
	r.setMode(m)
	q := r.s.quiesce()
	r.setMode(mode{})
	if q.State == harness.NativeStopped {
		r.pending = true
	}
	return reply(q)
}

// input attaches as the CLI does and writes one byte.
func (r *recording) input() string {
	client, backend := net.Pipe()
	handled := make(chan struct{})
	go func() {
		r.s.handle(backend)
		close(handled)
	}()
	defer func() {
		_ = client.Close()
		<-handled
	}()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	encoder, decoder := json.NewEncoder(client), json.NewDecoder(client)
	if err := encoder.Encode(request{Op: harness.NativeAttach}); err != nil {
		r.t.Fatal(err)
	}
	var first response
	if err := decoder.Decode(&first); err != nil {
		r.t.Fatal(err)
	}
	said := "attach " + reply(first)
	if first.Error != "" {
		return said
	}
	if err := encoder.Encode(request{Op: opWrite, Data: []byte("x")}); err != nil {
		r.t.Fatal(err)
	}
	for {
		var written response
		if err := decoder.Decode(&written); err != nil {
			r.t.Fatal(err)
		}
		if written.Op == opWrite {
			return said + ", write " + reply(written)
		}
	}
}

// hook is Claude Code's UserPromptSubmit for the selected conversation.
func (r *recording) hook() string {
	path, err := harness.ClaudeProjectPath(workspace, selected)
	if err != nil {
		r.t.Fatal(err)
	}
	return reply(r.s.claudeHook(&claudeHook{SessionID: selected, TranscriptPath: filepath.Join(claudeHome, path),
		Cwd: workspace, Event: claudePromptSubmit, Prompt: "continue"}))
}

// completedTurn is a quiesce that observes a completed turn.
func completedTurn() mode { return mode{completed: true, conversation: true} }

// ignoresQuitMode is a completed turn whose harness ignores its finish.
func ignoresQuitMode() mode {
	m := completedTurn()
	m.finish = func(r *recording, _ *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		r.note("finish, ignored")
		<-ctx.Done()
		return nativepty.Exit{}, ctx.Err()
	}
	return m
}

// during is a completed turn in which something happens while the
// supervisor waits for the harness to finish.
func during(event func(r *recording)) mode {
	m := completedTurn()
	m.finish = func(r *recording, n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		event(r)
		r.settle()
		return r.ownFinish(n, ctx)
	}
	return m
}

// recordedEvent is one event the record applies in every state. A nil
// result means the event cannot happen in that state or world. Only events
// marked late happen in a late world, whose deadline would race the rest.
type recordedEvent struct {
	name  string
	late  bool
	apply func(r *recording) *string
}

func said(s string) *string { return &s }

var recordedEvents = []recordedEvent{
	{"configure", true, func(r *recording) *string {
		c := r.config()
		return said(reply(r.s.configure(&c)))
	}},
	{"activate", true, func(r *recording) *string {
		if wireState(r.s) == harness.NativePrepared {
			return said(reply(r.launch()))
		}
		return said(reply(r.s.activate()))
	}},
	{"gate", true, func(r *recording) *string { return said(reply(r.s.gate())) }},
	{"status", true, func(r *recording) *string { return said(reply(r.s.status())) }},
	{"input", true, func(r *recording) *string { return said(r.input()) }},
	{"hook", true, func(r *recording) *string { return said(r.hook()) }},
	{"quiesce", false, func(r *recording) *string { return said(r.quiesce(mode{})) }},
	{"quiesce, turn completed", false, func(r *recording) *string { return said(r.quiesce(completedTurn())) }},
	{"quiesce, finish ignored", false, func(r *recording) *string { return said(r.quiesce(ignoresQuitMode())) }},
	{"quiesce, finish ignored and kill never confirmed", false, func(r *recording) *string {
		m := ignoresQuitMode()
		m.unconfirmed = true
		return said(r.quiesce(m))
	}},
	{"quiesce, harness exits while waiting for its turn", false, func(r *recording) *string {
		return said(r.quiesce(mode{conversation: true, onObserve: (*recording).exits}))
	}},
	{"quiesce, deadline passes while waiting for its turn", false, func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		deadlineIn(r.s, 200*time.Millisecond)
		return said(r.quiesce(mode{conversation: true}))
	}},
	{"quiesce, deadline passes while waiting for the finish", false, func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		deadlineIn(r.s, 200*time.Millisecond)
		return said(r.quiesce(ignoresQuitMode()))
	}},
	{"quiesce, terminated while finishing", false, func(r *recording) *string {
		return said(r.quiesce(during(func(r *recording) { terminate(r.s) })))
	}},
	{"quiesce, harness exits while finishing", false, func(r *recording) *string {
		return said(r.quiesce(during((*recording).exits)))
	}},
	{"quiesce, deadline raised while finishing", false, func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		return said(r.quiesce(during(func(r *recording) { raiseDeadline(r.s) })))
	}},
	{"harness exits", false, func(r *recording) *string {
		if !r.alive() {
			return nil
		}
		r.exits()
		return said("")
	}},
	{recordedTerminated, false, func(r *recording) *string {
		terminate(r.s)
		return said("")
	}},
	{recordedDeadline, false, func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		raiseDeadline(r.s)
		return said("")
	}},
	{"the deadline passes", true, func(r *recording) *string {
		if !r.w.late {
			return nil
		}
		select {
		case <-r.s.done:
		case <-time.After(10 * time.Second):
			r.t.Fatal("the supervisor did not stop at its deadline")
		}
		return said("")
	}},
	{"reply read", false, func(r *recording) *string {
		if !r.pending {
			return nil
		}
		r.pending = false
		r.s.conclude()
		return said("")
	}},
}

// causeWords name the exit causes.
var causeWords = map[exitCause]string{causeNone: "no cause", causeSelf: "cause exited",
	causeQuiesced: "cause quiesced", causeDeadline: "cause deadline", causeTerminated: "cause terminated",
	causeFailed: "cause failed"}

// snapshot is the supervisor at rest: what can be seen of it, and the facts
// its lifecycle keeps.
func (r *recording) snapshot() string {
	var b strings.Builder
	fmt.Fprintf(&b, "state %s", wireState(r.s))
	r.s.mu.Lock()
	thread := r.s.threadID
	r.s.mu.Unlock()
	if thread != "" {
		fmt.Fprintf(&b, ", thread %s", thread)
	}
	cause, gated, turnless := lifeFacts(r.s)
	b.WriteString(", " + causeWords[cause])
	if gated {
		b.WriteString(", gated")
	}
	if turnless {
		b.WriteString(", turnless")
	}
	switch {
	case r.n == nil:
		b.WriteString(", no harness")
	case r.alive():
		b.WriteString(", harness running")
	default:
		b.WriteString(", harness exited")
	}
	fmt.Fprintf(&b, ", input %s, %s", r.inputProbe(), r.loginState())
	if r.login != nil {
		switch stored, _ := os.ReadFile(r.login.stored); string(stored) {
		case refreshedR3:
			b.WriteString(", store unchanged")
		case unsavedLogin:
			b.WriteString(", store has the harness's login")
		default:
			fmt.Fprintf(&b, ", store %q", stored)
		}
	}
	if r.pending && !r.concluded() {
		b.WriteString(", stopped reply unread")
	}
	if r.concluded() {
		fmt.Fprintf(&b, ", supervisor exited %d", supervisorExitCode(r.s))
	}
	return b.String()
}

// inputProbe says whether terminal input would reach the harness.
func (r *recording) inputProbe() string {
	if r.n == nil {
		return "none"
	}
	a, err := r.n.Attach(0)
	if err != nil {
		return "attach failed: " + err.Error()
	}
	defer func() { _ = a.Close() }()
	switch _, err = a.Write(nil); {
	case err == nil:
		return "open"
	case errors.Is(err, nativepty.ErrGated):
		return "gated"
	case errors.Is(err, nativepty.ErrExited):
		return "closed"
	default:
		return err.Error()
	}
}

// effects is what one step did beyond its reply: what reached the harness,
// how it ended, and the lines the supervisor wrote. In a late world the
// supervisor's watcher and, before, the terminal's own deadline race to
// kill, so only that it was killed is recorded.
func (r *recording) effects(kills int32, harnessWasAlive bool) []string {
	var out []string
	r.sentMu.Lock()
	if len(r.sent) > 0 {
		out = append(out, "sent "+strings.Join(r.sent, ", "))
	}
	r.sentMu.Unlock()
	if k := r.kills.Load() - kills; k > 0 && !r.w.late {
		out = append(out, fmt.Sprintf("killed %d", k))
	}
	if harnessWasAlive && !r.alive() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		exit, err := r.n.Wait(ctx)
		cancel()
		reason := string(exit.Reason)
		if r.w.late && exit.Reason != nativepty.NativeExit {
			reason = "killed"
		}
		line := fmt.Sprintf("harness ended %d %s", exit.Code, reason)
		if err != nil {
			line += " unconfirmed: " + err.Error()
		}
		out = append(out, line)
	}
	lines := func(b *lockedBuffer, seen *int, prefix string) {
		all := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
		if len(all) == 1 && all[0] == "" {
			all = nil
		}
		for _, line := range all[*seen:] {
			out = append(out, prefix+line)
		}
		*seen = len(all)
	}
	lines(r.reports, &r.reported, "out ")
	lines(r.diagnostics, &r.diagnosed, "log ")
	return out
}

// step applies event e and returns its record, and whether it happened.
func (r *recording) step(e recordedEvent) ([]string, bool) {
	if r.w.late && !e.late {
		return nil, false
	}
	r.sentMu.Lock()
	r.sent, r.killed = nil, false
	r.sentMu.Unlock()
	kills, alive := r.kills.Load(), r.alive()
	result := e.apply(r)
	if result == nil {
		return nil, false
	}
	r.settle()
	out := []string{}
	if *result != "" {
		out = append(out, "reply "+*result)
	}
	out = append(out, r.effects(kills, alive)...)
	return append(out, "then "+r.snapshot()), true
}

// node is a state the record reached, by the events of path from a start.
type node struct {
	bootstrap bool
	path      []int
	// rest is the snapshot after each event of path, to check a replay.
	rest []string
}

func (n node) describe() string {
	start := "configured"
	if n.bootstrap {
		start = "bootstrapped"
	}
	names := make([]string, 0, 1+len(n.path))
	names = append(names, start)
	for _, i := range n.path {
		names = append(names, recordedEvents[i].name)
	}
	return strings.Join(names, " > ")
}

// replay drives a new supervisor along n's path.
func replay(t *testing.T, w world, n node) *recording {
	r := newRecording(t, w, n.bootstrap)
	for i, e := range n.path {
		if _, ok := r.step(recordedEvents[e]); !ok {
			t.Fatalf("%s: step %d did not happen on replay", n.describe(), i)
		}
		if got := r.snapshot(); got != n.rest[i] {
			t.Fatalf("%s: step %d replayed as %q, recorded %q", n.describe(), i, got, n.rest[i])
		}
	}
	return r
}

// recordWorld applies every event to every state reached in w, breadth
// first from both starts, and returns the record and the number of steps.
// States are told apart by everything the snapshot holds.
func recordWorld(t *testing.T, w world) (string, int) {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", w)
	seen := map[string]bool{}
	var frontier []node
	for _, bootstrap := range []bool{true, false} {
		n := node{bootstrap: bootstrap}
		r := replay(t, w, n)
		at := r.snapshot()
		r.close()
		fmt.Fprintf(&b, "\n%s: %s\n", n.describe(), at)
		seen[at] = true
		frontier = append(frontier, n)
	}
	steps := 0
	for len(frontier) > 0 {
		var next []node
		for _, from := range frontier {
			for i, e := range recordedEvents {
				r := replay(t, w, from)
				out, ok := r.step(e)
				at := r.snapshot()
				ended := r.concluded()
				r.close()
				if !ok {
					continue
				}
				steps++
				fmt.Fprintf(&b, "\n%s > %s\n", from.describe(), e.name)
				for _, line := range out {
					fmt.Fprintf(&b, "  %s\n", line)
				}
				if seen[at] || ended {
					continue
				}
				seen[at] = true
				next = append(next, node{bootstrap: from.bootstrap, path: append(slices.Clone(from.path), i),
					rest: append(slices.Clone(from.rest), at)})
			}
		}
		frontier = next
	}
	return b.String(), steps
}

// recordedWorlds are, for each managed kind in registry order, its first and
// resumed visits with a deadline, and for the default kind (Codex) also
// without one; then a default-kind world whose deadline passes.
func recordedWorlds() []world {
	var worlds []world
	for _, kind := range harness.ManagedKinds() {
		if kind == harness.Default() {
			worlds = append(worlds, world{kind: kind}, world{kind: kind, deadline: true}, world{kind: kind, first: true},
				world{kind: kind, first: true, deadline: true})
			continue
		}
		worlds = append(worlds, world{kind: kind, deadline: true}, world{kind: kind, first: true, deadline: true})
	}
	return append(worlds, world{kind: harness.Default(), deadline: true, late: true})
}

// The supervisor's behaviour, for every event in every state it reaches, is
// the recorded one: each control reply, the state status reports and the
// lifecycle's facts (cause, gating, the turnless latch), what reached the
// harness and what the sign-in sync had recorded when it did, how the
// harness ended, the login store, the exit report and log lines, and the
// supervisor's exit code.
func TestSupervisorLifecycleRecord(t *testing.T) {
	if codexJournalExists(codexHome) {
		t.Skip("a real Codex home is present")
	}
	for _, kind := range harness.ManagedKinds() {
		// A missing function would panic a world and hide every other
		// test's report in this package.
		if adapter, err := adapterFor(kind); err != nil || len(adapterProblems(adapter)) != 0 {
			t.Fatalf("managed harness %q has no supervisor adapter with every required function, so its lifecycle "+
				"cannot be recorded; TestEveryManagedKindHasAnAdapter and TestEveryAdapterSetsEveryRequiredFunction "+
				"name what to add", kind)
		}
		recordedFinishes[kind] = nativeAdapters[kind].finish
		withAdapter(t, kind, func(a *nativeAdapter) {
			a.observe = func(s *server) (string, string, error) { return recordingOf(s).observe() }
			a.turnless = func(s *server) bool { return !recordingOf(s).current().conversation }
			a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
				return recordingOf(n).finish(n, ctx)
			}
		})
	}
	previous := forceStop
	forceStop = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		return recordingOf(n).forceStop(n, ctx)
	}
	t.Cleanup(func() { forceStop = previous })
	setQuitWait(t, 600*time.Millisecond)
	setForcedStopWait(t, 200*time.Millisecond)
	setTurnlessSettle(t, 0)
	previousWait, previousSettle, previousStop := quiesceWait, inputSettle, stopWait
	// A kill's reap waits for the process group and the terminal's reader,
	// which on a loaded machine can outlast stop's 3 s; past it the exit is
	// recorded with the wait's error instead of the kill. The record's real
	// kills get realStopLimit, as its real quits do.
	quiesceWait, inputSettle, stopWait = 2*time.Second, time.Hour, realStopLimit
	t.Cleanup(func() { quiesceWait, inputSettle, stopWait = previousWait, previousSettle, previousStop })

	worlds := recordedWorlds()
	records := make([]string, len(worlds))
	counts := make([]int, len(worlds))
	t.Run("worlds", func(t *testing.T) {
		for i, w := range worlds {
			t.Run(w.String(), func(t *testing.T) {
				t.Parallel()
				records[i], counts[i] = recordWorld(t, w)
			})
		}
	})
	if t.Failed() {
		return
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	got := fmt.Sprintf("# The supervisor's lifecycle: %d steps. Written by TestSupervisorLifecycleRecord.\n\n%s",
		total, strings.Join(records, "\n"))
	t.Logf("recorded %d steps", total)
	if *updateLifecycleRecord {
		if err := os.WriteFile(lifecycleRecord, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(lifecycleRecord)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		// The first line counts the steps; a differing step says more.
		for i := range min(len(gotLines), len(wantLines)) {
			if i > 0 && gotLines[i] != wantLines[i] {
				t.Fatalf("the supervisor's lifecycle differs from %s at line %d:\n got %s\nwant %s\nIf the change is "+
					"meant, regenerate it with -update-lifecycle-record and show the difference in its own commit",
					lifecycleRecord, i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("the supervisor's lifecycle differs from %s in length: %d lines, recorded %d; if the change is "+
			"meant, regenerate it with -update-lifecycle-record and show the difference in its own commit",
			lifecycleRecord, len(gotLines), len(wantLines))
	}
}
