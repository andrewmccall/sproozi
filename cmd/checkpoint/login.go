package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// loginDirectory is where the placement mounts a harness's login volume for
// initLogin.
const loginDirectory = "/login"

// nativeUID and nativeGID are the agent container's user.
const nativeUID, nativeGID = 1000, 1000

// initLogin prepares a harness's persistent sign-in file for the unprivileged
// agent (ADR 0047): an empty 0600 file owned by uid:gid. The directory stays
// with this process's user, mode 0711, so later runs can still inspect the
// file without CAP_DAC_OVERRIDE, and the agent, which mounts the directory as
// its login store, can reach that file but not list or add entries
// (ADR 0049). It never
// reads or rewrites an existing sign-in, and refuses one that isn't a regular
// file with that owner and mode.
func initLogin(path string, uid, gid int) error {
	if filepath.Dir(path) != loginDirectory || filepath.Base(path) == "." {
		return fmt.Errorf("login file must be directly inside %s", loginDirectory)
	}
	return prepareLogin(path, uid, gid)
}

func prepareLogin(path string, uid, gid int) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("login directory is unavailable")
	}
	if owner(info) != os.Getuid() {
		return errors.New("login directory owner changed")
	}
	if info.Mode().Perm() != 0o711 {
		if err = os.Chmod(dir, 0o711); err != nil {
			return err
		}
	}
	info, err = os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		if uid != os.Getuid() {
			return os.Chown(path, uid, gid)
		}
		return nil
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return errors.New("login file is not a regular file")
	case owner(info) != uid || info.Mode().Perm() != 0o600:
		return errors.New("login file owner or mode changed")
	}
	return nil
}

func owner(info fs.FileInfo) int {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid)
	}
	return -1
}
