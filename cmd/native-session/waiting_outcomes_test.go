package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// waitingOutcomes records, for every managed kind and an unregistered one,
// what a graceful stop reports it waits for (waitingFor) in each recorded
// situation, and what activate replies to a task launch that resumes a
// conversation. A change that only moves where this behaviour is kept leaves
// it byte-identical; regenerate it with SPROOZI_UPDATE_OUTCOMES=1 only for a
// change that means to alter an outcome, and show the difference in that
// change.
const waitingOutcomes = "testdata/waiting-outcomes.json"

// unregisteredKind is a kind the registry does not hold.
const unregisteredKind harness.NativeKind = "not-registered"

// The recorded conversations: a Claude Code transcript the pinned client
// wrote, and a Copilot journal, from internal/harness's fixtures.
const (
	recordedClaudeSession  = "ef2d33f3-d05d-4ee1-857d-97eca64894f5"
	recordedClaudeFixture  = "../../internal/harness/testdata/claude-2.1.318-sproozi-skill.jsonl"
	recordedCopilotSession = "0a7c5e1e-3b2d-4c4f-9a8b-1f2e3d4c5b6a"
	recordedCopilotFixture = "../../internal/harness/testdata/copilot/turn.jsonl"
	recordedTask           = "Fix the fixture"
)

// outcomeKinds are the managed kinds, in registry order, then an
// unregistered one.
func outcomeKinds() []harness.NativeKind {
	return append(harness.ManagedKinds(), unregisteredKind)
}

// recordedHomes gives each harness an empty native home for the test, and
// restores the supervisor's own homes afterwards.
func recordedHomes(t *testing.T) (codex, claude, copilot string) {
	t.Helper()
	dir := t.TempDir()
	previousCodex, previousClaude, previousCopilot := codexHome, claudeHome, copilotHome
	codexHome, claudeHome, copilotHome = filepath.Join(dir, "codex"), filepath.Join(dir, "claude"),
		filepath.Join(dir, "copilot")
	for _, home := range []string{codexHome, claudeHome, copilotHome} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { codexHome, claudeHome, copilotHome = previousCodex, previousClaude, previousCopilot })
	return codexHome, claudeHome, copilotHome
}

