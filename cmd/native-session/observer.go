package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// observeTurn reads a bounded native journal while input is gated. It is neither
// capture nor a writer fence. The workload can modify this journal; a successful
// observation only permits graceful quit before the runtime independently stops
// and observes the entire container.
func observeTurn(home, selected, instructionDigest string, maxBytes int64) (string, string, error) {
	return observeTaskTurn(home, selected, instructionDigest, maxBytes, "", "")
}

func observeTaskTurn(home, selected, instructionDigest string, maxBytes int64,
	task, baseline string,
) (string, string, error) {
	var state harness.CodexState
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("native journal symlink is unsupported")
		}
		if entry.IsDir() || filepath.Ext(name) != ".jsonl" {
			return nil
		}
		if selected != "" && !strings.HasSuffix(filepath.Base(name), "-"+selected+".jsonl") {
			return nil
		}
		if state.ThreadID != "" {
			return errors.New("ambiguous native journal")
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > maxBytes {
			return errors.New("native journal exceeds limit")
		}
		// A writer may have appended a partial final row. Retry after the next poll.
		if !bytes.HasSuffix(data, []byte{'\n'}) {
			return errors.New("native journal row is incomplete")
		}
		relative, err := filepath.Rel(home, name)
		if err != nil {
			return err
		}
		var first struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		line, _, _ := bytes.Cut(data, []byte{'\n'})
		if json.Unmarshal(line, &first) != nil || first.Type != "session_meta" || first.Payload.ID == "" {
			return errors.New("native journal metadata is unavailable")
		}
		state = harness.CodexState{ThreadID: first.Payload.ID, Path: filepath.ToSlash(relative), Rollout: data}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	turn, err := harness.CompletedCodexTurn(state, maxBytes)
	if err != nil {
		return "", "", err
	}
	if err = harness.ValidateCodexContinuity(state, instructionDigest, maxBytes); err != nil {
		return "", "", err
	}
	if task != "" && (turn == baseline || !taskInTurn(state.Rollout, turn, task)) {
		return "", "", errors.New("submitted native task has no new completed turn")
	}
	return state.ThreadID, turn, nil
}

// taskInTurn binds completion to the actual submitted user text in the new
// native turn. A restored completed leaf cannot satisfy an initial task request.
func taskInTurn(rollout []byte, turn, task string) bool {
	var active string
	var matched bool
	for line := range bytes.SplitSeq(rollout, []byte{'\n'}) {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				TurnID  string `json:"turn_id"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if row.Type == "event_msg" && row.Payload.Type == "task_started" {
			active, matched = row.Payload.TurnID, false
		}
		if active == turn && row.Type == "response_item" && row.Payload.Type == "message" && row.Payload.Role == "user" {
			var text strings.Builder
			for _, c := range row.Payload.Content {
				if c.Type == "input_text" {
					text.WriteString(c.Text)
				}
			}
			if text.String() == task {
				matched = true
			}
		}
	}
	return active == turn && matched
}

// codexJournalExists reports whether Codex has started any native journal.
// Codex writes its rollout only once a turn starts, so a home without one
// has no conversation. Anything it cannot read counts as a journal, so an
// unreadable home never makes a visit turnless.
func codexJournalExists(home string) bool {
	found := false
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found || err != nil && !errors.Is(err, fs.ErrNotExist)
}
