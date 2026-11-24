package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	opOpen      = "open"
	rolloutName = "rollout"
)

// A failure to reach or read the agent's own file is the fenced state's, and
// refused; an I/O error from the volume is the environment's, and not.
func TestStateFileErrorDecidesByKind(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		err     error
		refused bool
	}{
		{"absent", &fs.PathError{Op: opOpen, Path: rolloutName, Err: fs.ErrNotExist}, true},
		{"permission", &fs.PathError{Op: opOpen, Path: rolloutName, Err: syscall.EACCES}, true},
		{"not a directory", &fs.PathError{Op: "lstat", Path: "sessions/x", Err: syscall.ENOTDIR}, true},
		{"link loop", &fs.PathError{Op: opOpen, Path: rolloutName, Err: syscall.ELOOP}, true},
		{"volume I/O error", &fs.PathError{Op: "read", Path: rolloutName, Err: syscall.EIO}, false},
		{"other", fmt.Errorf("read: %w", syscall.ENOSPC), false},
	} {
		if got := errors.Is(stateFileError(test.err), ErrStateRefused); got != test.refused {
			t.Errorf("%s: refused %v, want %v", test.name, got, test.refused)
		}
	}
}

// Claude Code and Copilot refuse a conversation file their agent deleted, and
// a native directory their agent replaced with a link, as Codex does.
func TestClaudeAndCopilotRefuseStatesTheirAgentCanLeave(t *testing.T) {
	t.Parallel()
	const thread = "01a126e8-288d-7350-9b52-627e16048343"
	home, workspace := t.TempDir(), t.TempDir()
	if _, err := CaptureClaudeState(home, workspace, thread, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("absent Claude transcript: %v", err)
	}
	if _, err := CaptureCopilotState(home, thread, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("absent Copilot journal: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "projects")); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureClaudeState(home, workspace, thread, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("Claude native directory replaced with a link: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "session-state")); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureCopilotState(home, thread, 1<<20); !errors.Is(err, ErrStateRefused) {
		t.Fatalf("Copilot native directory replaced with a link: %v", err)
	}
}
