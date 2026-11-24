package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// activatable makes activate launch the recorded harness in a temporary
// workspace, running prepare as the adapter's launch preparation. started
// says whether a terminal was started.
func activatable(t *testing.T, prepare func()) *atomic.Bool {
	t.Helper()
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.executable = recordedShell
		a.prepare = func(_ config, _ string, env []string) ([]string, []string, error) {
			prepare()
			return []string{"-c", recordedHarness}, env, nil
		}
	})
	started := &atomic.Bool{}
	previous := startTerminal
	startTerminal = func(launch nativepty.Launch) (*nativepty.Supervisor, error) {
		started.Store(true)
		launch.Dir = t.TempDir()
		n, err := previous(launch)
		if err == nil {
			t.Cleanup(func() { _, _ = n.Stop(t.Context()) })
		}
		return n, err
	}
	t.Cleanup(func() { startTerminal = previous })
	return started
}

// acrossDeadline activates a fresh supervisor whose real deadline is ahead
// when activate first checks it and has passed, on the machine's clock, when
// preparing ends: prepare, the adapter's preparation, sleeps until then. The
// deadline is never changed once configured, so the launch-time check is the
// one that must refuse. If the first check already refused, preparing never
// ran; the next attempt's deadline is twice as far ahead. The outcome never
// depends on the machine's speed; only the number of attempts does.
type acrossDeadline struct {
	until atomic.Int64 // the current attempt's deadline, in Unix nanoseconds
	ran   atomic.Bool
}

func (a *acrossDeadline) prepare() {
	a.ran.Store(true)
	time.Sleep(time.Until(time.Unix(0, a.until.Load())) + 10*time.Millisecond)
}

func (a *acrossDeadline) activate(t *testing.T) (*server, *lockedBuffer, response) {
	t.Helper()
	ahead := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		until := time.Now().Add(ahead)
		a.until.Store(until.UnixNano())
		a.ran.Store(false)
		s, reports := activation(until)
		r := s.activate()
		if a.ran.Load() {
			return s, reports, r
		}
		if r.Error != deadlineExpired || attempt == 6 {
			t.Fatalf("activation with a deadline %s ahead never prepared: %+v", ahead, r)
		}
		ahead *= 2
	}
}

// activation is a configured supervisor whose deadline is until.
func activation(until time.Time) (*server, *lockedBuffer) {
	s := newServer(config{Harness: harness.NativeCodex, ThreadID: selected, Instructions: instructions,
		InstructionDigest: digest(), ClientConfig: testClients, Env: []string{}, RolloutMaxBytes: 8192,
		Deadline: until}, false)
	reports := &lockedBuffer{}
	s.reports, s.diagnostics = reports, &lockedBuffer{}
	return s, reports
}

// activate refuses after the deadline and starts no harness.
func TestActivateAfterTheDeadlineIsRefused(t *testing.T) {
	started := activatable(t, func() {})
	s, _ := activation(time.Now().Add(-time.Second))
	r := s.activate()
	if r.Error != deadlineExpired || r.State != harness.NativePrepared || started.Load() {
		t.Fatalf("activate after the deadline: %+v, started %v", r, started.Load())
	}
}

// A deadline that passes while activate prepares the launch, which can take
// seconds (the gateway forwarder, a locked login store), still starts no
// harness. The reply is the one the terminal gave when it was handed the
// deadline, and at the deadline the supervisor exits as a prepared one does,
// with no exit report.
func TestDeadlineDuringActivationStartsNoHarness(t *testing.T) {
	across := &acrossDeadline{}
	started := activatable(t, across.prepare)
	s, reports, r := across.activate(t)
	if r.Error != launchPastDeadline || r.State != harness.NativePrepared || started.Load() {
		t.Fatalf("activation overtaken by the deadline: %+v, started %v", r, started.Load())
	}
	s.expire()
	<-s.done
	if reports.String() != "" || supervisorExitCode(s) != 1 {
		t.Fatalf("reports %q, exit code %d", reports.String(), supervisorExitCode(s))
	}
}

// The refused activation stops the sign-in sync it started, so its final
// save runs and nothing syncs for a harness that never ran.
func TestDeadlineDuringActivationStopsTheSignInSync(t *testing.T) {
	across := &acrossDeadline{}
	activatable(t, across.prepare)
	l := recordedLogin(t)
	previous := startLogin
	startLogin = func(config) (*loginSync, error) { return l, nil }
	t.Cleanup(func() { startLogin = previous })
	if _, _, r := across.activate(t); r.Error != launchPastDeadline {
		t.Fatalf("activate: %+v", r)
	}
	select {
	case <-l.quit:
	default:
		t.Fatal("the refused activation left its sign-in sync running")
	}
}

// Without a deadline the same activation launches the harness.
func TestActivationLaunchesTheHarness(t *testing.T) {
	started := activatable(t, func() {})
	s, _ := activation(time.Time{})
	if r := s.activate(); r.Error != "" || r.State != harness.NativeActive || !started.Load() {
		t.Fatalf("activate: %+v, started %v", r, started.Load())
	}
}
