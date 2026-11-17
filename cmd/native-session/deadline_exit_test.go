package main

import (
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

// readyHarness runs the recorded harness, which exits by itself with code 3
// on SIGUSR1, and waits until it is ready.
func readyHarness(t *testing.T) *nativepty.Supervisor {
	t.Helper()
	n := startNative(t, recordedHarness, time.Time{})
	for !strings.Contains(string(n.Tail(64)), "ready") {
		select {
		case <-n.Done():
			t.Fatal("the harness exited before it was ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return n
}

// slowlyWatched is an active supervisor running n whose exit watcher takes
// slowly to its lock, as one saving a sign-in does. watched is closed when
// the watcher returns.
func slowlyWatched(t *testing.T, n *nativepty.Supervisor) (s *server, reports *lockedBuffer, watched chan struct{}) {
	t.Helper()
	s = newServer(config{Harness: harness.NativeCodex, ThreadID: selected, Deadline: time.Now().Add(time.Hour)},
		false)
	reports = &lockedBuffer{}
	s.reports, s.diagnostics = reports, &lockedBuffer{}
	s.mu.Lock()
	s.native = n
	s.apply(change{event: evActivate})
	s.mu.Unlock()
	watched = make(chan struct{})
	go func() {
		s.watch(n, func() { time.Sleep(300 * time.Millisecond) })
		close(watched)
	}()
	return s, reports, watched
}

// exitsByItself makes n exit with code 3 and waits for it.
func exitsByItself(t *testing.T, n *nativepty.Supervisor) {
	t.Helper()
	if err := unix.Kill(n.PID(), unix.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-n.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the harness did not exit")
	}
}

// onlyReport is the cause of the one exit report s wrote once concluded.
func onlyReport(t *testing.T, s *server, reports *lockedBuffer, watched chan struct{}) harness.NativeExitReport {
	t.Helper()
	for _, c := range []<-chan struct{}{watched, s.done} {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatal("the supervisor did not finish")
		}
	}
	var found []harness.NativeExitReport
	for line := range strings.SplitSeq(reports.String(), "\n") {
		if r, ok := harness.ParseNativeExitReport([]byte(line)); ok {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("reports: %+v", found)
	}
	return found[0]
}

// A harness that ended by itself before the deadline was noticed ended by
// itself (ADR 0038): a quiesce waiting for its turn that finds both the exit
// and a passed deadline reports the exit, and the exit watcher settles it as
// the harness's own.
func TestQuiesceFindingAnOwnExitAndThePassedDeadlineReportsTheExit(t *testing.T) {
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.turnless = func(*server) bool { return false }
		a.observe = func(s *server) (string, string, error) {
			s.mu.Lock()
			n := s.native
			s.mu.Unlock()
			select {
			case <-n.Done():
			default:
				_ = unix.Kill(n.PID(), unix.SIGUSR1)
				<-n.Done()
			}
			return "", "", errRecordedTurn
		}
	})
	for range 5 {
		s, reports, watched := slowlyWatched(t, readyHarness(t))
		deadlineIn(s, -time.Second)
		r := s.quiesce()
		report := onlyReport(t, s, reports, watched)
		if r.Error != "native process stopped before completed turn observation" ||
			report.Cause != harness.NativeExitSelf || report.Code != 3 || supervisorExitCode(s) != 3 {
			t.Fatalf("reply %+v, report %+v, exit code %d", r, report, supervisorExitCode(s))
		}
	}
}

// The deadline raised after the harness ended by itself, while its exit
// watcher has not yet settled the exit, leaves the exit the harness's own.
func TestDeadlineRaisedAfterAnOwnExitLeavesItTheHarnessesOwn(t *testing.T) {
	n := readyHarness(t)
	s, reports, watched := slowlyWatched(t, n)
	exitsByItself(t, n)
	deadlineIn(s, -time.Second)
	s.expire()
	report := onlyReport(t, s, reports, watched)
	if report.Cause != harness.NativeExitSelf || report.Code != 3 || supervisorExitCode(s) != 3 {
		t.Fatalf("report %+v, exit code %d", report, supervisorExitCode(s))
	}
}
