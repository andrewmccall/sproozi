//nolint:goconst // Test identity values remain literal inputs.
package work

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
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
	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

const fixtureResource = "exact-resource"
const fixtureTask = "Fix the repository"
const fixtureModel = "model.inference"
const quietArg = "--quiet"
const executorKubernetes = "kubernetes"
const repositorySproozi = "sproozi"

const testThread = "01a126e8-288d-7350-9b52-627e16048343"
const testInstructions = "Act only within the approved task"
const testResult = "Verified the repository change"
const testLimit = int64(8 << 20)

// fixtureVisit is what the fixture placement holds for one allocation.
type fixtureVisit struct {
	state       session.PlacementState
	incarnation session.Incarnation
	stopped     *session.Stopped
}

// fixturePlacement is a Kubernetes placement double for the coordinator: it
// keeps each allocation's state, so Inspect reports what Prepare, Activate,
// Stop and Abort did.
type fixturePlacement struct {
	mu                     sync.Mutex
	data                   []byte
	meta                   checkpoint.Metadata
	initial, active, stops int
	aborts                 int
	lostStop               bool
	missingSeal            bool
	missingFence           bool
	lostInitial            bool
	refusePrepare          error
	heldStop               <-chan struct{}
	stopStarted            chan struct{}
	// turnPending is how many stops report that the task's turn is still
	// running before one succeeds.
	turnPending int
	// stopReturning closes just before a stop returns success.
	stopReturning chan struct{}
	launches      []session.Launch
	// inputs is what each Prepare restored, by allocation.
	inputs   map[string][]byte
	profiles map[string][]byte
	visits   map[string]fixtureVisit
	// exited marks allocations whose harness ended on its own; exit is what
	// their stop reports.
	exited map[string]bool
	exit   *session.Exit
	// attached receives each attachment's input frames.
	attached [][]byte
}

func (p *fixturePlacement) Exited(_ context.Context, l session.Launch, i session.Incarnation) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.visits[l.AllocationID]
	return ok && v.state == session.PlacementActive && v.incarnation == i && p.exited[l.AllocationID], nil
}

