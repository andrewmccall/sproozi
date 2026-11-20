package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

func startIdleNative(t *testing.T) *nativepty.Supervisor {
	t.Helper()
	return startNative(t, "while :; do sleep 1; done", time.Now().Add(time.Minute))
}

func setTurnlessSettle(t *testing.T, d time.Duration) {
	t.Helper()
	previous := turnlessSettle
	turnlessSettle = d
	t.Cleanup(func() { turnlessSettle = previous })
}

// A first interactive Codex visit with no native journal stops promptly
// instead of waiting until its deadline for a turn that never comes.
func TestTurnlessFirstVisitQuiescesWithoutConversation(t *testing.T) {
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
	if r := s.status(); r.Turnless {
		t.Fatal("turnless before gated input settled")
	}
	started := time.Now()
	r := s.quiesce()
	if r.Error != "" || r.State != harness.NativeStopped || !r.Turnless || r.ThreadID != "" {
		t.Fatalf("turnless quiesce: %+v", r)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("turnless quiesce took %s", elapsed)
	}
	select {
	case <-native.Done():
	default:
		t.Fatal("turnless quiesce left the harness running")
	}
}

func TestTurnlessNeedsAnInteractiveFirstVisit(t *testing.T) {
	setTurnlessSettle(t, 0)
	for name, c := range map[string]config{
		"selected thread": {Harness: harness.NativeCodex, ThreadID: selected},
		"task":            {Harness: harness.NativeCodex, Task: "fix it"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &server{config: c, life: lifecycle{phase: phaseGated, gatedAt: time.Now().Add(-time.Second)}}
			if s.isTurnless() {
				t.Fatal("visit with a conversation counted as turnless")
			}
		})
	}
	s := &server{config: config{Harness: harness.NativeCodex}, life: lifecycle{phase: phaseActive}}
	if s.isTurnless() {
		t.Fatal("ungated visit counted as turnless")
	}
}

func TestCodexJournalExists(t *testing.T) {
	home := t.TempDir()
	if codexJournalExists(home) {
		t.Fatal("empty home has a journal")
	}
	if err := os.MkdirAll(filepath.Join(home, "sessions", "2026"), 0700); err != nil {
		t.Fatal(err)
	}
	if codexJournalExists(home) {
		t.Fatal("empty sessions directory has a journal")
	}
	if codexJournalExists(writeJournal(t, journal(""))) == false {
		t.Fatal("rollout journal not found")
	}
}

