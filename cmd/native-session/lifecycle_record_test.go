package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
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
// state it can reach. It was written from the supervisor before its lifecycle
// became one declared state, and must not change unless a behaviour change is
// intended.
const lifecycleRecord = "testdata/supervisor-lifecycle.txt"

// recordedHarness stands in for Codex: a quit typed into its terminal ends it
// with code 0, and SIGUSR1 makes it exit by itself with code 3. It prints
// only "ready", once its terminal no longer echoes input.
const recordedHarness = `stty -echo; trap 'exit 3' USR1; printf ready; read -r line </dev/tty & wait $!; exit 0`

// recordedTurn is the error the recorded harness's observer gives while no
// turn has completed.
var recordedTurn = errors.New("no completed native turn")

// world is what stays fixed for a supervisor's whole life: whether its visit
// is a first one (no selected conversation) and whether its execution has a
// deadline.
type world struct{ first, deadline bool }

func (w world) String() string {
	visit, until := "resumed visit", "no deadline"
	if w.first {
		visit = "first visit"
	}
	if w.deadline {
		until = "a deadline"
	}
	return visit + ", " + until
}

// recordDeadline is far enough away that it passes only when an event says
// so.
var recordDeadline = time.Now().Add(time.Hour)

func (w world) config() config {
	c := config{Harness: harness.NativeCodex, Instructions: instructions, InstructionDigest: digest(),
		ClientConfig: testClients, Env: []string{}, RolloutMaxBytes: 8192}
	if !w.first {
		c.ThreadID = selected
	}
	if w.deadline {
		c.Deadline = recordDeadline
	}
	return c
}

// mode is how the recorded harness and its observer behave during one event.
type mode struct {
	// completed: the observer sees a completed turn of selected.
	completed bool
	// conversation: a first visit has a conversation, so it is not turnless.
	conversation bool
	// onObserve runs once, at the first observation.
	onObserve func(r *recording)
	// finish replaces the adapter's quit.
	finish func(r *recording, n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error)
	// unconfirmed: the forced stop's kill is never confirmed.
	unconfirmed bool
}

// recording is one supervisor driven along one path of events.
type recording struct {
	t       *testing.T
	w       world
	s       *server
	n       *nativepty.Supervisor
	watched chan struct{}
	kills   atomic.Int32
	// mode is read by the exit watcher too, when its report looks for the
	// conversation.
	modeMu sync.Mutex
	mode   mode
	sent   []string
	// pending: a stopped quiesce reply has not been read yet, so the
	// handler has not concluded.
	pending     bool
	reports     *lockedBuffer
	diagnostics *lockedBuffer
	reported    int
	diagnosed   int
}

// recordings finds the recording of a supervisor or of its harness, for the
// adapter and forced stop the record installs.
var recordings sync.Map

func recordingOf(key any) *recording {
	r, ok := recordings.Load(key)
	if !ok {
		panic(fmt.Sprintf("no recording for %T", key))
	}
	return r.(*recording)
}

