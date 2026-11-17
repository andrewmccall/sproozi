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
	if r.Error != "native deadline expired" || r.State != harness.NativePrepared || started.Load() {
		t.Fatalf("activate after the deadline: %+v, started %v", r, started.Load())
	}
}

// A deadline that passes while activate prepares the launch, which can take
// seconds (the gateway forwarder, a locked login store), still starts no
// harness. The reply is the one the terminal gave when it was handed the
// deadline, and at the deadline the supervisor exits as a prepared one does,
// with no exit report.
func TestDeadlineDuringActivationStartsNoHarness(t *testing.T) {
	started := activatable(t, func() { time.Sleep(600 * time.Millisecond) })
	s, reports := activation(time.Now().Add(300 * time.Millisecond))
	r := s.activate()
	if r.Error != launchPastDeadline || r.State != harness.NativePrepared || started.Load() {
		t.Fatalf("activation overtaken by the deadline: %+v, started %v", r, started.Load())
	}
	s.expire()
	<-s.done
	if reports.String() != "" || supervisorExitCode(s) != 1 {
		t.Fatalf("reports %q, exit code %d", reports.String(), supervisorExitCode(s))
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
