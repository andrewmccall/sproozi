package main

import "github.com/andrewmccall/sproozi/internal/harness"

// Codex's supervisor adapter functions that read its native home. Its launch
// is prepareCodexLaunch (launch.go) and its journal observer observeTaskTurn
// (observer.go).

func (s *server) observeCodexTurn() (string, string, error) {
	s.mu.Lock()
	c := s.config
	id := s.threadID
	baseline := s.taskBaseline
	s.mu.Unlock()
	return observeTaskTurn(codexHome, id, c.InstructionDigest, c.RolloutMaxBytes, c.Task, baseline)
}

// codexTurnless reports a first visit with no Codex conversation to keep:
// Codex writes its journal only once a turn starts.
func (s *server) codexTurnless() bool { return !codexJournalExists(codexHome) }

// codexWaiting is what a graceful stop of Codex waits for: its first turn
// until its journal exists, then the current one.
func (s *server) codexWaiting() string {
	return turnWaiting(harness.NativeCodex, codexJournalExists(codexHome))
}

// codexTaskBaseline is the resumed conversation's completed turn before a
// task launch, which observeCodexTurn requires the task's turn to differ
// from. The caller holds s.mu.
func (s *server) codexTaskBaseline() (string, error) {
	_, baseline, err := observeTurn(codexHome, s.threadID, s.config.InstructionDigest, s.config.RolloutMaxBytes)
	return baseline, err
}
