//go:build !windows

package home

import (
	"fmt"
	"os"
	"syscall"
)

// ownedEntry requires a non-link entry of the expected type owned by the
// current user. Shared agent files may be readable by others; they hold
// preferences, not secrets.
func ownedEntry(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() ||
		stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("home: shared agent path %s must be an owned, non-link entry not writable by others", path)
	}
	return nil
}

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
