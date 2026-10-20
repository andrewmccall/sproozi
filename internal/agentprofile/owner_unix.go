//go:build !windows

package agentprofile

import (
	"fmt"
	"os"
	"syscall"
)

// ownedEntry requires a non-link entry of the expected type owned by the
// current user. Profile files may be readable by others; they hold
// guidance, not secrets.
func ownedEntry(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() ||
		stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("profile %s must be an owned, non-link entry not writable by others", path)
	}
	return nil
}
