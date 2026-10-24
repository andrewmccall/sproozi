package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are journals written by stock Copilot 1.0.102 in a Linux
// container at the managed workspace path, against a scripted provider:
// turn.jsonl one answered prompt; resumed-tool.jsonl that session resumed by
// --session-id with a prompt answered after a bash tool call; error.jsonl a
// prompt whose model call the provider refused (403).
const (
	copilotFixtureSession = "0a7c5e1e-3b2d-4c4f-9a8b-1f2e3d4c5b6a"
	copilotErrorSession   = "1b8d6f2f-4c3e-4d5a-8b9c-2a3b4c5d6e7f"
)

func copilotFixture(t *testing.T, name, id string) CopilotState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "copilot", name))
	if err != nil {
		t.Fatal(err)
	}
	return CopilotState{ThreadID: id, Events: data}
}

// lastTurnEnd is the ID of the journal's last assistant.turn_end event.
func lastTurnEnd(t *testing.T, state CopilotState) string {
	t.Helper()
	id := ""
	for line := range bytes.SplitSeq(state.Events, []byte{'\n'}) {
		var e copilotEvent
		if len(line) != 0 && json.Unmarshal(line, &e) == nil && e.Type == "assistant.turn_end" {
			id = e.ID
		}
	}
	return id
}

func TestCopilotActualJournalsCompleteTheirLastTurn(t *testing.T) {
	for _, name := range []string{"turn.jsonl", "resumed-tool.jsonl"} {
		state := copilotFixture(t, name, copilotFixtureSession)
		turn, err := CompletedCopilotTurn(state, 1<<20)
		if err != nil || turn != lastTurnEnd(t, state) {
			t.Fatalf("%s: turn %q, err %v", name, turn, err)
		}
		if !CopilotPrompted(state) {
			t.Fatalf("%s: no prompt recorded", name)
		}
	}
}

// Every prefix of a journal is a moment a live observer can see. Only a
// prefix ending after the answer's turn_end is complete: not one ending on
// the turn that requested the tool, during the tool, or before the answer.
func TestCopilotPrefixesCompleteOnlyAfterTheAnswer(t *testing.T) {
	state := copilotFixture(t, "resumed-tool.jsonl", copilotFixtureSession)
	lines := bytes.SplitAfter(state.Events, []byte{'\n'})
	final := lastTurnEnd(t, state)
	first := lastTurnEnd(t, CopilotState{Events: bytes.Join(lines[:9], nil)})
	sawToolTurn := false
	for n := 1; n <= len(lines); n++ {
		prefix := CopilotState{ThreadID: copilotFixtureSession, Events: bytes.Join(lines[:n], nil)}
		if len(prefix.Events) == 0 {
			continue
		}
		turn, err := CompletedCopilotTurn(prefix, 1<<20)
		last := lastTurnEnd(t, prefix)
		resumed := bytes.Contains(prefix.Events, []byte(`"type":"session.resume"`))
		resumedPrompt := resumed && bytes.Count(prefix.Events, []byte(`"type":"user.message"`)) == 2
		switch {
		case !resumedPrompt && last == first && last != "":
			// The first session's answer, before the resumed prompt.
			if err != nil || turn != first {
				t.Fatalf("prefix %d: first answer not complete: %q %v", n, turn, err)
			}
		case resumedPrompt && last == final:
			if err != nil || turn != final {
				t.Fatalf("prefix %d: final answer not complete: %q %v", n, turn, err)
			}
		default:
			if !errors.Is(err, ErrCopilotTurnIncomplete) {
				t.Fatalf("prefix %d: complete too early: %q %v", n, turn, err)
			}
			if resumedPrompt && last != "" {
				sawToolTurn = true
			}
		}
	}
	if !sawToolTurn {
		t.Fatal("the tool-requesting turn_end was never observed as incomplete")
	}
}

