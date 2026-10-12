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
	return state.ThreadID, turn, nil
}