// Attach copies the attachment's frames to its output, as an echoing
// terminal would, and keeps them.
func (p *fixturePlacement) Attach(_ context.Context, l session.Launch, i session.Incarnation, input io.Reader, output io.Writer) error {
	p.mu.Lock()
	v, ok := p.visits[l.AllocationID]
	p.mu.Unlock()
	if !ok || v.state != session.PlacementActive || v.incarnation != i {
		return errors.New("attachment needs the exact active visit")
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.attached = append(p.attached, data)
	p.mu.Unlock()
	_, err = output.Write(data)
	return err
}

func (p *fixturePlacement) StoreProfile(_ context.Context, digest string, archive []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.profiles == nil {
		p.profiles = map[string][]byte{}
	}
	p.profiles[digest] = archive
	return nil
}

func (p *fixturePlacement) set(l session.Launch, v fixtureVisit) {
	if p.visits == nil {
		p.visits = map[string]fixtureVisit{}
	}
	p.visits[l.AllocationID] = v
}

func (p *fixturePlacement) receipt(l session.Launch, i session.Incarnation) *session.Receipt {
	usage := map[string]budget.TrustedUsage{}
	for name := range l.Allowances {
		usage[name] = budget.TrustedUsage{}
	}
	usage[fixtureModel] = budget.TrustedUsage{Units: 7}
	return &session.Receipt{Launch: l, Incarnation: i, Usage: usage}
}

func (p *fixturePlacement) Prepare(_ context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initial++
	p.launches = append(p.launches, l)
	resumed := l.ThreadID != "" && l.CheckpointDigest == p.meta.SHA256 && bytes.Equal(data, p.data)
	fresh := l.ThreadID == "" && l.CheckpointDigest == "" && len(data) > 0
	if !resumed && !fresh {
		return session.Incarnation{}, errors.New("invalid initial intent")
	}
	if p.inputs == nil {
		p.inputs = map[string][]byte{}
	}
	p.inputs[l.AllocationID] = bytes.Clone(data)
	if p.refusePrepare != nil {
		return session.Incarnation{}, p.refusePrepare
	}
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: fixtureResource}
	if p.lostInitial {
		p.lostInitial = false
		p.set(l, fixtureVisit{state: session.PlacementPreparing, incarnation: i})
		return session.Incarnation{}, errors.New("lost preparation reply")
	}
	p.set(l, fixtureVisit{state: session.PlacementPrepared, incarnation: i})
	return i, nil
}
func (p *fixturePlacement) Activate(_ context.Context, l session.Launch, i session.Incarnation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active++
	p.set(l, fixtureVisit{state: session.PlacementActive, incarnation: i})
	return nil
}
func (p *fixturePlacement) Inspect(_ context.Context, l session.Launch) (session.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.visits[l.AllocationID]
	if !ok {
		return session.Observation{Launch: l, State: session.PlacementAbsent}, nil
	}
	return session.Observation{Launch: l, State: v.state, Incarnation: v.incarnation, Stopped: v.stopped}, nil
}
func (p *fixturePlacement) Abort(_ context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.aborts++
	if i.ResourceUID != fixtureResource {
		return session.Observation{}, errors.New("exact incarnation required")
	}
	stopped := &session.Stopped{Launch: l, Incarnation: i, ActualStopped: true, Receipt: p.receipt(l, i)}
	p.set(l, fixtureVisit{state: session.PlacementStopped, incarnation: i, stopped: stopped})
	return session.Observation{Launch: l, Incarnation: i, State: session.PlacementStopped, Stopped: stopped}, nil
}
func (p *fixturePlacement) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
	if p.heldStop != nil {
		close(p.stopStarted)
		select {
		case <-p.heldStop:
		case <-ctx.Done():
			return session.Stopped{}, ctx.Err()
		}
	}
	if p.turnPending > 0 {
		p.turnPending--
		return session.Stopped{}, &harness.NativeTurnPending{Waiting: "the task's turn to complete"}
	}
	s := session.Stopped{Launch: l, Incarnation: i, ActualStopped: !p.missingFence, ThreadID: testThread, Checkpoint: p.data, CheckpointDigest: p.meta.SHA256}
	if p.exited[l.AllocationID] && p.exit != nil {
		exit := *p.exit
		s.Exit = &exit
	}
	if !p.missingSeal {
		s.Receipt = p.receipt(l, i)
	}
	if p.lostStop {
		p.lostStop = false
		return session.Stopped{}, errors.New("lost stopped reply")
	}
	if p.missingFence {
		return s, nil
	}
	p.set(l, fixtureVisit{state: session.PlacementStopped, incarnation: i, stopped: &s})
	if p.missingSeal {
		return s, session.ErrUsageUnsealed
	}
	if p.stopReturning != nil {
		close(p.stopReturning)
		p.stopReturning = nil
	}
	return s, nil
}

// template is the Git snapshot and stopped checkpoint every fixture starts
// from. It is built once per test binary; each test gets a private copy of
// the repository, so tests cannot change one another's snapshot.
var template struct {
	once sync.Once
	root string
	repo string
	data []byte
	meta checkpoint.Metadata
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if template.root != "" {
		_ = os.RemoveAll(template.root)
	}
	os.Exit(code)
}

