package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// maxSignInBytes bounds a harness's login file.
const maxSignInBytes = 1 << 20

// signInPaths are the harness's login file in the runtime's sign-in
// directory and in its native home (ADR 0047).
func signInPaths(c config) (stored, native string, ok bool) {
	h, err := harness.Managed(c.Harness)
	if c.SignInDirectory == "" || err != nil || h.SignInFile == "" {
		return "", "", false
	}
	return filepath.Join(c.SignInDirectory, h.SignInFile),
		filepath.Join("/workspace/session", h.NativeDirectory, h.SignInFile), true
}

// restoreSignIn copies the harness's own login file into its native home
// before launch. With none stored yet, the harness asks the person to sign in.
func restoreSignIn(c config) error {
	stored, native, ok := signInPaths(c)
	if !ok {
		return nil
	}
	return copyPrivate(stored, native)
}

// saveSignIn writes the harness's refreshed login file back after it exits,
// when the sign-in directory is writable. A read-only Secret keeps its token.
func saveSignIn(c config) error {
	stored, native, ok := signInPaths(c)
	if !ok {
		return nil
	}
	err := copyPrivate(native, stored)
	if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return err
}

// copyPrivate replaces target with source's bytes, mode 0600, through a
// temporary file in target's directory. A missing source, a link or an
// unchanged target is left alone.
func copyPrivate(source, target string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSignInBytes {
		return fmt.Errorf("sign-in file %s is not a bounded regular file", source)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if current, err := os.ReadFile(target); err == nil && bytes.Equal(current, data) {
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".sign-in-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), target)
}