// copyFixture writes the fixture at from to path.
func copyFixture(t *testing.T, from, path string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitingSituation is one recorded situation of a graceful stop with no
// conversation named yet.
type waitingSituation struct {
	name string
	// thread is the launch's selected conversation; launchID the one a
	// supervisor named itself.
	thread, launchID string
	task             string
	// journals: each harness's home holds a conversation: a Codex rollout,
	// the Copilot journal under copilotSession and the Claude Code
	// transcript.
	journals       bool
	copilotSession string
	// unsettled: terminal input was accepted just now and after any prompt.
	unsettled bool
	claude    claudeLifecycle
}

func waitingSituations() []waitingSituation {
	running := claudeLifecycle{id: recordedClaudeSession, turn: turnRunning, pending: map[string]string{}}
	pending := claudeLifecycle{id: recordedClaudeSession, turn: turnRunning,
		pending: map[string]string{"tool-one": claudeBash}}
	completed := claudeLifecycle{id: recordedClaudeSession, turn: turnCompleted, everCompleted: true,
		pending: map[string]string{}}
	failed := claudeLifecycle{id: recordedClaudeSession, turn: turnFailed, pending: map[string]string{}}
	return []waitingSituation{
		{name: "empty homes, new conversation"},
		{name: "empty homes, conversation named at launch", launchID: recordedCopilotSession},
		{name: "journals, conversation named at launch", launchID: recordedCopilotSession, journals: true,
			copilotSession: recordedCopilotSession},
		{name: "journals, conversation named at launch, unsettled input", launchID: recordedCopilotSession,
			journals: true, copilotSession: recordedCopilotSession, unsettled: true},
		{name: "journals of another conversation, conversation named at launch", launchID: recordedCopilotSession,
			journals: true, copilotSession: recordedClaudeSession},
		{name: "empty homes, restored conversation", thread: recordedClaudeSession},
		{name: "journals, restored conversation", thread: recordedClaudeSession, journals: true,
			copilotSession: recordedClaudeSession},
		{name: "journals, running turn", journals: true, claude: running},
		{name: "journals, running tool", journals: true, claude: pending},
		{name: "journals, failed turn", journals: true, claude: failed},
		{name: "journals, completed turn", journals: true, claude: completed},
		{name: "empty homes, completed turn", claude: completed},
		{name: "journals, completed turn, unsettled input", journals: true, claude: completed, unsettled: true},
		{name: "journals, completed turn of another prompt than the task", journals: true, claude: completed,
			task: recordedTask},
	}
}

// waitingHomes gives each harness a native home for situation w, holding
// w's journals.
func waitingHomes(t *testing.T, w waitingSituation) {
	t.Helper()
	codex, claude, copilot := recordedHomes(t)
	if !w.journals {
		return
	}
	copyFixture(t, recordedCopilotFixture, filepath.Join(copilot, "session-state", w.copilotSession, "events.jsonl"))
	path, err := harness.ClaudeProjectPath(workspace, recordedClaudeSession)
	if err != nil {
		t.Fatal(err)
	}
	copyFixture(t, recordedClaudeFixture, filepath.Join(claude, path))
	writeCodexRollout(t, codex)
}

// writeCodexRollout writes a rollout of selected with one completed turn to
// a Codex home.
func writeCodexRollout(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "11")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-11T12-00-00-"+selected+".jsonl"),
		[]byte(journal("")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitingServer is a gated supervisor of kind in situation w.
func waitingServer(kind harness.NativeKind, w waitingSituation) *server {
	s := &server{config: config{Harness: kind, ThreadID: w.thread, Task: w.task, Instructions: instructions,
		InstructionDigest: digest(), RolloutMaxBytes: 1 << 20, Env: []string{}},
		life: lifecycle{phase: phaseGated, gatedAt: time.Now().Add(-time.Hour)}, threadID: w.thread,
		launchID: w.launchID, claude: w.claude, lastInputAt: time.Now().Add(-time.Hour)}
	if w.unsettled {
		s.inputGeneration, s.lastInputAt = w.claude.input+1, time.Now()
	}
	return s
}

// recordedActivation is activate's reply to a task launch that resumes
// selected, with kind's launch preparation, sign-in and terminal replaced so
// that nothing outside the test's homes is written and no harness starts.
func recordedActivation(t *testing.T, kind harness.NativeKind, journals bool) map[string]any {
	t.Helper()
	codex, _, _ := recordedHomes(t)
	if journals {
		writeCodexRollout(t, codex)
	}
	for _, managed := range harness.ManagedKinds() {
		withAdapter(t, managed, func(a *nativeAdapter) {
			a.executable = recordedShell
			a.prepare = func(_ config, _ string, env []string) ([]string, []string, error) {
				return []string{"-c", recordedHarness}, env, nil
			}
		})
	}
	previousLogin, previousTerminal := startLogin, startTerminal
	// No sign-in sync: a launch without one is a harness signed in by the
	// gateway.
	startLogin = func(config) (*loginSync, error) { return nil, nil }
	startTerminal = func(nativepty.Launch) (*nativepty.Supervisor, error) {
		return nil, errors.New("the recorded terminal does not start")
	}
	t.Cleanup(func() { startLogin, startTerminal = previousLogin, previousTerminal })
	c := config{Harness: kind, ThreadID: selected, Task: recordedTask, Instructions: instructions,
		InstructionDigest: digest(), ClientConfig: testClients, Env: []string{}, RolloutMaxBytes: 8192}
	s := newServer(c, false)
	s.reports, s.diagnostics = &lockedBuffer{}, &lockedBuffer{}
	r := s.activate()
	return map[string]any{
		"validate": strings.ReplaceAll(errText(c.Validate(time.Now())), codex, "<codex home>"),
		"error":    strings.ReplaceAll(r.Error, codex, "<codex home>"),
		"state":    string(r.State),
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func recordedWaitingOutcomes(t *testing.T) map[string]any {
	waiting := make([]map[string]any, 0, len(waitingSituations()))
	for _, w := range waitingSituations() {
		waitingHomes(t, w)
		texts := map[string]string{}
		for _, kind := range outcomeKinds() {
			texts[string(kind)] = waitingServer(kind, w).waitingFor()
		}
		waiting = append(waiting, map[string]any{"situation": w.name,
			"claudeObservation": waitingServer(harness.NativeClaude, w).claudeCompletion().Observation,
			"waiting":           texts})
	}
	activations := make([]map[string]any, 0, 2*len(outcomeKinds()))
	for _, kind := range outcomeKinds() {
		for _, journals := range []bool{false, true} {
			home := "empty Codex home"
			if journals {
				home = "Codex home with the resumed conversation"
			}
			outcome := recordedActivation(t, kind, journals)
			outcome["kind"], outcome["home"] = string(kind), home
			activations = append(activations, outcome)
		}
	}
	return map[string]any{"waitingFor": waiting, "taskActivation": activations}
}

// A kind the registry does not hold never reaches a graceful stop, so it
// needs no wait text: the launch contract's validation refuses it, configure
// and loading the contract run that validation, and a supervisor given one
// anyway cannot activate it, so gate and quiesce, which begin every stop,
// refuse.
func TestAnUnregisteredKindNeverStops(t *testing.T) {
	c := config{Harness: unregisteredKind, Instructions: instructions, InstructionDigest: digest(),
		ClientConfig: testClients, Env: []string{}, RolloutMaxBytes: 8192}
	if err := c.Validate(time.Now()); err == nil {
		t.Fatal("the launch contract accepted an unregistered kind")
	}
	if r := newServer(config{}, true).configure(&c); r.Error == "" {
		t.Fatalf("configure accepted an unregistered kind: %+v", r)
	}
	s := newServer(c, false)
	if r := s.activate(); r.Error != "unsupported native harness" || r.State != harness.NativePrepared {
		t.Fatalf("activate of an unregistered kind: %+v", r)
	}
	if r := s.gate(); r.Error != inactiveError {
		t.Fatalf("gate of an unregistered kind: %+v", r)
	}
	if r := s.quiesce(); r.Error != inactiveError {
		t.Fatalf("quiesce of an unregistered kind: %+v", r)
	}
	if r := s.status(); r.Waiting != "" || r.State != harness.NativePrepared {
		t.Fatalf("status of an unregistered kind: %+v", r)
	}
}

// What a graceful stop waits for, and how a task launch that resumes a
// conversation activates, are the recorded ones for every kind.
func TestWaitingOutcomesAreUnchanged(t *testing.T) {
	got, err := json.MarshalIndent(recordedWaitingOutcomes(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("SPROOZI_UPDATE_OUTCOMES") == "1" {
		if err := os.WriteFile(waitingOutcomes, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(waitingOutcomes)
	if err != nil {
		t.Fatalf("%v; record the outcomes with SPROOZI_UPDATE_OUTCOMES=1", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a wait text or task activation changed; %s records them. If the change is meant, regenerate it "+
			"with SPROOZI_UPDATE_OUTCOMES=1 and show the difference in its own commit", waitingOutcomes)
	}
}
