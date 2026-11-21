package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FinalText of a state with no completed turn returns CompletedTurn's error,
// whatever the format, rather than an answer from an unfinished turn.
func TestFinalTextRefusesAStateWithNoCompletedTurn(t *testing.T) {
	const thread = "01a126e8-288d-7350-9b52-627e16048343"
	claudePath, err := ClaudeProjectPath("/workspace/session/workspace", thread)
	if err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]ConversationState{
		"Codex": CodexState{ThreadID: thread,
			Path: "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + thread + ".jsonl",
			Rollout: []byte(`{"type":"session_meta","payload":{"id":"` + thread + `","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"open-turn"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"Half an answer."}}
`)},
		"Claude Code": ClaudeState{ThreadID: thread, Workspace: "/workspace/session/workspace", Path: claudePath,
			Transcript: []byte(`{"type":"user","sessionId":"` + thread + `","uuid":"11111111-1111-4111-8111-111111111111",` +
				`"parentUuid":null,"isSidechain":false,"cwd":"/workspace","version":"2.1.318",` +
				`"message":{"role":"user","content":"Start"}}` + "\n")},
		"Copilot": copilotFixture(t, "error.jsonl", copilotErrorSession),
	} {
		t.Run(name, func(t *testing.T) {
			_, turnErr := state.CompletedTurn(1 << 20)
			text, err := state.FinalText(1 << 20)
			if turnErr == nil || err == nil || err.Error() != turnErr.Error() || text != "" {
				t.Fatalf("FinalText %q, %v; want CompletedTurn's error %v", text, err, turnErr)
			}
		})
	}
}

// The directory walk checks the native home itself: a home that is a link,
// even to a real directory, is refused on capture and on restore.
func TestNativeStateRefusesALinkedHome(t *testing.T) {
	real := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, home); err != nil {
		t.Fatal(err)
	}
	if err := stateDirectories(home, "a/b", true, "unsafe fixture directory"); !errors.Is(err, ErrStateRefused) ||
		!strings.Contains(err.Error(), "unsafe fixture directory") {
		t.Fatalf("stateDirectories through a linked home: %v", err)
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("stateDirectories created %v through a linked home", entries)
	}
	copilot := copilotFixture(t, "turn.jsonl", copilotFixtureSession)
	if err := RestoreCopilotState(home, copilot, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("Copilot restore into a linked home: %v", err)
	}
	if _, err := CaptureCopilotState(home, copilotFixtureSession, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("Copilot capture from a linked home: %v", err)
	}
	// Claude Code's restore validates the transcript before it walks, and
	// shares the walk with its capture.
	const thread = "26386254-894d-4504-9bac-23ab929077cd"
	if _, err := CaptureClaudeState(home, "/workspace/session/workspace", thread, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("Claude Code capture from a linked home: %v", err)
	}
}