func buildTemplate() error {
	root, err := os.MkdirTemp("", "sproozi-work-fixture-")
	if err != nil {
		return err
	}
	template.root = root
	repo, home := filepath.Join(root, "repository"), filepath.Join(root, "home")
	for _, dir := range []string{repo, home} {
		if err = testutil.Mkdir(dir, 0700); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"init", quietArg, "--template=", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", quietArg, "--allow-empty", "-m", "Fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, e := cmd.CombinedOutput(); e != nil {
			return fmt.Errorf("git fixture: %w %s", e, out)
		}
	}
	rollout := `{"type":"session_meta","payload":{"id":"` + testThread + `","cli_version":"0.161.0","cwd":"/workspace/session/workspace"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + testInstructions + `"}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"task-turn"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"task-turn","last_agent_message":"` + testResult + `"}}` + "\n"
	state := harness.CodexState{ThreadID: testThread, Path: "sessions/2026/10/12/rollout-2026-10-12T12-00-00-" + testThread + ".jsonl", Rollout: []byte(rollout)}
	if err = harness.RestoreCodexState(home, state, testLimit); err != nil {
		return err
	}
	template.data, template.meta, err = checkpoint.CaptureStopped(context.Background(), harness.NativeCodex, home, repo, testThread, nil, testLimit)
	template.repo = repo
	return err
}

func fixture(t *testing.T) (Config, *fixturePlacement) {
	t.Helper()
	template.once.Do(func() { template.err = buildTemplate() })
	if template.err != nil {
		t.Fatalf("build fixture template: %v", template.err)
	}
	repo := filepath.Join(testutil.PrivateDir(t), "repository")
	if err := os.CopyFS(repo, os.DirFS(template.repo)); err != nil {
		t.Fatal(err)
	}
	p := &fixturePlacement{data: bytes.Clone(template.data), meta: template.meta}
	return fixtureConfig(t, repo, p), p
}

