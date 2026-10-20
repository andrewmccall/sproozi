//go:build !windows

package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Check rejects symbolic links, wrong owners, non-ordinary objects and access
// by any other user or group.
func Check(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkInfo(path, info, directory)
}

func checkInfo(path string, info os.FileInfo, directory bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return unsafePath(path)
	}
	return nil
}

// EnsureDir creates missing private directories and validates an existing leaf.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return Check(path, true)
}
func mkdir(path string) error { return os.Mkdir(path, 0700) }

func openFile(path string, create bool) (*os.File, error) {
	flags := os.O_RDONLY
	if create {
		flags = os.O_RDWR | os.O_CREATE
	}
	return open(path, flags)
}
func createFile(path string) (*os.File, error) { return open(path, os.O_RDWR|os.O_CREATE|os.O_EXCL) }
func open(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if errors.Is(err, syscall.ELOOP) {
		// O_NOFOLLOW refuses a final symbolic link with ELOOP: a link is not an
		// ordinary private file.
		return nil, unsafePath(path)
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = checkInfo(path, info, false)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Lock opens a private lock file and retains a nonblocking exclusive process lock.
func Lock(path string) (*os.File, error) {
	if err := Check(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	f, err := openFile(path, true)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, lockedError{err}
		}
		return nil, err
	}
	return f, nil
}

// UnlockClose releases the process lock and closes its retained handle.
func UnlockClose(f *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}

func replaceFile(temp, target, dir string) error {
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