func TestCopilotRefusedModelCallIsNotACompletedTurn(t *testing.T) {
	state := copilotFixture(t, "error.jsonl", copilotErrorSession)
	if _, err := CompletedCopilotTurn(state, 1<<20); !errors.Is(err, ErrCopilotTurnIncomplete) {
		t.Fatalf("refused call completed: %v", err)
	}
	if !CopilotPrompted(state) {
		t.Fatal("refused prompt not recorded")
	}
	// A first visit whose only prompt was refused has nothing to keep.
	if !CopilotTurnless(state, 1<<20) {
		t.Fatal("refused first prompt is not turnless")
	}
	for _, name := range []string{"turn.jsonl", "resumed-tool.jsonl"} {
		if CopilotTurnless(copilotFixture(t, name, copilotFixtureSession), 1<<20) {
			t.Fatalf("%s: a completed conversation is turnless", name)
		}
	}
	lines := bytes.SplitAfter(state.Events, []byte{'\n'})
	if !CopilotTurnless(CopilotState{ThreadID: copilotErrorSession, Events: bytes.Join(lines[:2], nil)}, 1<<20) {
		t.Fatal("a journal with no prompt is not turnless")
	}
	if CopilotTurnless(CopilotState{ThreadID: copilotErrorSession, Events: bytes.Join(lines[:5], nil)}, 1<<20) {
		t.Fatal("a turn still running is turnless")
	}
}

func TestCopilotJournalOfAnotherOrBrokenSessionRejected(t *testing.T) {
	state := copilotFixture(t, "turn.jsonl", copilotFixtureSession)
	other := state
	other.ThreadID = copilotErrorSession
	if _, err := CompletedCopilotTurn(other, 1<<20); err == nil {
		t.Fatal("journal accepted for another session")
	}
	lines := bytes.SplitAfter(state.Events, []byte{'\n'})
	orphan := bytes.Replace(lines[2], []byte(`"parentId":"`), []byte(`"parentId":"x`), 1)
	broken := bytes.Join(append(append([][]byte{}, lines[:2]...), append([][]byte{orphan}, lines[3:]...)...), nil)
	if _, err := CompletedCopilotTurn(CopilotState{ThreadID: copilotFixtureSession, Events: broken}, 1<<20); err == nil {
		t.Fatal("event with an unknown parent accepted")
	}
	if _, err := CompletedCopilotTurn(CopilotState{ThreadID: copilotFixtureSession, Events: bytes.Join(lines[1:], nil)}, 1<<20); err == nil {
		t.Fatal("journal without session.start accepted")
	}
	if _, err := CompletedCopilotTurn(state, int64(len(state.Events)-1)); err == nil {
		t.Fatal("oversized journal accepted")
	}
	if _, err := CompletedCopilotTurn(CopilotState{ThreadID: copilotFixtureSession, Events: bytes.TrimSuffix(state.Events, []byte("\n"))}, 1<<20); err == nil {
		t.Fatal("journal ending mid-line accepted")
	}
}

func TestCopilotCaptureAndFreshHomeRestore(t *testing.T) {
	source := copilotFixture(t, "resumed-tool.jsonl", copilotFixtureSession)
	home := t.TempDir()
	if err := RestoreCopilotState(home, source, 1<<20); err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureCopilotState(home, copilotFixtureSession, 1<<20)
	if err != nil || !bytes.Equal(captured.Events, source.Events) {
		t.Fatalf("capture: %v", err)
	}
	if err = RestoreCopilotState(home, source, 1<<20); err == nil {
		t.Fatal("restore overwrote an existing journal")
	}
	if _, err = CaptureCopilotState(home, copilotErrorSession, 1<<20); err == nil {
		t.Fatal("captured a session that isn't there")
	}
	if err = RestoreCopilotState(t.TempDir(), copilotFixture(t, "error.jsonl", copilotErrorSession), 1<<20); err == nil {
		t.Fatal("restored a journal with no completed turn")
	}
}

func TestCopilotStateRefusesLinkedDirectories(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(home, "session-state")); err != nil {
		t.Fatal(err)
	}
	state := copilotFixture(t, "turn.jsonl", copilotFixtureSession)
	if err := RestoreCopilotState(home, state, 1<<20); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("restore through a link: %v", err)
	}
	if _, err := CaptureCopilotState(home, copilotFixtureSession, 1<<20); err == nil {
		t.Fatal("capture through a link")
	}
}
