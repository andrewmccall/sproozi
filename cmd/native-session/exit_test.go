package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

// startNative runs script as the harness under a terminal supervisor.
func startNative(t *testing.T, script string, deadline time.Time) *nativepty.Supervisor {
	t.Helper()
	native, err := nativepty.Start(nativepty.Launch{Executable: "/bin/sh", Args: []string{"-c", script},
		Env: []string{testPathEnvironment, "TERM=xterm"}, Dir: t.TempDir(), Deadline: deadline,
		Size: nativepty.Size{Rows: 24, Columns: 80}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = native.Stop(ctx)
	})
	return native
}

// watched is an active supervisor whose exit watcher runs, as activate
// leaves it.
func watched(native *nativepty.Supervisor) *server {
	s := &server{native: native, state: harness.NativeActive, threadID: selected, done: make(chan struct{}),
		config: config{Harness: harness.NativeCodex, ThreadID: selected}, reports: &bytes.Buffer{}}
	go s.watch(native, func() {})
	return s
}

// ended runs drive, waits for the supervisor to finish and returns the exit
// reports it wrote, which it writes before it finishes.
func ended(t *testing.T, s *server, drive func()) []harness.NativeExitReport {
	t.Helper()
	drive()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	var reports []harness.NativeExitReport
	for line := range strings.SplitSeq(s.reports.(*bytes.Buffer).String(), "\n") {
		if r, ok := harness.ParseNativeExitReport([]byte(line)); ok {
			reports = append(reports, r)
		}
	}
	return reports
}

func TestSelfExitIsReportedOnce(t *testing.T) {
	native := startNative(t, "exit 3", time.Now().Add(time.Minute))
	s := watched(native)
	reports := ended(t, s, func() {})
	if len(reports) != 1 || reports[0].Cause != harness.NativeExitSelf || reports[0].Code != 3 {
		t.Fatalf("self exit reports: %+v", reports)
	}
	if s.state != harness.NativeStopped || s.exitCode != 3 {
		t.Fatalf("self exit settled as %s with code %d", s.state, s.exitCode)
	}
}

// A stop the supervisor makes is never reported as the harness's own exit,
// whichever path settles the exit first.
func TestSupervisorStopsAreNotReportedAsSelfExits(t *testing.T) {
	for name, cause := range map[string]exitCause{"deadline": causeDeadline, "terminated": causeTerminated,
		"failed": causeFailed} {
		t.Run(name, func(t *testing.T) {
			s := watched(startIdleNative(t))
			reports := ended(t, s, func() { s.stop(cause) })
			want, _ := cause.reported()
			if len(reports) != 1 || reports[0].Cause != want {
				t.Fatalf("stop reports: %+v, want one %q", reports, want)
			}
			if s.exitCode == 0 {
				t.Fatal("a forced stop exited the supervisor cleanly")
			}
		})
	}
}

// The terminal supervisor's own deadline ends the harness as a deadline.
func TestTerminalDeadlineIsReportedAsDeadline(t *testing.T) {
	native := startNative(t, "exec sleep 30", time.Now().Add(300*time.Millisecond))
	s := watched(native)
	reports := ended(t, s, func() {})
	if len(reports) != 1 || reports[0].Cause != harness.NativeExitDeadline {
		t.Fatalf("deadline reports: %+v", reports)
	}
}

// A harness that dies after input is gated, with no graceful stop in flight,
// still ends the supervisor.
func TestHarnessThatDiesWhileStoppingReachesStopped(t *testing.T) {
	native := startIdleNative(t)
	s := watched(native)
	if r := s.gate(); r.Error != "" || r.State != harness.NativeStopping {
		t.Fatalf("gate: %+v", r)
	}
	reports := ended(t, s, func() {
		if err := unix.Kill(-native.PID(), unix.SIGKILL); err != nil {
			t.Error(err)
		}
	})
	if len(reports) != 1 || reports[0].Cause != harness.NativeExitSelf {
		t.Fatalf("exit while stopping reports: %+v", reports)
	}
	if st := s.status(); st.State != harness.NativeStopped {
		t.Fatalf("state after exit while stopping: %s", st.State)
	}
}

// A graceful stop owns the exit it causes: no exit report, a clean exit.
func TestTurnlessStopWritesNoReport(t *testing.T) {
	setTurnlessSettle(t, 0)
	native := startIdleNative(t)
	deadline := time.Now().Add(time.Minute)
	s := watched(native)
	s.config, s.threadID, s.deadline = config{Harness: harness.NativeCodex, Deadline: deadline}, "", deadline
	var r response
	reports := ended(t, s, func() {
		r = s.quiesce()
		if r.State == harness.NativeStopped {
			s.conclude()
		}
	})
	if r.Error != "" || !r.Turnless || len(reports) != 0 || s.exitCode != 0 {
		t.Fatalf("turnless stop: %+v reports %+v code %d", r, reports, s.exitCode)
	}
}
