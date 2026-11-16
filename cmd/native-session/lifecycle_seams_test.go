package main

import (
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// The lifecycle record (lifecycle_record_test.go) drives the supervisor
// through these few seams. They are the only part of the record that reads
// or sets the supervisor's internals, so a change to how the supervisor holds
// its lifecycle changes them and nothing else.

// newRecordedServer is the supervisor serve starts, bootstrapped or
// configured.
func newRecordedServer(c config, bootstrap bool) *server {
	s := &server{config: c, deadline: c.Deadline, state: harness.NativePrepared, threadID: c.ThreadID,
		done: make(chan struct{})}
	if bootstrap {
		s.state = harness.NativeUnconfigured
	}
	return s
}

// launched leaves s as a successful activate leaves it with n running; the
// record starts the exit watcher itself.
func launched(s *server, n *nativepty.Supervisor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.native, s.state = n, harness.NativeActive
}

// wireState is the state status reports, read without status's side effects.
func wireState(s *server) harness.NativeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// supervisorExitCode is the code the supervisor exits with once concluded.
func supervisorExitCode(s *server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

// terminate is serve's response to SIGTERM.
func terminate(s *server) { s.stop(causeTerminated) }

// raiseDeadline is serve's response to the execution's deadline passing.
func raiseDeadline(s *server) { s.stop(causeDeadline) }

// deadlineIn moves the execution's deadline to d from now, for a quiesce
// that the deadline interrupts.
func deadlineIn(s *server, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = time.Now().Add(d)
}
