package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// ErrStateRefused marks a capture failure that is a property of the stopped
// native state itself: its conversation file holds no completed turn, names
// another conversation, exceeds the checkpoint bound, is not a plain file, is
// absent or unreadable by its own permissions, or sits in a native directory
// that fails the safety checks (stateFileError). The writer is fenced, so
// capturing the same state again fails the same way. Every other capture
// failure, such as an I/O error from the volume, is environmental and may
// succeed when repeated (ADR 0062).
var ErrStateRefused = errors.New("stopped native state refused")

// stateFileError classifies a failure to reach or read a file or directory
// in the stopped native home. The agent owns that home and can delete its
// conversation file, remove its permissions or replace it with another kind
// of file before it exits; the writer is fenced, so such a state is the same
// on every capture and is refused. One error value can be either, so its kind
// decides: a missing file, a permission refused on it, or a path through
// something that is not a directory is the state; anything else, such as an
// I/O error from the volume, is the environment and may pass when repeated.
func stateFileError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.ELOOP) {
		return refusedState(err)
	}
	return err
}

// refusedState marks err as a refusal of the stopped state.
func refusedState(err error) error {
	if err == nil || errors.Is(err, ErrStateRefused) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStateRefused, err)
}