func newRecording(t *testing.T, w world, bootstrap bool) *recording {
	c := w.config()
	if bootstrap {
		c = config{Deadline: c.Deadline}
	}
	r := &recording{t: t, w: w, s: newRecordedServer(c, bootstrap), reports: &lockedBuffer{},
		diagnostics: &lockedBuffer{}}
	r.s.reports, r.s.diagnostics = r.reports, r.diagnostics
	recordings.Store(r.s, r)
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

func (r *recording) note(what string) { r.sent = append(r.sent, what) }

// launch is a successful activate: the harness starts on its terminal and
// the exit watcher runs. Activation itself needs a container, so the reply
// is the one activate gives.
func (r *recording) launch() response {
	n, err := nativepty.Start(nativepty.Launch{Executable: "/bin/sh", Args: []string{"-c", recordedHarness},
		Env: []string{testPathEnvironment, testTerminalEnvironment}, Dir: r.t.TempDir(),
		Size: nativepty.Size{Rows: 24, Columns: 80}, BeforeKill: func() { r.kills.Add(1) }})
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
	r.n, r.watched = n, make(chan struct{})
	recordings.Store(n, r)
	launched(r.s, n)
	go func() {
		r.s.watch(n, func() {})
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
	return "", "", recordedTurn
}

func (r *recording) finish(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
	if finish := r.current().finish; finish != nil {
		return finish(r, n, ctx)
	}
	return r.quit(n, ctx)
}

// quit is the adapter's own quit.
func (r *recording) quit(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
	r.note("quit")
	exit, err := n.QuitAfterCompletedTurn(ctx)
	r.settle()
	return exit, err
}

func (r *recording) forceStop(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
	if r.current().unconfirmed {
		r.note("kill, never confirmed")
		<-ctx.Done()
		return nativepty.Exit{}, ctx.Err()
	}
	r.note("kill")
	exit, err := n.Stop(ctx)
	r.settle()
	return exit, err
}

// exits makes the harness exit by itself, with code 3.
func (r *recording) exits() {
	if err := unix.Kill(r.n.PID(), unix.SIGUSR1); err != nil {
		r.t.Fatal(err)
	}
	select {
	case <-r.n.Done():
	case <-time.After(10 * time.Second):
		r.t.Fatal("the harness did not exit")
	}
	r.settle()
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

// completedTurn is a quiesce that observes a completed turn.
func completedTurn() mode { return mode{completed: true, conversation: true} }

// ignoresQuitMode is a completed turn whose harness ignores its quit.
func ignoresQuitMode() mode {
	m := completedTurn()
	m.finish = func(r *recording, _ *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		r.note("quit, ignored")
		<-ctx.Done()
		return nativepty.Exit{}, ctx.Err()
	}
	return m
}

// during is a completed turn in which something happens while the
// supervisor waits for the harness to quit.
func during(event func(r *recording)) mode {
	m := completedTurn()
	m.finish = func(r *recording, n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		event(r)
		r.settle()
		return r.quit(n, ctx)
	}
	return m
}

// recordedEvent is one event the record applies in every state. A nil
// result means the event cannot happen in that state or world.
type recordedEvent struct {
	name  string
	apply func(r *recording) *string
}

func said(s string) *string { return &s }

var recordedEvents = []recordedEvent{
	{"configure", func(r *recording) *string {
		c := r.w.config()
		return said(reply(r.s.configure(&c)))
	}},
	{"activate", func(r *recording) *string {
		if wireState(r.s) == harness.NativePrepared {
			return said(reply(r.launch()))
		}
		return said(reply(r.s.activate()))
	}},
	{"gate", func(r *recording) *string { return said(reply(r.s.gate())) }},
	{"status", func(r *recording) *string { return said(reply(r.s.status())) }},
	{"input", func(r *recording) *string { return said(r.input()) }},
	{"quiesce", func(r *recording) *string { return said(r.quiesce(mode{})) }},
	{"quiesce, turn completed", func(r *recording) *string { return said(r.quiesce(completedTurn())) }},
	{"quiesce, quit ignored", func(r *recording) *string { return said(r.quiesce(ignoresQuitMode())) }},
	{"quiesce, quit ignored and kill never confirmed", func(r *recording) *string {
		m := ignoresQuitMode()
		m.unconfirmed = true
		return said(r.quiesce(m))
	}},
	{"quiesce, harness exits while waiting for its turn", func(r *recording) *string {
		return said(r.quiesce(mode{conversation: true, onObserve: (*recording).exits}))
	}},
	{"quiesce, deadline passes while waiting for its turn", func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		deadlineIn(r.s, 200*time.Millisecond)
		return said(r.quiesce(mode{conversation: true}))
	}},
	{"quiesce, deadline passes while waiting for the quit", func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		deadlineIn(r.s, 200*time.Millisecond)
		return said(r.quiesce(ignoresQuitMode()))
	}},
	{"quiesce, terminated while quitting", func(r *recording) *string {
		return said(r.quiesce(during(func(r *recording) { terminate(r.s) })))
	}},
	{"quiesce, harness exits while quitting", func(r *recording) *string {
		return said(r.quiesce(during((*recording).exits)))
	}},
	{"quiesce, deadline raised while quitting", func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		return said(r.quiesce(during(func(r *recording) { raiseDeadline(r.s) })))
	}},
	{"harness exits", func(r *recording) *string {
		if !r.alive() {
			return nil
		}
		r.exits()
		return said("")
	}},
	{"terminated", func(r *recording) *string {
		terminate(r.s)
		return said("")
	}},
	{"deadline", func(r *recording) *string {
		if !r.w.deadline {
			return nil
		}
		raiseDeadline(r.s)
		return said("")
	}},
	{"reply read", func(r *recording) *string {
		if !r.pending {
			return nil
		}
		r.pending = false
		r.s.conclude()
		return said("")
	}},
}

// snapshot is what can be seen of the supervisor at rest.
func (r *recording) snapshot() string {
	var b strings.Builder
	fmt.Fprintf(&b, "state %s", wireState(r.s))
	r.s.mu.Lock()
	thread := r.s.threadID
	r.s.mu.Unlock()
	if thread != "" {
		fmt.Fprintf(&b, ", thread %s", thread)
	}
	switch {
	case r.n == nil:
		b.WriteString(", no harness")
	case r.alive():
		b.WriteString(", harness running")
	default:
		b.WriteString(", harness exited")
	}
	fmt.Fprintf(&b, ", input %s", r.inputProbe())
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
// how it ended, and the lines the supervisor wrote.
func (r *recording) effects(kills int32, harnessWasAlive bool) []string {
	var out []string
	if len(r.sent) > 0 {
		out = append(out, "sent "+strings.Join(r.sent, ", "))
	}
	if k := r.kills.Load() - kills; k > 0 {
		out = append(out, fmt.Sprintf("killed %d", k))
	}
	if harnessWasAlive && !r.alive() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		exit, err := r.n.Wait(ctx)
		cancel()
		line := fmt.Sprintf("harness ended %d %s", exit.Code, exit.Reason)
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
	r.sent = nil
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
	names := []string{start}
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

// The supervisor's behaviour, for every event in every state it reaches, is
// the recorded one: each control reply, the state status reports, what
// reached the harness, how it ended, the exit report and log lines, and the
// supervisor's exit code.
func TestSupervisorLifecycleRecord(t *testing.T) {
	if codexJournalExists(nativeHome) {
		t.Skip("a real Codex home is present")
	}
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.observe = func(s *server) (string, string, error) { return recordingOf(s).observe() }
		a.turnless = func(s *server) bool { return !recordingOf(s).current().conversation }
		a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
			return recordingOf(n).finish(n, ctx)
		}
	})
	previous := forceStop
	forceStop = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		return recordingOf(n).forceStop(n, ctx)
	}
	t.Cleanup(func() { forceStop = previous })
	setQuitWait(t, 600*time.Millisecond)
	setForcedStopWait(t, 200*time.Millisecond)
	setTurnlessSettle(t, 0)
	previousWait := quiesceWait
	quiesceWait = 2 * time.Second
	t.Cleanup(func() { quiesceWait = previousWait })

	worlds := []world{{}, {deadline: true}, {first: true}, {first: true, deadline: true}}
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
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("the supervisor's lifecycle differs from %s at line %d:\n got %s\nwant %s",
					lifecycleRecord, i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("the supervisor's lifecycle differs from %s in length: %d lines, recorded %d",
			lifecycleRecord, len(gotLines), len(wantLines))
	}
}
