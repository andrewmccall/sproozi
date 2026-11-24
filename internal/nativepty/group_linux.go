//go:build linux

package nativepty

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// A zombie has stopped executing, even when the namespace's PID 1 has not
// reaped it yet. Signal-zero alone would mistake those records for writers.
// scanGroup decides which unreadable entries are processes that exited.
func groupStopped(group int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	pids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			pids = append(pids, entry.Name())
		}
	}
	return scanGroup(group, pids, func(pid string) ([]byte, error) {
		return os.ReadFile(filepath.Join("/proc", pid, "stat"))
	})
}

func groupGone(err error) bool { return errors.Is(err, unix.ESRCH) }
