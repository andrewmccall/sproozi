//go:build !windows

package workspace

import (
	"errors"
	"os"
	"syscall"
)

func inspectFilesystemPath(_ string, info os.FileInfo) (uint64, bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, errors.New("workspace: filesystem identity unavailable")
	}
	return uint64(stat.Dev), info.Mode()&os.ModeSymlink != 0, nil
}

func capturedFileMode(info os.FileInfo, _ int64, _ bool) int64 {
	return int64(info.Mode().Perm())
}

func gitPlatformEnvironment() []string { return nil }