// fixtureConfig is the owner configuration for a snapshot repository, with a
// coordinator whose only placement is p. It runs no Git, so tests of
// configuration validation need no repository. The driver's automatic
// recovery waits an hour, so a test sees an unsettled phase until it
// recovers; tests of automatic recovery shorten it.
func fixtureConfig(t *testing.T, repo string, p *fixturePlacement) Config {
	t.Helper()
	root := testutil.PrivateDir(t)
	coordinator, err := session.Open(filepath.Join(root, "sessions"), map[string]session.Placement{executorKubernetes: p}, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	return Config{Directory: filepath.Join(root, "work"), Principal: "hermes", Harness: harness.NativeCodex,
		Placement: executorKubernetes, InstructionDigest: hash([]byte(testInstructions)),
		Allowances:  map[string]session.Allowance{fixtureModel: {Units: session.Limit{Bounded: true, Value: 100}}},
		MaxDuration: time.Minute, WorkTokenMaxDuration: time.Minute, MaxCheckpointBytes: testLimit,
		Repositories: map[string]Repository{repositorySproozi: {Snapshot: repo}},
		Coordinator:  coordinator, Profiles: p, Terminal: p, Log: fixtureLog{t}, retry: backoff{first: time.Hour}}
}

type fixtureLog struct{ t *testing.T }

func (l fixtureLog) Write(p []byte) (int, error) {
	l.t.Helper()
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func submit(t *testing.T, o *Owner) View {
	t.Helper()
	v, e := o.Submit(t.Context(), Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex, Executor: executorKubernetes, Task: fixtureTask})
	if e != nil {
		t.Fatal(e)
	}
	return v
}

// untilDeadline bounds a wait for owner progress only by the test binary's own
// deadline. Each step runs real Git capture, which has no dependable duration
// on a loaded machine, so a fixed bound turns slowness into failure.
func untilDeadline(t *testing.T) (context.Context, context.CancelFunc) {
	if deadline, ok := t.Deadline(); ok {
		return context.WithDeadline(t.Context(), deadline.Add(-5*time.Second))
	}
	return context.WithCancel(t.Context())
}

// waitPhase waits for the session to reach want. Reaching another settled
// phase, from which it cannot move on its own, fails at once.
func waitPhase(t *testing.T, o *Owner, id string, want Phase) View {
	t.Helper()
	ctx, cancel := untilDeadline(t)
	defer cancel()
	for {
		v, e := o.Info(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if v.Phase == want {
			return v
		}
		if v.Phase.Settled() {
			t.Fatalf("phase=%s want=%s problem=%s", v.Phase, want, v.Problem)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("phase=%s want=%s problem=%s", v.Phase, want, v.Problem)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func assertRemoteSubmissionReplay(t *testing.T, o *Owner, q Submit, v View) {
	t.Helper()
	if v.ExecutionID != v.ID || !CanonicalID(v.AllocationID) || v.AllocationID == v.ExecutionID {
		t.Fatalf("session, bounded execution and allocation identities differ: %+v", v)
	}
	again, e := o.Submit(t.Context(), q)
	if e != nil || again.ID != v.ID || again.ExecutionID != v.ExecutionID || again.AllocationID != v.AllocationID || again.Placement != executorKubernetes {
		t.Fatalf("submission replay: %+v %v", again, e)
	}
	q.Task = "A different task"
	if _, e = o.Submit(t.Context(), q); e == nil {
		t.Fatal("conflicting task accepted")
	}
}

func TestRemoteCatalogueRetainsCompletionAndExclusiveClaimAcrossRestart(t *testing.T) {
	c, p := fixture(t)
	o, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = o.Close() })
	q := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex, Executor: executorKubernetes, Task: fixtureTask}
	v, e := o.Submit(t.Context(), q)
	if e != nil {
		t.Fatal(e)
	}
	assertRemoteSubmissionReplay(t, o, q, v)
	done := waitPhase(t, o, v.ID, phaseRetained)
	if done.ID != v.ID || done.ExecutionID != v.ExecutionID || done.AllocationID != v.AllocationID || done.Incarnation == nil || done.ThreadID != testThread || done.Result != testResult || done.CheckpointDigest != p.meta.SHA256 {
		t.Fatalf("missing retained result: %+v", done)
	}
	op := uuid.NewString()
	export, e := o.BeginExport(t.Context(), v.ID, op)
	if e != nil {
		t.Fatal(e)
	}
	if export.Claimed || export.Stopped.Receipt.Usage[fixtureModel].Units != 7 {
		t.Fatal("download altered ownership or receipt")
	}
	if listed, e := o.Info(t.Context(), v.ID); e != nil || listed.Phase != PhaseExporting {
		t.Fatalf("export in progress: %+v %v", listed, e)
	}
	if _, e = o.Claim(t.Context(), v.ID, op, strings.Repeat("0", 64)); e == nil {
		t.Fatal("wrong digest claimed ownership")
	}
	claim, e := o.Claim(t.Context(), v.ID, op, p.meta.SHA256)
	if e != nil || !claim.Claimed {
		t.Fatalf("claim: %+v %v", claim, e)
	}
	if _, e = o.BeginExport(t.Context(), v.ID, uuid.NewString()); e == nil {
		t.Fatal("second transfer accepted")
	}
	if e = o.Close(); e != nil {
		t.Fatal(e)
	}
	resumed, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = resumed.Close() }()
	replay, e := resumed.Claim(t.Context(), v.ID, op, p.meta.SHA256)
	if e != nil || !replay.Claimed || replay.Stopped.Receipt.Usage[fixtureModel].Units != 7 {
		t.Fatal("claim lost after restart")
	}
	list, e := resumed.List(t.Context(), SessionQuery{})
	if e != nil || len(list) != 1 || list[0].Phase != PhasePulled {
		t.Fatalf("catalogue: %+v %v", list, e)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initial != 1 || p.active != 1 {
		t.Fatalf("task relaunched: initial=%d active=%d", p.initial, p.active)
	}
}

// An unproven stop and unsealed usage are separate phases, and neither can
// export.
func TestUnknownStopOrAccountingCannotExport(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]Phase{"fence": phaseUnknownStop, "accounting": phaseUnknownUsage} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, p := fixture(t)
			p.missingFence = name == "fence"
			p.missingSeal = name == "accounting"
			o, e := Open(c)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = o.Close() }()
			v := submit(t, o)
			got := waitPhase(t, o, v.ID, want)
			if got.Problem == "" {
				t.Fatalf("%s without a problem", got.Phase)
			}
			if _, e = o.BeginExport(t.Context(), v.ID, uuid.NewString()); e == nil {
				t.Fatal("unknown source exported")
			}
		})
	}
}

