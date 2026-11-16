package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// journalFormat is one harness's journal format: a complete journal, where
// capture finds it in a home, and the capture and the validator a checkpoint
// of it must pass.
type journalFormat struct {
	complete []byte
	path     string
	capture  func(home string) ([]byte, error)
	validate func(journal []byte) error
}

func journalFormats(t *testing.T) map[NativeKind]journalFormat {
	t.Helper()
	codex := completedNativeState()
	claude := claudeProofState(t)
	copilot := copilotFixture(t, "resumed-tool.jsonl", copilotFixtureSession)
	copilotPath, err := CopilotEventsPath(copilotFixtureSession)
	if err != nil {
		t.Fatal(err)
	}
	return map[NativeKind]journalFormat{
		NativeCodex: {complete: codex.Rollout, path: codex.Path,
			capture: func(home string) ([]byte, error) {
				s, err := CaptureCodexState(home, codex.ThreadID, 8192)
				return s.Rollout, err
			},
			validate: func(journal []byte) error {
				s := codex
				s.Rollout = journal
				_, err := CompletedCodexTurn(s, 8192)
				return err
			}},
		NativeClaude: {complete: claude.Transcript, path: claude.Path,
			capture: func(home string) ([]byte, error) {
				s, err := CaptureClaudeState(home, claude.Workspace, claude.ThreadID, 8192)
				return s.Transcript, err
			},
			validate: func(journal []byte) error {
				s := claude
				s.Transcript = journal
				_, err := ClaudeTranscriptLeaf(s, 8192)
				return err
			}},
		NativeCopilot: {complete: copilot.Events, path: copilotPath,
			capture: func(home string) ([]byte, error) {
				s, err := CaptureCopilotState(home, copilotFixtureSession, 1<<20)
				return s.Events, err
			},
			validate: func(journal []byte) error {
				_, err := CompletedCopilotTurn(CopilotState{ThreadID: copilotFixtureSession, Events: journal}, 1<<20)
				return err
			}},
	}
}

// writeJournal leaves journal in a fresh home at path, as a stopped writer
// left it.
func writeJournal(t *testing.T, path string, journal []byte) string {
	t.Helper()
	home := t.TempDir()
	name := filepath.Join(home, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// A harness killed while appending a row to its journal leaves a final line
// without its newline. Each journal is append-only JSON lines, so the rows
// before that line are exactly the journal at its last completed write.
// Capture runs only after the placement proved the writer stopped, so it
// keeps those rows and leaves the unfinished line out; the validator still
// decides whether they hold a completed turn. A row cut after the completed
// turn keeps the turn, and one cut inside it keeps nothing.
func TestCaptureLeavesOutTheRowAStoppedWriterDidNotFinish(t *testing.T) {
	for name, j := range journalFormats(t) {
		t.Run(string(name), func(t *testing.T) {
			if err := j.validate(j.complete); err != nil {
				t.Fatalf("the complete journal: %v", err)
			}
			// A row appended after the completed turn, cut short.
			appended := append(bytes.Clone(j.complete), `{"type":"event_msg","payload":{"type":"tok`...)
			captured, err := j.capture(writeJournal(t, j.path, appended))
			if err != nil || !bytes.Equal(captured, j.complete) {
				t.Fatalf("a row cut after the completed turn: err %v, kept %d of %d complete bytes",
					err, len(captured), len(j.complete))
			}
			// The final row itself, cut short anywhere: capture keeps the
			// rows before it exactly when the validator accepts them.
			last := bytes.LastIndexByte(j.complete[:len(j.complete)-1], '\n') + 1
			before := j.complete[:last]
			wantErr := j.validate(before)
			for _, cut := range []int{last + 1, (last + len(j.complete)) / 2, len(j.complete) - 1} {
				captured, err = j.capture(writeJournal(t, j.path, j.complete[:cut]))
				if wantErr == nil && (err != nil || !bytes.Equal(captured, before)) {
					t.Fatalf("cut at %d: err %v, kept %d of %d bytes", cut, err, len(captured), len(before))
				}
				if wantErr != nil && err == nil {
					t.Fatalf("cut at %d inside the final row: captured a journal the validator rejects (%v)", cut, wantErr)
				}
			}
			// A journal with no complete row at all keeps nothing.
			if _, err = j.capture(writeJournal(t, j.path, j.complete[:bytes.IndexByte(j.complete, '\n')])); err == nil {
				t.Fatal("captured a journal with no complete row")
			}
		})
	}
}

// The validators the live observers use refuse a final row cut short inside
// its JSON: the writer may still be running, so they wait for the next poll.
// Codex's validator does accept a final row that is complete JSON without its
// newline; the supervisor's journal observer refuses that one before it
// (cmd/native-session/observer.go), while Claude Code's and Copilot's
// validators refuse any journal without a final newline.
func TestValidatorsStillRefuseAnUnfinishedFinalRow(t *testing.T) {
	const row = `{"type":"event_msg","payload":{"type":"token_count"}}`
	for name, j := range journalFormats(t) {
		cut := append(bytes.Clone(j.complete), row[:len(row)/2]...)
		if err := j.validate(cut); err == nil {
			t.Fatalf("%s: the validator accepted a row cut inside its JSON", name)
		}
		unterminated := append(bytes.Clone(j.complete), row...)
		if err := j.validate(unterminated); (err == nil) != (name == NativeCodex) {
			t.Fatalf("%s: a complete final row without its newline: %v", name, err)
		}
	}
}
