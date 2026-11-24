package main

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// A stopped quiesce reply must reach the placement before the supervisor
// exits: on Kubernetes the supervisor is PID 1, so exiting ends the container
// and can cut the exec stream that carries the reply. The supervisor waits
// for the client, which closes only after it has written the reply out.
func TestStoppedQuiesceWaitsForTheClientBeforeConcluding(t *testing.T) {
	if codexJournalExists(codexHome) {
		t.Skip("a real Codex home is present")
	}
	setTurnlessSettle(t, 100*time.Millisecond)
	native := startIdleNative(t)
	deadline := time.Now().Add(time.Minute)
	s := &server{native: native, life: lifecycle{phase: phaseActive}, done: make(chan struct{}),
		config: config{Harness: harness.NativeCodex, Deadline: deadline}, deadline: deadline}
	if r := s.gate(); r.Error != "" {
		t.Fatal(r.Error)
	}
	client, backend := net.Pipe()
	handled := make(chan struct{})
	go func() { s.handle(backend); close(handled) }()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(client).Encode(request{Op: harness.NativeQuiesce}); err != nil {
		t.Fatal(err)
	}
	var r response
	if err := json.NewDecoder(bufio.NewReader(client)).Decode(&r); err != nil || r.State != harness.NativeStopped {
		t.Fatalf("quiesce reply: %+v %v", r, err)
	}
	select {
	case <-s.done:
		t.Fatal("the supervisor concluded while the client still held the reply")
	case <-time.After(300 * time.Millisecond):
	}
	_ = client.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not conclude after the client closed")
	}
	<-handled
}

// A client that never closes cannot hold the supervisor past the grace.
func TestStoppedQuiesceConcludesAfterTheGrace(t *testing.T) {
	if codexJournalExists(codexHome) {
		t.Skip("a real Codex home is present")
	}
	setTurnlessSettle(t, 100*time.Millisecond)
	previous := quiesceReplyGrace
	quiesceReplyGrace = 200 * time.Millisecond
	t.Cleanup(func() { quiesceReplyGrace = previous })
	native := startIdleNative(t)
	deadline := time.Now().Add(time.Minute)
	s := &server{native: native, life: lifecycle{phase: phaseActive}, done: make(chan struct{}),
		config: config{Harness: harness.NativeCodex, Deadline: deadline}, deadline: deadline}
	if r := s.gate(); r.Error != "" {
		t.Fatal(r.Error)
	}
	client, backend := net.Pipe()
	defer func() { _ = client.Close() }()
	go s.handle(backend)
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(client).Encode(request{Op: harness.NativeQuiesce}); err != nil {
		t.Fatal(err)
	}
	var r response
	if err := json.NewDecoder(bufio.NewReader(client)).Decode(&r); err != nil || r.State != harness.NativeStopped {
		t.Fatalf("quiesce reply: %+v %v", r, err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("a client that never closed held the supervisor open")
	}
}
