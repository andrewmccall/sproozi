package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func TestClaudeLifecycleWaitsForForegroundToolsAndFreshInput(t *testing.T) {
	c := claudeLifecycle{}
	inactive := false
	if c.accept(claudeHook{Event: claudeStop, StopActive: &inactive}, 0) == nil {
		t.Fatal("Stop without a submitted turn accepted")
	}
	if err := c.accept(claudeHook{Event: claudePromptSubmit}, 1); err != nil {
		t.Fatal(err)
	}
	tool := claudeHook{Event: claudePreTool, ToolID: "toolu_literal", ToolName: claudeBash}
	if err := c.accept(tool, 1); err != nil {
		t.Fatal(err)
	}
	if c.accept(claudeHook{Event: claudeStop, StopActive: &inactive}, 1) == nil {
		t.Fatal("Stop with a foreground tool accepted")
	}
	tool.Event = claudePostTool
	if err := c.accept(tool, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(claudeHook{Event: claudeStop, StopActive: &inactive}, 1); err != nil {
		t.Fatal(err)
	}
	s := server{claude: c, inputGeneration: 2}
	if _, _, err := s.observeClaudeTurn(); err == nil {
		t.Fatal("prior Stop reused after fresh accepted input")
	}
	if err := c.accept(claudeHook{Event: claudePromptSubmit}, 2); err != nil {
		t.Fatal(err)
	}
	if c.completed {
		t.Fatal("submitted turn retained earlier completion")
	}
	if c.accept(claudeHook{Event: claudePreTool, ToolID: "task", ToolName: "Task"}, 2) == nil {
		t.Fatal("unsupported delegation accepted")
	}
}

func TestClaudeHookBoundToExactNativeSelection(t *testing.T) {
	s := server{config: config{Harness: harness.NativeClaude, ThreadID: selected}, state: harness.NativeActive}
	path, _ := harness.ClaudeProjectPath(workspace, selected)
	h := &claudeHook{SessionID: selected, TranscriptPath: claudeHome + "/" + path, Cwd: workspace,
		Event: claudePromptSubmit}
	if got := s.claudeHook(h); got.Error != "" {
		t.Fatal(got.Error)
	}
	h.TranscriptPath = "/other/" + path
	if got := s.claudeHook(h); got.Error == "" {
		t.Fatal("foreign hook path accepted")
	}
	h.TranscriptPath = claudeHome + "/" + path
	h.SessionID = "00000000-0000-0000-0000-000000000000"
	if got := s.claudeHook(h); got.Error == "" {
		t.Fatal("foreign native hook ID accepted")
	}
	s.state = harness.NativePrepared
	if got := s.claudeHook(h); got.Error == "" {
		t.Fatal("inert worker could emit a completion hint")
	}
}

func TestClaudeHookBoundedPinnedPayload(t *testing.T) {
	raw := `{"session_id":"` + selected + `","transcript_path":"/selected","cwd":"/workspace",` +
		`"hook_event_name":"Stop","stop_hook_active":false,"permission_mode":"bypassPermissions"}`
	h, err := readClaudeHook(strings.NewReader(raw))
	if err != nil || h.Event != claudeStop || h.StopActive == nil || *h.StopActive {
		t.Fatalf("pinned payload: %v", err)
	}
	if _, err = readClaudeHook(strings.NewReader(raw + "{}")); err == nil {
		t.Fatal("trailing hook JSON accepted")
	}
	if _, err = readClaudeHook(strings.NewReader(strings.Repeat(" ", 65537))); err == nil {
		t.Fatal("unbounded hook input accepted")
	}
}
func TestExplicitHarnessContractAndClaudeOptionBoundary(t *testing.T) {
	c := config{Harness: harness.NativeClaude, Deadline: time.Now().Add(time.Minute), Instructions: instructions,
		InstructionDigest: digest(), ClientConfig: testClients, Env: []string{testPathEnvironment}, RolloutMaxBytes: 8192, Model: "claude-sonnet-4-5"}
	if err := c.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []harness.NativeKind{"", "unknown"} {
		c.Harness = kind
		if c.Validate(time.Now()) == nil {
			t.Fatal("implicit or unknown harness accepted")
		}
	}
	c.Harness = harness.NativeClaude
	c.Options = []string{"developer_instructions=\"changed\""}
	if c.Validate(time.Now()) == nil {
		t.Fatal("Codex option reinterpreted for Claude")
	}
	c.Options = nil
	c.Env = append(c.Env, "CLAUDE_CONFIG_DIR=/untrusted")
	if c.Validate(time.Now()) == nil {
		t.Fatal("portable home override accepted")
	}
}
func TestClaudePrivateSettingsRetryIsExact(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	value := map[string]any{claudeHooksKey: map[string]any{}}
	if err := installClaudeJSON(path, value); err != nil {
		t.Fatal(err)
	}
	if err := installClaudeJSON(path, value); err != nil {
		t.Fatal(err)
	}
	if err := installClaudeJSON(path, map[string]any{claudeHooksKey: "different"}); err == nil {
		t.Fatal("changed settings retry accepted")
	}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeStatusExplainsCompletionBarrierWithoutPayloads(t *testing.T) {
	s := server{config: config{Harness: harness.NativeClaude, ThreadID: selected},
		state: harness.NativeStopping, threadID: selected}
	path, _ := harness.ClaudeProjectPath(workspace, selected)
	h := &claudeHook{SessionID: selected, TranscriptPath: claudeHome + "/" + path, Cwd: workspace,
		Event: claudePromptSubmit}
	s.inputGeneration = 2
	if got := s.claudeHook(h); got.Error != "" {
		t.Fatal(got.Error)
	}
	h.Event, h.ToolName, h.ToolID = claudePreTool, claudeBash, "private-tool-id"
	if got := s.claudeHook(h); got.Error != "" {
		t.Fatal(got.Error)
	}
	if got := s.status().Completion; got.Observation != "pending_tools" || got.PendingTools != 1 {
		t.Fatalf("pending tool diagnostic: %+v", got)
	}
	h.Event = claudePostTool
	if got := s.claudeHook(h); got.Error != "" {
		t.Fatal(got.Error)
	}
	inactive := false
	h.Event, h.StopActive = claudeStop, &inactive
	if got := s.claudeHook(h); got.Error != "" {
		t.Fatal(got.Error)
	}
	s.inputGeneration = 3
	d := s.status().Completion
	if d.Observation != "input_changed" || !d.Completed || d.PromptGeneration != 2 || d.InputGeneration != 3 {
		t.Fatalf("input diagnostic: %+v", d)
	}
	encoded, err := json.Marshal(s.status())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{workspace, path, h.ToolID} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("completion diagnostic disclosed a private payload")
		}
	}
	s.inputGeneration = 2
	if got := s.status().Completion; got.Observation != journalUnavailable {
		t.Fatalf("missing selected journal diagnostic: %+v", got)
	}
	h.TranscriptPath = "/foreign/private/path"
	if got := s.claudeHook(h); got.Error == "" {
		t.Fatal("foreign hook accepted")
	}
	if got := s.status().Completion; got.Rejected != "identity_conflict" {
		t.Fatalf("rejected hook diagnostic: %+v", got)
	}
}

func TestClaudeJournalDiagnosticsExposeOnlyFixedCategories(t *testing.T) {
	for _, tc := range []struct{ message, category string }{
		{"unsupported Claude stop hook summary", "journal_stop_summary_invalid"},
		{"incompatible Claude primary transcript chain", "journal_primary_chain_invalid"},
		{"unmatched Claude post-tool hook progress", "journal_hook_progress_invalid"},
		{"claude transcript has no settled final assistant leaf", "journal_leaf_unsettled"},
		{"unsupported Claude content block private-provider-content", journalUnavailable},
		{"open /private/native/file: denied", journalUnavailable},
	} {
		if got := claudeJournalDiagnostic(errors.New(tc.message)); got != tc.category {
			t.Fatalf("fixed diagnostic category: got %s, want %s", got, tc.category)
		}
	}
}
