package harness

import (
	"errors"
	"fmt"
)

// ErrStateRefused marks a capture failure that is a property of the stopped
// native state itself: its conversation file holds no completed turn, names
// another conversation, exceeds the checkpoint bound or is not a plain file.
// The writer is fenced, so capturing the same state again fails the same way.
// Every other capture failure, such as a file that cannot be read, is
// environmental and may succeed when repeated (ADR 0062).
var ErrStateRefused = errors.New("stopped native state refused")

// refusedState marks err as a refusal of the stopped state.
func refusedState(err error) error {
	if err == nil || errors.Is(err, ErrStateRefused) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStateRefused, err)
}
