package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// writeStateFile publishes a restored conversation file: it creates name,
// never replacing an existing file, writes and syncs data, and removes the
// file again if writing, syncing or closing it fails.
func writeStateFile(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return errors.Join(writeErr, closeErr)
	}
	return nil
}

// stateDirectories checks home and each directory of relative under it, and
// with create makes the missing ones. A directory it cannot reach is
// classified by stateFileError; a link or a non-directory is refused with
// unsafe.
func stateDirectories(home, relative string, create bool, unsafe string) error {
	dir := home
	for i, step := range append([]string{""}, strings.Split(filepath.ToSlash(relative), "/")...) {
		if i > 0 {
			if step == "" || step == "." {
				continue
			}
			dir = filepath.Join(dir, step)
			if create {
				if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
					return err
				}
			}
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return stateFileError(err)
		}
		// An Lstat of a link reports no directory, so the link test adds
		// nothing on Unix. It stays as the guard every native walk writes
		// (codex_state.go too), stating the intent, and costs no call.
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return refusedState(errors.New(unsafe))
		}
	}
	return nil
}
