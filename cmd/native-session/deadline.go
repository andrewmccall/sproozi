package main

import (
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
)

// The execution's deadline has one owner in the supervisor: s.deadline, set
// once from the launch contract, or none (ADR 0056), and read only through
// these methods. When it passes, the supervisor raises one event, deadline,
// through expire: from watchDeadline, which serve runs, or from a stop that
// finds it passed first. The harness's terminal is given no deadline of its
// own.

// until is the execution's deadline.
func (s *server) until() deadline.Deadline { return deadline.At(s.deadline) }

// pastDeadline reports, by the wall clock, that the deadline has passed.
// Without one it never has.
func (s *server) pastDeadline() bool { return s.until().Passed(time.Now()) }

// watchDeadline raises the deadline when the wall clock reaches it, unless
// the supervisor concludes first.
func (s *server) watchDeadline() {
	select {
	case <-s.until().Done(s.done):
		s.expire()
	case <-s.done:
	}
}

// expire raises the deadline event once, stopping the harness with the cause
// deadline, and reports whether it did. A harness that already ended by
// itself, before anything forced it, keeps that exit as its own (ADR 0038):
// the deadline is not raised and the exit watcher settles the exit. A caller
// that finds the deadline being raised waits until that stop has finished.
func (s *server) expire() bool {
	s.expired.Do(func() {
		if s.endedByItself() {
			return
		}
		s.stop(evDeadline)
		s.deadlineRaised = true
	})
	return s.deadlineRaised
}

// endedByItself reports a harness that has exited while nothing was forcing
// or finishing it, so its exit is its own.
func (s *server) endedByItself() bool {
	s.mu.Lock()
	n, p := s.native, s.life.phase
	s.mu.Unlock()
	if n == nil || (p != phaseActive && p != phaseGated) {
		return false
	}
	select {
	case <-n.Done():
		return true
	default:
		return false
	}
}

// expireIfPassed raises the deadline if it has passed, and reports whether
// it did.
func (s *server) expireIfPassed() bool { return s.pastDeadline() && s.expire() }