func TestLostStopReplyRecoversWithoutReactivation(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	p.lostStop = true
	o, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = o.Close() }()
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseUnknownStop)
	// The driver waits an hour before it retries; recover retries now.
	if _, e = o.Recover(t.Context(), v.ID); e != nil {
		t.Fatal(e)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initial != 1 || p.active != 1 || p.stops != 2 {
		t.Fatalf("recovery replayed launch: initial=%d active=%d stops=%d", p.initial, p.active, p.stops)
	}
}

// The driver retries an unproven stop by itself, with a backoff, while the
// executor runs.
func TestDriverRecoversAnUnprovenStopByItself(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.retry = backoff{first: 5 * time.Millisecond, limit: 20 * time.Millisecond}
	p.lostStop = true
	o, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = o.Close() }()
	v := submit(t, o)
	if done := waitPhase(t, o, v.ID, phaseRetained); done.Result != testResult || done.Problem != "" {
		t.Fatalf("recovered task: %+v", done)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != 1 || p.stops != 2 {
		t.Fatalf("activations=%d stops=%d", p.active, p.stops)
	}
}

func TestTaskCredentialCannotClaimHomeOwnership(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = o.Close() }()
	taskToken, home := strings.Repeat("t", 32), strings.Repeat("h", 32)
	h, e := NewHandler(o, taskToken, homeToken(home))
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		token, path string
		want        int
	}{{"", "/work/run", 401}, {taskToken, "/work/claim", 403}, {home, "/work/list", 200}} {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s got %d want %d", tc.path, w.Code, tc.want)
		}
	}
}

func TestSubmissionReplayDoesNotWaitForHeldNativeWork(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	release := make(chan struct{})
	p.heldStop = release
	p.stopStarted = make(chan struct{})
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	defer close(release)
	q := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Task: fixtureTask}
	initial, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	// Native stop stays held until the test returns, so replay and export can
	// only answer while it is in progress; waiting for it would never return.
	// stillHeld confirms each answer arrived while the driver was in Stop.
	stillHeld := func(what string) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.running[initial.ID] == nil {
			t.Fatalf("%s answered after native work finished", what)
		}
	}
	wait, cancel := untilDeadline(t)
	defer cancel()
	select {
	case <-p.stopStarted:
	case <-wait.Done():
		t.Fatal("task did not reach held work")
	}
	done := make(chan View, 1)
	go func() { v, _ := o.Submit(t.Context(), q); done <- v }()
	select {
	case v := <-done:
		if v.ID != initial.ID || v.ExecutionID != initial.ExecutionID || v.AllocationID != initial.AllocationID ||
			v.Phase != phaseStopping {
			t.Fatalf("replay changed identity or phase: %+v", v)
		}
		stillHeld("submission replay")
	case <-wait.Done():
		t.Fatal("submission replay waited for native completion")
	}
	exportDone := make(chan error, 1)
	go func() { _, e := o.BeginExport(t.Context(), initial.ID, uuid.NewString()); exportDone <- e }()
	select {
	case e := <-exportDone:
		if e == nil {
			t.Fatal("export accepted an active writer")
		}
		stillHeld("export")
	case <-wait.Done():
		t.Fatal("export waited for native completion instead of rejecting")
	}
}

