//nolint:goconst // Operation and project names stay literal, as the outcome table prints them.
package work

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

var updateDriverOutcomes = flag.Bool("update-driver-outcomes", false,
	"rewrite testdata/driver-outcomes.txt from the current driver")

const driverOutcomesFile = "testdata/driver-outcomes.txt"

// driverStepLimit bounds the steps recorded for one starting state: enough
// for the longest run of immediate steps (start, recover, complete, finish).
const driverStepLimit = 5

// errUnreachable is every call of a placement the executor cannot reach.
var errUnreachable = errors.New("placement unreachable")

// abortOutcome is what stepPlacement's Abort reports while a state is built.
type abortOutcome int

const (
	abortSealed abortOutcome = iota
	// abortUnknown leaves the writer's stop unproven.
	abortUnknown
	// abortUnsealed fences the writer without sealing its usage.
	abortUnsealed
	// abortMismatched answers for another visit, so the coordinator records
	// nothing and the cancellation stays as it was retained.
	abortMismatched
)

// stepPlacement wraps the fixture placement with the outcomes the driver
// outcome table needs, and counts every call the coordinator makes.
type stepPlacement struct {
	*fixturePlacement
	mu             sync.Mutex
	unreachable    bool
	refuseActivate bool
	abort          abortOutcome
	calls          map[string]int
}

func (p *stepPlacement) count(call string) (unreachable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[call]++
	return p.unreachable
}

func (p *stepPlacement) Prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	if p.count("prepare") {
		return session.Incarnation{}, errUnreachable
	}
	return p.fixturePlacement.Prepare(ctx, l, data)
}

func (p *stepPlacement) Activate(ctx context.Context, l session.Launch, i session.Incarnation) error {
	p.mu.Lock()
	refuse := p.refuseActivate
	p.mu.Unlock()
	if p.count("activate") || refuse {
		return errUnreachable
	}
	return p.fixturePlacement.Activate(ctx, l, i)
}

func (p *stepPlacement) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	if p.count("stop") {
		return session.Stopped{}, errUnreachable
	}
	return p.fixturePlacement.Stop(ctx, l, i)
}

func (p *stepPlacement) Inspect(ctx context.Context, l session.Launch) (session.Observation, error) {
	if p.count("inspect") {
		return session.Observation{}, errUnreachable
	}
	return p.fixturePlacement.Inspect(ctx, l)
}

func (p *stepPlacement) Abort(ctx context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	if p.count("abort") {
		return session.Observation{}, errUnreachable
	}
	p.mu.Lock()
	outcome := p.abort
	p.mu.Unlock()
	switch outcome {
	case abortUnknown:
		return session.Observation{}, errUnreachable
	case abortMismatched:
		return session.Observation{State: session.PlacementStopped}, nil
	}
	o, err := p.fixturePlacement.Abort(ctx, l, i)
	if outcome == abortUnsealed && err == nil {
		o.Stopped = nil
	}
	return o, err
}

func (p *stepPlacement) counts() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var parts []string
	for _, call := range []string{"prepare", "activate", "stop", "abort", "inspect"} {
		if n := p.calls[call]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", call, n))
		}
	}
	clear(p.calls)
	if len(parts) == 0 {
		return "calls=none"
	}
	return "calls=" + strings.Join(parts, ",")
}

// driverClock is when the driver steps relative to the session's deadline:
// before it, after it, or for a session without one.
type driverClock string

const (
	clockEarly driverClock = "early"
	clockLate  driverClock = "late"
	clockNone  driverClock = "no-deadline"
)

var driverClocks = []driverClock{clockEarly, clockLate, clockNone}

// stepHealth is how the placement answers while the driver steps.
type stepHealth string

const (
	healthReachable   stepHealth = "reachable"
	healthUnreachable stepHealth = "unreachable"
	healthTurnPending stepHealth = "turn-pending"
)

// catalogueTail is what the catalogue holds beside the execution: nothing,
// a transfer, or a cancellation it recorded before the coordinator received
// the execution.
type catalogueTail string

const (
	tailNone      catalogueTail = "-"
	tailTransfer  catalogueTail = "transfer"
	tailCancelled catalogueTail = "catalogue-cancel"
)

// driverEnv is one owner, coordinator and placement, with no driver
// running, so the test takes each step itself.
type driverEnv struct {
	o        *Owner
	p        *stepPlacement
	offset   *atomic.Int64
	id       string
	request  string
	stopOp   string
	cancelOp string
	tailOp   string
	logs     *strings.Builder
	logMu    *sync.Mutex
	journals string
}

