package checkpoint

import (
	"errors"
	"fmt"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// The checkpoint helper's capture-stopped command ends with one of these codes
// when it fails, so the placement that runs it can tell a stopped state it
// refused from a capture it could not carry out (ADR 0062). They are a
// contract between the helper and the placements, which both import them.
// They differ from 1, which every failure of a helper older than the codes
// returns and every other command still returns, from a shell's 126 and 127,
// and from a signal's 128+n; they follow sysexits(3).
const (
	// ExitStateRefused (EX_DATAERR): the stopped state does not make a
	// checkpoint, a property of the fenced bytes that capturing again finds
	// the same: no completed turn, another conversation, a failed continuity
	// check, or a state over the checkpoint bound.
	ExitStateRefused = 65
	// ExitCaptureFailed (EX_IOERR): the capture could not be carried out,
	// such as a file that cannot be read or a Git failure. Repeating it may
	// succeed.
	ExitCaptureFailed = 74
)

// ErrStateRefused marks a refusal of the stopped state (ExitStateRefused).
var ErrStateRefused = harness.ErrStateRefused

// RefuseState marks err as a refusal of the stopped state; nil stays nil.
func RefuseState(err error) error {
	if err == nil || errors.Is(err, ErrStateRefused) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStateRefused, err)
}

// CaptureExitCode is the code capture-stopped ends with for err: 0, or which
// of the two failures it is.
func CaptureExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrStateRefused):
		return ExitStateRefused
	}
	return ExitCaptureFailed
}

// HelperExitCode is the exit code of a helper command that failed with err, as
// the exec reports it, and false when err carries none: a transport or exec
// failure, or a command that never ended. A Docker exec reports it through
// ExitCode, a Kubernetes exec through ExitStatus; a command a signal ended
// reports a negative code or 128+n.
func HelperExitCode(err error) (int, bool) {
	var docker interface{ ExitCode() int }
	if errors.As(err, &docker) {
		return docker.ExitCode(), true
	}
	var kube interface{ ExitStatus() int }
	if errors.As(err, &kube) {
		return kube.ExitStatus(), true
	}
	return 0, false
}