func TestCatalogueRejectsReusedDirectoryForAnotherPrincipal(t *testing.T) {
	c, _ := fixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	c.Principal = "another-home"
	other, err := Open(c)
	if err == nil {
		_ = other.Close()
		t.Fatal("accepted catalogue belonging to another principal")
	}
}
func TestCancellationObservesIncarnationAfterLostPreparationReply(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	p.lostInitial = true
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v := submit(t, o)
	if stalled := waitDriver(t, o, v.ID); stalled.Phase != phasePreparing || !strings.Contains(stalled.Problem, "recover") {
		t.Fatalf("lost preparation reply: %+v", stalled)
	}
	operation := uuid.NewString()
	cancelled, err := o.Cancel(t.Context(), v.ID, operation)
	if err != nil || cancelled.Phase != phaseCancelled {
		t.Fatalf("failed to cancel exact observed incarnation: %+v %v", cancelled, err)
	}
	if retried, err := o.Cancel(t.Context(), v.ID, operation); err != nil || retried.Phase != phaseCancelled || p.aborts != 1 {
		t.Fatalf("retried cancellation aborted again: %+v %v aborts=%d", retried, err, p.aborts)
	}
	if _, err := o.Cancel(t.Context(), v.ID, uuid.NewString()); OutcomeCode(err) != profile.CodeConflict {
		t.Fatalf("another cancellation operation accepted: %v", err)
	}
	rec, err := c.Coordinator.Inspect(v.ID)
	if err != nil || len(rec.Visits) != 1 || rec.Visits[0].Incarnation.ResourceUID != fixtureResource || rec.Visits[0].Receipt == nil {
		t.Fatalf("lost exact observed binding: %+v %v", rec.Visits, err)
	}
	if p.active != 0 {
		t.Fatal("cancellation activated unfinished preparation")
	}
}

// A submitted profile reaches the placement once and the launch names it; the
// catalogue retains only the digest.
func TestSubmissionStoresItsProfileAndLaunchesWithItsDigest(t *testing.T) {
	c, p := fixture(t)
	o, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = o.Close() })
	archive, digest, e := agentprofile.Profile{Instructions: "Profile instructions"}.Archive()
	if e != nil {
		t.Fatal(e)
	}
	q := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Task: fixtureTask, Profile: []byte("not an archive")}
	if _, e = o.Submit(t.Context(), q); e == nil {
		t.Fatal("invalid profile archive accepted")
	}
	q.RequestID, q.Profile, q.ProfileDigest = uuid.NewString(), nil, digest
	if _, e = o.Submit(t.Context(), q); e == nil {
		t.Fatal("caller named a profile digest without its archive")
	}
	q.RequestID, q.Profile, q.ProfileDigest = uuid.NewString(), archive, ""
	v, e := o.Submit(t.Context(), q)
	if e != nil {
		t.Fatal(e)
	}
	if again, e := o.Submit(t.Context(), q); e != nil || again.ID != v.ID {
		t.Fatalf("profile submission replay: %+v %v", again, e)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	p.mu.Lock()
	defer p.mu.Unlock()
	if string(p.profiles[digest]) != string(archive) || len(p.launches) == 0 || p.launches[0].ProfileDigest != digest {
		t.Fatalf("profile not stored or named: %d stored, launches %+v", len(p.profiles), p.launches)
	}
	entries, e := os.ReadDir(c.Directory)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(c.Directory, entry.Name()))
		if bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(archive)[:32])) {
			t.Fatalf("catalogue record %s retained the archive", entry.Name())
		}
	}
}

// A submission is refused once retained bytes, plus a checkpoint for each
// unsettled session and the new one, would exceed the bound.
func TestSubmissionRespectsRetainedBytes(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.RetainedBytes = 2 * c.MaxCheckpointBytes
	release := make(chan struct{})
	p.heldStop, p.stopStarted = release, make(chan struct{})
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	defer close(release)
	submit(t, o)
	if _, err = o.Submit(t.Context(), Submit{RequestID: uuid.NewString(), Repository: repositorySproozi,
		Harness: harness.NativeCodex, Executor: executorKubernetes, Task: fixtureTask}); OutcomeCode(err) != profile.CodeUnavailable {
		t.Fatalf("second submission beyond retainedBytes: %v", err)
	}
	c.RetainedBytes = c.MaxCheckpointBytes
	if err = c.validate(); err == nil {
		t.Fatal("retainedBytes below twice the checkpoint limit accepted")
	}
}
