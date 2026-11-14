package main

import (
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// A supervisor bootstrapped without a deadline configures a launch without
// one, and a configuration cannot add one later (ADR 0056).
func TestBootstrapWithoutDeadline(t *testing.T) {
	s := &server{config: config{}, state: harness.NativeUnconfigured, done: make(chan struct{})}
	approved := config{Harness: harness.NativeCodex, ThreadID: selected, Instructions: instructions,
		InstructionDigest: digest(), ClientConfig: testClients, Env: []string{}, RolloutMaxBytes: 8192}
	if r := s.configure(&approved); r.Error != "" || r.State != harness.NativePrepared {
		t.Fatalf("configure without deadline: %+v", r)
	}
	changed := approved
	changed.Deadline = time.Now().Add(time.Hour)
	if r := s.configure(&changed); r.Error == "" {
		t.Fatal("configuration added a deadline")
	}
}
