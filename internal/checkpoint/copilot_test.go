package checkpoint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// The journal is a stock Copilot 1.0.102 session resumed with a tool call
// (internal/harness/testdata/copilot).
func TestCopilotCheckpointCarriesOnlyTheSessionJournal(t *testing.T) {
	repository, _, _ := source(t)
	const id = "0a7c5e1e-3b2d-4c4f-9a8b-1f2e3d4c5b6a"
	events, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "copilot", "resumed-tool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err = harness.RestoreCopilotState(home, harness.CopilotState{ThreadID: id, Events: events}, checkpointLimit); err != nil {
		t.Fatal(err)
	}
	// Login, settings and Copilot's own index stay out of the checkpoint.
	for _, name := range []string{"config.json", "settings.json", "session-store.db",
		filepath.Join("session-state", id, "workspace.yaml")} {
		if err = os.WriteFile(filepath.Join(home, name), []byte("nonportable"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, meta, err := CaptureStopped(t.Context(), harness.NativeCopilot, home, repository, id, nil, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	turn, _ := harness.CompletedCopilotTurn(harness.CopilotState{ThreadID: id, Events: events}, checkpointLimit)
	if meta.Harness != harness.NativeCopilot || meta.ThreadID != id || meta.TurnID != turn || turn == "" {
		t.Fatalf("continuity metadata: %+v", meta)
	}
	if err = ValidateContinuity(data, harness.NativeClaude, strings.Repeat("0", 64), checkpointLimit); err == nil {
		t.Fatal("Copilot artifact admitted for Claude Code")
	}
	if err = ValidateContinuity(data, harness.NativeCopilot, strings.Repeat("0", 64), checkpointLimit); err != nil {
		t.Fatal(err)
	}
	if text, _, err := FinalText(data, checkpointLimit); err != nil || text != "Fixture reply." {
		t.Fatalf("final text %q: %v", text, err)
	}
	target := filepath.Join(t.TempDir(), "session")
	if restored, err := Restore(t.Context(), data, target, checkpointLimit); err != nil || restored != meta {
		t.Fatalf("restore: %v", err)
	}
	target, _ = filepath.EvalSymlinks(target)
	native := filepath.Join(target, "copilot")
	captured, err := harness.CaptureCopilotState(native, id, checkpointLimit)
	if err != nil || !bytes.Equal(captured.Events, events) {
		t.Fatalf("journal changed: %v", err)
	}
	for _, name := range []string{"config.json", "settings.json", "session-store.db",
		filepath.Join("session-state", id, "workspace.yaml")} {
		if _, err = os.Stat(filepath.Join(native, name)); !os.IsNotExist(err) {
			t.Fatalf("%s carried in portable state", name)
		}
	}
}
