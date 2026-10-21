//go:build darwin

package nativepty

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Darwin refuses to signal a group whose remaining members are zombies with
// EPERM rather than ESRCH. A zombie has stopped executing, so both mean the
// group no longer runs; the supervisor's children share its user, so a real
// permission refusal does not arise here.
func groupStopped(group int) (bool, error) {
	err := unix.Kill(-group, 0)
	if groupGone(err) {
		return true, nil
	}
	return false, err
}

func groupGone(err error) bool {
	return errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EPERM)
}
