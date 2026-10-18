package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
)

var codexThreadID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var codexRolloutPath = regexp.MustCompile(`^sessions/[0-9]{4}/[0-9]{2}/[0-9]{2}/rollout-[0-9T-]+-[0-9a-f-]+\.jsonl$`)

// CodexState contains conversation data, never authority. Launch configuration,
// authentication, hooks and plugins must be generated independently on restore.
type CodexState struct {
	ThreadID string
	// Workspace is the original session metadata path. Resume receives an
	// explicit destination cwd; the opaque original history is not rewritten.
	Workspace string
	Path      string
	Rollout   []byte
}

// CaptureCodexState selects one native rollout after the supervisor has stopped
// every native writer. A completed-turn marker alone does not prove process stop.
// Only the selected native session file is selected; no home or index DB is copied.
func CaptureCodexState(home, threadID string, maxBytes int64) (CodexState, error) {
	if !codexThreadID.MatchString(threadID) || maxBytes <= 0 {
		return CodexState{}, fmt.Errorf("invalid Codex checkpoint selection")
	}
	root := filepath.Join(home, "sessions")
	var result CodexState
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("native session symlinks are unsupported")
		}
		if entry.IsDir() || filepath.Ext(name) != ".jsonl" {
			return nil
		}
		if !bytes.HasSuffix([]byte(filepath.Base(name)), []byte("-"+threadID+".jsonl")) {
			return nil
		}
		if result.ThreadID != "" {
			return fmt.Errorf("ambiguous native thread state")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxBytes {
			return fmt.Errorf("native state exceeds checkpoint limit")
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if int64(len(data)) > maxBytes {
			return fmt.Errorf("native state exceeds checkpoint limit")
		}
		relative, err := filepath.Rel(home, name)
		if err != nil {
			return err
		}
		result = CodexState{ThreadID: threadID, Path: filepath.ToSlash(relative), Rollout: data}
		workspace, _, err := inspectCodexState(result, maxBytes)
		result.Workspace = workspace
		return err
	})
	if err != nil {
		return CodexState{}, err
	}
	if result.ThreadID == "" {
		return CodexState{}, fmt.Errorf("native thread state is unavailable")
	}
	return result, nil
}

// CompletedCodexTurn observes the native completion event. The supervisor
// must separately block input and confirm native process/descendant termination.
func CompletedCodexTurn(state CodexState, maxBytes int64) (string, error) {
	_, turn, err := inspectCodexState(state, maxBytes)
	return turn, err
}

// RestoreCodexState installs only conversation data into a prepared native home.
// The caller owns that home exclusively until activation and supplies fresh
// trusted configuration separately. Existing conversation files are never replaced.
func RestoreCodexState(home string, state CodexState, maxBytes int64) error {
	if _, _, err := inspectCodexState(state, maxBytes); err != nil {
		return err
	}
	info, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("native home must be an owned directory")
	}
	dir := home
	parts := bytes.SplitSeq([]byte(path.Dir(state.Path)), []byte{'/'})
	for part := range parts {
		dir = filepath.Join(dir, string(part))
		if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err = os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe native restore directory")
		}
	}
	name := filepath.Join(home, filepath.FromSlash(state.Path))
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(state.Rollout)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return nil
}

func inspectCodexState(state CodexState, maxBytes int64) (string, string, error) {
	if err := validateCodexSelection(state, maxBytes); err != nil {
		return "", "", err
	}
	var workspace, active, complete string
	meta := false
	for line := range bytes.SplitSeq(state.Rollout, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return "", "", fmt.Errorf("malformed native rollout: %w", err)
		}
		switch event.Type {
		case "session_meta":
			var value struct {
				ID      string `json:"id"`
				Version string `json:"cli_version"`
				Cwd     string `json:"cwd"`
			}
			if err := json.Unmarshal(event.Payload, &value); err != nil || meta || value.ID != state.ThreadID || value.Version == "" || (!path.IsAbs(value.Cwd) && !filepath.IsAbs(value.Cwd)) {
				return "", "", fmt.Errorf("incompatible native session metadata")
			}
			workspace, meta = value.Cwd, true
		case "event_msg":
			var value struct {
				Type  string          `json:"type"`
				Turn  string          `json:"turn_id"`
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(event.Payload, &value); err != nil {
				return "", "", fmt.Errorf("malformed native event")
			}
			switch value.Type {
			case "task_started":
				active, complete = value.Turn, ""
			case "task_complete":
				if active == "" || value.Turn != active {
					return "", "", fmt.Errorf("unmatched native turn completion")
				}
				if len(value.Error) > 0 && !bytes.Equal(bytes.TrimSpace(value.Error), []byte("null")) {
					complete = ""
				} else {
					complete = value.Turn
				}
			case "turn_aborted":
				complete = ""
			}
		}
	}
	if !meta || complete == "" || (state.Workspace != "" && state.Workspace != workspace) {
		return "", "", fmt.Errorf("native state has no completed final turn")
	}
	return workspace, complete, nil
}

func validateCodexSelection(state CodexState, maxBytes int64) error {
	if !codexThreadID.MatchString(state.ThreadID) || maxBytes <= 0 || len(state.Rollout) == 0 || int64(len(state.Rollout)) > maxBytes {
		return fmt.Errorf("invalid or oversized native checkpoint")
	}
	clean := filepath.ToSlash(filepath.Clean(state.Path))
	if clean != state.Path || filepath.IsAbs(state.Path) || !codexRolloutPath.MatchString(clean) {
		return fmt.Errorf("invalid native checkpoint path")
	}
	if !bytes.HasSuffix([]byte(clean), []byte("-"+state.ThreadID+".jsonl")) {
		return fmt.Errorf("native path does not identify selected thread")
	}
	return nil
}
