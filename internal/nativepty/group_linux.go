//go:build linux

package nativepty

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// A zombie has stopped executing, even when the namespace's PID 1 has not
// reaped it yet. Signal-zero alone would mistake those records for writers.
func groupStopped(group int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return false, errors.New("invalid process status")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 3 {
			return false, errors.New("incomplete process status")
		}
		processGroup, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if processGroup == group && fields[0] != "Z" && fields[0] != "X" {
			return false, nil
		}
	}
	return true, nil
}

func groupGone(err error) bool { return errors.Is(err, unix.ESRCH) }