type lockedLog struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (l lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

var driverInput struct {
	once sync.Once
	data []byte
	err  error
}

func newDriverEnv(t *testing.T, interactive bool, clock driverClock) *driverEnv {
	t.Helper()
	template.once.Do(func() { template.err = buildTemplate() })
	if template.err != nil {
		t.Fatalf("build fixture template: %v", template.err)
	}
	// Every case captures the same snapshot, once (see create), so the
	// template's repository is read and never changed.
	fixture := &fixturePlacement{data: bytes.Clone(template.data), meta: template.meta}
	c := withProjects(fixtureConfig(t, template.repo, fixture))
	p := &stepPlacement{fixturePlacement: fixture, calls: map[string]int{}}
	offset := new(atomic.Int64)
	journals := filepath.Join(testutil.PrivateDir(t), "sessions")
	coordinator, err := session.Open(journals,
		map[string]session.Placement{executorKubernetes: p}, testLimit, session.WithClock(func() time.Time {
			return time.Now().Add(time.Duration(offset.Load()))
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	logs, logMu := &strings.Builder{}, &sync.Mutex{}
	c.Coordinator, c.Terminal, c.Log = coordinator, p, lockedLog{logMu, logs}
	o := openOwner(t, c)
	// No driver or exit sweep runs: each step is the test's.
	o.stopAll()
	o.wg.Wait()
	q := Submit{RequestID: uuid.NewString(), Project: "sproozi-dev", Harness: c.Harness,
		Executor: executorKubernetes, Task: fixtureTask}
	if interactive {
		q.Mode, q.Task = ModeInteractive, ""
	}
	v, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	r, err := o.read(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	switch clock {
	case clockEarly:
		r.First.Deadline = now.Add(time.Hour)
	case clockLate:
		r.First.Deadline = now.Add(-time.Hour)
		// The state is built while the deadline is still ahead.
		offset.Store(int64(-2 * time.Hour))
	case clockNone:
		r.First.Deadline = time.Time{}
	}
	if err = o.save(r); err != nil {
		t.Fatal(err)
	}
	e := &driverEnv{o: o, p: p, offset: offset, id: v.ID, request: q.RequestID, journals: journals,
		stopOp: uuid.NewString(), cancelOp: uuid.NewString(), tailOp: uuid.NewString(), logs: logs, logMu: logMu}
	if !interactive {
		e.stopOp = derivedOperation(v.ID, actionTaskComplete)
	}
	return e
}

func (e *driverEnv) create(t *testing.T) {
	t.Helper()
	r, err := e.o.read(e.id)
	if err != nil {
		t.Fatal(err)
	}
	driverInput.once.Do(func() { driverInput.data, driverInput.err = e.o.capture(t.Context(), r) })
	if driverInput.err != nil {
		t.Fatal(driverInput.err)
	}
	if _, err = e.o.config.Coordinator.Create(r.First, driverInput.data, ""); err != nil {
		t.Fatal(err)
	}
}

func (e *driverEnv) activate(t *testing.T) {
	t.Helper()
	e.create(t)
	if _, err := e.o.config.Coordinator.Recover(t.Context(), e.id); err != nil {
		t.Fatal(err)
	}
}

func (e *driverEnv) fixture(set func(*fixturePlacement)) {
	e.p.fixturePlacement.mu.Lock()
	set(e.p.fixturePlacement)
	e.p.fixturePlacement.mu.Unlock()
}

func (e *driverEnv) cancelWith(t *testing.T, outcome abortOutcome) {
	t.Helper()
	e.activate(t)
	e.p.mu.Lock()
	e.p.abort = outcome
	e.p.mu.Unlock()
	_, _ = e.o.config.Coordinator.Cancel(t.Context(), e.id, e.cancelOp, nil)
	e.p.mu.Lock()
	e.p.abort = abortSealed
	e.p.mu.Unlock()
}

// abandon leaves the first visit's start ended by the deadline, with the
// execution idle (ADR 0060).
func (e *driverEnv) abandon(t *testing.T, prepared bool) {
	t.Helper()
	e.create(t)
	e.fixture(func(p *fixturePlacement) {
		if prepared {
			p.lostInitial = true
		} else {
			p.refusePrepare = errUnreachable
		}
	})
	_, _ = e.o.config.Coordinator.Recover(t.Context(), e.id)
	e.fixture(func(p *fixturePlacement) { p.refusePrepare = nil })
	e.offset.Store(int64(3 * time.Hour))
	if rec, err := e.o.config.Coordinator.Recover(t.Context(), e.id); !errors.Is(err, session.ErrDeadlinePassed) || rec.Phase != session.PhaseIdle {
		t.Fatalf("abandon: %s %v", rec.Phase, err)
	}
}

// journalState is a state of the coordinator's journal for the session's
// execution, and how a test reaches it through the coordinator.
type journalState struct {
	name string
	// task or interactive restricts a state one mode cannot hold.
	taskOnly     bool
	needDeadline bool
	build        func(*testing.T, *driverEnv)
}

// stateNotReceived names the state before the coordinator receives the
// execution.
const stateNotReceived = "none"

func journalStates() []journalState {
	finalize := func(t *testing.T, e *driverEnv) {
		t.Helper()
		_, _ = e.o.config.Coordinator.Finalize(t.Context(), e.id, e.stopOp)
	}
	return []journalState{
		{name: stateNotReceived, build: func(*testing.T, *driverEnv) {}},
		{name: "preparing", build: func(t *testing.T, e *driverEnv) { e.create(t) }},
		{name: "activating", build: func(t *testing.T, e *driverEnv) {
			e.create(t)
			e.p.mu.Lock()
			e.p.refuseActivate = true
			e.p.mu.Unlock()
			_, _ = e.o.config.Coordinator.Recover(t.Context(), e.id)
			e.p.mu.Lock()
			e.p.refuseActivate = false
			e.p.mu.Unlock()
		}},
		{name: "active", build: func(t *testing.T, e *driverEnv) { e.activate(t) }},
		{name: "stopping", build: func(t *testing.T, e *driverEnv) {
			e.activate(t)
			e.fixture(func(p *fixturePlacement) { p.turnPending = 1 })
			finalize(t, e)
		}},
		{name: "unknown-stop", build: func(t *testing.T, e *driverEnv) {
			e.activate(t)
			e.fixture(func(p *fixturePlacement) { p.lostStop = true })
			finalize(t, e)
		}},
		{name: "unknown-usage", build: func(t *testing.T, e *driverEnv) {
			e.activate(t)
			e.fixture(func(p *fixturePlacement) { p.missingSeal = true })
			finalize(t, e)
			e.fixture(func(p *fixturePlacement) { p.missingSeal = false })
		}},
		{name: "cancel-unknown-stop", build: func(t *testing.T, e *driverEnv) { e.cancelWith(t, abortUnknown) }},
		{name: "cancel-unknown-usage", build: func(t *testing.T, e *driverEnv) { e.cancelWith(t, abortUnsealed) }},
		{name: "cancelling", build: func(t *testing.T, e *driverEnv) { e.cancelWith(t, abortMismatched) }},
		{name: "cancelled", build: func(t *testing.T, e *driverEnv) { e.cancelWith(t, abortSealed) }},
		{name: "idle", build: func(t *testing.T, e *driverEnv) {
			e.activate(t)
			finalize(t, e)
		}},
		{name: "idle-finished", taskOnly: true, build: func(t *testing.T, e *driverEnv) {
			e.activate(t)
			finalize(t, e)
			r, err := e.o.read(e.id)
			if err != nil {
				t.Fatal(err)
			}
			r.Finished = true
			if err = e.o.save(r); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "idle-start-ended", needDeadline: true, build: func(t *testing.T, e *driverEnv) { e.abandon(t, false) }},
		{name: "idle-start-ended-aborted", needDeadline: true, build: func(t *testing.T, e *driverEnv) { e.abandon(t, true) }},
	}
}

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// revoke removes this installation from the session's project.
func (e *driverEnv) revoke() {
	projects := project.Clone(e.o.config.Projects)
	d := projects["sproozi-dev"]
	d.Members = []string{"another-installation"}
	projects["sproozi-dev"] = d
	e.o.config.Projects = projects
}

// opName names an operation the table can tell apart.
func (e *driverEnv) opName(op string) string {
	switch op {
	case "":
		return "-"
	case e.request:
		return "first-visit"
	case e.stopOp:
		return "stop"
	case e.cancelOp, e.tailOp:
		return "setup"
	case derivedOperation(e.id, "deadline-expired"):
		return "deadline-expired"
	case derivedOperation(e.id, "project-revoked"):
		return "project-revoked"
	}
	return "other"
}

// state describes the catalogue record and the journal after a step.
func (e *driverEnv) state() string {
	var b strings.Builder
	r, err := e.o.read(e.id)
	if err != nil {
		return "catalogue unreadable"
	}
	fmt.Fprintf(&b, "catalogue{finished=%t cancel=%s transfer=%s problem=%q}", r.Finished,
		e.opName(r.CancelOperation), e.opName(r.Transfer), r.Problem)
	rec, err := e.o.config.Coordinator.Inspect(e.id)
	switch {
	case os.IsNotExist(err):
		b.WriteString(" journal{absent}")
	case err != nil:
		b.WriteString(" journal{unreadable}")
	default:
		fmt.Fprintf(&b, " journal{%s cancel=%s pending=%s visits=%d}", rec.Phase,
			e.opName(rec.CancelOperation), e.opName(rec.PendingOperation), len(rec.Visits))
	}
	b.WriteString(" " + e.p.counts())
	e.logMu.Lock()
	logged := strings.TrimSpace(e.logs.String())
	e.logs.Reset()
	e.logMu.Unlock()
	if logged != "" {
		fmt.Fprintf(&b, " log=%q", uuidPattern.ReplaceAllString(logged, "ID"))
	}
	return b.String()
}

var nextNames = map[next]string{nextDone: "done", nextAgain: "again", nextWait: "wait", nextPaced: "paced"}

// continues reports a step after which drive takes another without the
// backoff: at once, or after the completion pace.
func continues(n next) bool { return n == nextAgain || n == nextPaced }

// driverCase is one starting combination of the outcome table.
type driverCase struct {
	state       journalState
	interactive bool
	revoked     bool
	clock       driverClock
	tail        catalogueTail
	health      stepHealth
}

func (c driverCase) String() string {
	mode := "task"
	if c.interactive {
		mode = "interactive"
	}
	membership := "member"
	if c.revoked {
		membership = "revoked"
	}
	return strings.Join([]string{c.state.name, mode, membership, string(c.clock), string(c.tail), string(c.health)}, " ")
}

// driverCases is every starting combination: each journal state, mode,
// project membership, deadline, catalogue tail and placement health. A
// catalogue tail ends the step before the placement is called, so it is
// recorded with a reachable placement only. A state one mode or a session
// without a deadline cannot hold is listed as unreachable.
func driverCases() []driverCase {
	cases := make([]driverCase, 0, len(journalStates())*2*2*len(driverClocks)*5)
	for _, state := range journalStates() {
		for _, interactive := range []bool{false, true} {
			for _, revoked := range []bool{false, true} {
				for _, clock := range driverClocks {
					base := driverCase{state: state, interactive: interactive, revoked: revoked, clock: clock}
					for _, health := range []stepHealth{healthReachable, healthUnreachable, healthTurnPending} {
						c := base
						c.tail, c.health = tailNone, health
						cases = append(cases, c)
					}
					for _, tail := range []catalogueTail{tailTransfer, tailCancelled} {
						c := base
						c.tail, c.health = tail, healthReachable
						cases = append(cases, c)
					}
				}
			}
		}
	}
	return cases
}

// runDriverCase builds the case's state, then takes steps as drive does,
// without waiting, until a step asks for anything but another step.
func runDriverCase(t *testing.T, c driverCase) string {
	t.Helper()
	if c.state.taskOnly && c.interactive || c.state.needDeadline && c.clock == clockNone {
		return c.String() + " | unreachable"
	}
	e := newDriverEnv(t, c.interactive, c.clock)
	c.state.build(t, e)
	e.offset.Store(0)
	r, err := e.o.read(e.id)
	if err != nil {
		t.Fatal(err)
	}
	switch c.tail {
	case tailTransfer:
		r.Transfer = e.tailOp
	case tailCancelled:
		r.CancelOperation = e.tailOp
	}
	if err = e.o.save(r); err != nil {
		t.Fatal(err)
	}
	if c.revoked {
		e.revoke()
	}
	switch c.health {
	case healthUnreachable:
		e.p.mu.Lock()
		e.p.unreachable = true
		e.p.mu.Unlock()
	case healthTurnPending:
		e.fixture(func(p *fixturePlacement) { p.turnPending = 1 << 30 })
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s | %s", c, e.state())
	for range driverStepLimit {
		n := e.o.step(t.Context(), e.id)
		fmt.Fprintf(&b, "\n    %s -> %s", nextNames[n], e.state())
		if !continues(n) {
			return b.String()
		}
	}
	b.WriteString("\n    (again after the limit)")
	return b.String()
}

// The driver's decision for every starting combination, and the state each
// step leaves, match the outcomes recorded from the driver before its
// decision became a table. Run with -update-driver-outcomes to record them
// again from the current code.
func TestDriverOutcomesAreUnchanged(t *testing.T) {
	cases := driverCases()
	results := make([]string, len(cases))
	t.Run("cases", func(t *testing.T) {
		for i, c := range cases {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				results[i] = runDriverCase(t, c)
			})
		}
	})
	if t.Failed() {
		return
	}
	got := strings.Join(results, "\n") + "\n"
	if *updateDriverOutcomes {
		if err := os.WriteFile(driverOutcomesFile, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(driverOutcomesFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("outcome %d changed:\n got: %s\nwant: %s", i, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("outcomes changed: %d lines, want %d", len(gotLines), len(wantLines))
	}
}
