package main

import "github.com/andrewmccall/sproozi/internal/harness"

// Codex's supervisor adapter functions that read its native home. Its launch
// is prepareCodexLaunch (launch.go) and its journal observer observeTaskTurn
// (observer.go).

func (s *server) observeCodexTurn() (string, string, error) {
	s.mu.Lock()
	c := s.config
	id := s.threadID
	s.mu.Unlock()
	return observeTaskTurn(codexHome, id, c.InstructionDigest, c.RolloutMaxBytes, c.Task)
}

// codexTurnless reports a first visit with no Codex conversation to keep:
// Codex writes its journal only once a turn starts.
func (s *server) codexTurnless() bool { return !codexJournalExists(codexHome) }

// codexWaiting is what a graceful stop of Codex waits for: its first turn
// until its journal exists, then the current one.
func (s *server) codexWaiting() string {
	return turnWaiting(harness.NativeCodex, codexJournalExists(codexHome))
}
