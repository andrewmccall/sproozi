//go:build darwin

package nativepty

import (
	"errors"

	"golang.org/x/sys/unix"
)

func groupStopped(group int) (bool, error) {
	err := unix.Kill(-group, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	return false, err
}
