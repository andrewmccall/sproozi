package home

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// startedOn starts a session on this machine and, for a remote destination,
// moves it there.
func startedOn(t *testing.T, s *Service, repo, destination string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if destination != testLocal {
		if _, err := s.Move(t.Context(), id, uuid.NewString(), destination); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// A placement that cannot answer, such as an unreachable executor, delays no
// other session's exit check: each sweep asks each placement on its own.
func TestAnUnreachablePlacementDelaysNoOtherExitCheck(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	held := startedOn(t, s, repo, testRemote)
	exited := startedOn(t, s, repo, testLocal)
	remote.blockExited = true
	local.exited = &session.Exit{Code: 2}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.sweepExits(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := s.Status(exited)
		if err == nil && status.Phase == stageFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the exited session waited for the unreachable placement: %+v %v", status, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The held session's check is still running, so a second sweep does
	// not start another.
	if _, running := s.exitChecks.Load(held); !running {
		t.Fatal("the unreachable placement's check ended before its bound")
	}
	s.sweepExits(ctx)
	cancel()
}

// A failing exit check is logged once each time its reason changes and once
// when checks succeed again, not on every sweep.
func TestFailedExitCheckIsLoggedOncePerReason(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	var log bytes.Buffer
	s.log = &log
	id := startedOn(t, s, repo, testLocal)
	sweep := func() {
		t.Helper()
		select {
		case <-s.sweepExits(t.Context()):
		case <-time.After(5 * time.Second):
			t.Fatal("sweep did not finish")
		}
	}
	local.exitedErr = errors.New("executor unreachable")
	sweep()
	sweep()
	local.exitedErr = errors.New("executor refused the token")
	sweep()
	local.exitedErr = nil
	sweep()
	sweep()
	got := log.String()
	if strings.Count(got, "Failed to check session "+id+" for an exited agent: executor unreachable") != 1 ||
		strings.Count(got, "Failed to check session "+id+" for an exited agent: executor refused the token") != 1 ||
		strings.Count(got, "Checked session "+id+" for an exited agent again") != 1 {
		t.Fatalf("exit check log: %q", got)
	}
}