func TestClaudeTurnlessOnlyWithoutACompletedTurn(t *testing.T) {
	inactive := false
	s := &server{}
	if !s.claudeTurnless() {
		t.Fatal("no submitted prompt is not turnless")
	}
	c := claudeLifecycle{}
	if c.accept(claudeHook{Event: claudeStopFailure}, 0) == nil {
		t.Fatal("turn failure without a started turn accepted")
	}
	if err := c.accept(claudeHook{Event: claudePromptSubmit}, 1); err != nil {
		t.Fatal(err)
	}
	s = &server{claude: c}
	if s.claudeTurnless() {
		t.Fatal("turn in progress counted as turnless")
	}
	if err := c.accept(claudeHook{Event: claudeStopFailure}, 1); err != nil {
		t.Fatal(err)
	}
	s = &server{claude: c}
	if !s.claudeTurnless() {
		t.Fatal("only turn failed but visit is not turnless")
	}
	// A later completed turn keeps the conversation.
	if err := c.accept(claudeHook{Event: claudePromptSubmit}, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(claudeHook{Event: claudeStop, StopActive: &inactive}, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(claudeHook{Event: claudePromptSubmit}, 3); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(claudeHook{Event: claudeStopFailure}, 3); err != nil {
		t.Fatal(err)
	}
	s = &server{claude: c}
	if s.claudeTurnless() {
		t.Fatal("conversation with a completed turn counted as turnless")
	}
}

func TestHarnessArgsFollowSprooziFlagsAndReplaceTheModel(t *testing.T) {
	c := config{Harness: harness.NativeCodex, Instructions: instructions, Model: "gpt-admin",
		Args: []string{"--search", "-c", "model_reasoning_effort=high"}}
	args := codexArguments(c, selected)
	resume := slices.Index(args, "resume")
	search := slices.Index(args, "--search")
	if search < 0 || resume < search || !slices.Contains(args, "gpt-admin") {
		t.Fatalf("codex arguments: %q", args)
	}
	c.Args = []string{"--model", "gpt-user"}
	args = codexArguments(c, "")
	if slices.Contains(args, "gpt-admin") || !slices.Contains(args, "gpt-user") {
		t.Fatalf("user model did not replace the configured one: %q", args)
	}
	claude := claudeArguments(config{Harness: harness.NativeClaude, ThreadID: selected, Model: "claude-admin",
		Args: []string{"--model=claude-user", "--effort", "high"}, ClientConfig: testClients}, agentprofile.Profile{}, false)
	if slices.Contains(claude, "claude-admin") || slices.Index(claude, "--effort") > slices.Index(claude, "--resume") {
		t.Fatalf("claude arguments: %q", claude)
	}
}

func TestMigrationNoticeAcknowledgesEveryCatalogUpgrade(t *testing.T) {
	notice, err := migrationNotice([]byte(`{"models":[{"slug":"gpt-5.5","upgrade":{"model":"gpt-6-sol"}},` +
		`{"slug":"gpt-6-sol","upgrade":null},{"slug":"bad slug","upgrade":{"model":"x"}},` +
		`{"slug":"gpt-5.6-luna","upgrade":{"model":"gpt-6-luna"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `notice.model_migrations={"gpt-5.5"="gpt-6-sol","gpt-5.6-luna"="gpt-6-luna"}`
	if notice != want {
		t.Fatalf("notice %q, want %q", notice, want)
	}
	if notice, err = migrationNotice([]byte(`{"models":[]}`)); err != nil || notice != "" {
		t.Fatalf("empty catalog: %q %v", notice, err)
	}
	if _, err = migrationNotice([]byte("not json")); err == nil || strings.Contains(err.Error(), "gpt") {
		t.Fatal("unreadable catalog accepted")
	}
}

// Terminal input after a completed Claude turn, such as a focus report or a
// terminal's reply to a query, no longer blocks a stop forever: once gated
// input and hooks are quiet, the completed turn stands. A prompt that input
// submitted reports itself and clears the completion.
func TestClaudeCompletedTurnSurvivesSettledTerminalInput(t *testing.T) {
	inactive := false
	c := claudeLifecycle{}
	if err := c.accept(claudeHook{Event: claudePromptSubmit}, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(claudeHook{Event: claudeStop, StopActive: &inactive}, 1); err != nil {
		t.Fatal(err)
	}
	quiet := time.Now().Add(-time.Minute)
	c.lastHook = quiet
	s := &server{claude: c, inputGeneration: 3, life: lifecycle{phase: phaseActive}, lastInputAt: time.Now()}
	if s.claudeTurnReady() {
		t.Fatal("ungated fresh input accepted the earlier Stop")
	}
	s.life.phase, s.life.gatedAt = phaseGated, time.Now()
	if s.claudeTurnReady() {
		t.Fatal("unsettled input accepted the earlier Stop")
	}
	s.lastInputAt = quiet
	if !s.claudeTurnReady() {
		t.Fatal("settled terminal input still blocks the completed turn")
	}
	if err := s.claude.accept(claudeHook{Event: claudePromptSubmit}, 3); err != nil {
		t.Fatal(err)
	}
	if s.claudeTurnReady() {
		t.Fatal("a submitted prompt kept the earlier completion")
	}
}

// A quiesce that cannot see a completed turn within its bound says what it
// waits for and leaves the harness running with input gated.
func TestQuiesceReportsWhatItWaitsForWithoutStopping(t *testing.T) {
	previous := quiesceWait
	quiesceWait = 200 * time.Millisecond
	t.Cleanup(func() { quiesceWait = previous })
	native := startIdleNative(t)
	deadline := time.Now().Add(time.Minute)
	s := &server{native: native, life: lifecycle{phase: phaseActive}, threadID: selected, done: make(chan struct{}),
		config: config{Harness: harness.NativeCodex, ThreadID: selected, Deadline: deadline}, deadline: deadline}
	r := s.quiesce()
	if r.Error != "" || r.State != harness.NativeStopping || r.Waiting == "" {
		t.Fatalf("bounded quiesce: %+v", r)
	}
	select {
	case <-native.Done():
		t.Fatal("bounded quiesce stopped the harness")
	default:
	}
	if st := s.status(); st.State != harness.NativeStopping {
		t.Fatalf("state after bounded quiesce: %+v", st)
	}
}

// The Stop hook is the only report that a Claude Code turn ended; the
// transcript is read only after it. A Stop that never arrives, such as a hook
// killed by Claude Code's hook timeout before it reported, leaves the turn
// running: each stop waits its bound, reports what it waits for and leaves
// the harness running. Only a later turn's Stop, a forced stop or the
// execution deadline ends that.
func TestClaudeMissedStopLeavesTheTurnRunning(t *testing.T) {
	previous := quiesceWait
	quiesceWait = 200 * time.Millisecond
	t.Cleanup(func() { quiesceWait = previous })
	native := startIdleNative(t)
	deadline := time.Now().Add(time.Minute)
	s := &server{native: native, life: lifecycle{phase: phaseActive}, done: make(chan struct{}),
		config: config{Harness: harness.NativeClaude, ThreadID: selected, Deadline: deadline}, deadline: deadline}
	s.claude.id = selected
	if err := s.claude.accept(claudeHook{Event: claudePromptSubmit}, 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r := s.quiesce()
		if r.Error != "" || r.State != harness.NativeStopping || r.Waiting != "the current Claude Code turn to finish" {
			t.Fatalf("stop after a missed Stop: %+v", r)
		}
	}
	select {
	case <-native.Done():
		t.Fatal("a stop ended the harness without a completed turn")
	default:
	}
}

// A resumed Claude visit with no new prompt keeps its restored conversation:
// a stop needs no new turn once any terminal input has settled.
func TestClaudeResumedVisitWithoutPromptStopsAsRestored(t *testing.T) {
	s := &server{config: config{Harness: harness.NativeClaude, ThreadID: selected},
		life: lifecycle{phase: phaseGated, gatedAt: time.Now()}}
	if !s.claudeRestoredUnchanged() {
		t.Fatal("resumed visit without input is not its restored conversation")
	}
	s.inputGeneration, s.lastInputAt = 2, time.Now()
	if s.claudeRestoredUnchanged() {
		t.Fatal("unsettled input accepted as unchanged")
	}
	s.lastInputAt = time.Now().Add(-time.Minute)
	if !s.claudeRestoredUnchanged() {
		t.Fatal("settled terminal input blocks the restored conversation")
	}
	if err := s.claude.accept(claudeHook{Event: claudePromptSubmit}, 2); err != nil {
		t.Fatal(err)
	}
	if s.claudeRestoredUnchanged() {
		t.Fatal("a submitted prompt still counts as the restored conversation")
	}
	first := &server{config: config{Harness: harness.NativeClaude}, life: lifecycle{phase: phaseGated}}
	if first.claudeRestoredUnchanged() {
		t.Fatal("a first visit has no restored conversation")
	}
}

func TestReportExitCarriesCodeOutputAndThread(t *testing.T) {
	native := startIdleNative(t)
	s := &server{native: native, threadID: selected, config: config{Harness: harness.NativeCodex}}
	out := captureStdout(t, func() { s.reportExit(native, harness.NativeExitSelf, 2) })
	report, ok := harness.ParseNativeExitReport([]byte(out))
	if !ok || report.Code != 2 || report.Cause != harness.NativeExitSelf || report.Thread != selected {
		t.Fatalf("exit report %q: %+v %v", out, report, ok)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = previous
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Every managed harness in the registry needs a supervisor adapter.
func TestEveryManagedKindHasAnAdapter(t *testing.T) {
	for _, kind := range harness.ManagedKinds() {
		if _, err := adapterFor(kind); err != nil {
			t.Errorf("managed harness %q has no supervisor adapter; add one to nativeAdapters in "+
				"cmd/native-session/launch.go", kind)
		}
	}
}

// Each adapter launches the registry's executable for its kind, so the
// supervisor keeps no path of its own.
func TestAdaptersLaunchTheRegistryExecutable(t *testing.T) {
	for _, kind := range harness.ManagedKinds() {
		adapter, err := adapterFor(kind)
		if err != nil {
			continue
		}
		if want := harness.NativeExecutable(kind); want == "" || adapter.executable != want {
			t.Errorf("managed harness %q launches %q, but its Executable in internal/harness/registry.go is %q",
				kind, adapter.executable, want)
		}
	}
}
