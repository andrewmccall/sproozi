// Package testutil creates host fixtures with the same privacy as production state.
package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

const windowsHost = "windows"

// PrivateDir creates an owner-private directory even under an elevated Windows
// test account, whose default object owner may otherwise be Administrators.
func PrivateDir(t testing.TB) string {
	t.Helper()
	dir, err := privatefs.TempDir("", "s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func MkdirAll(path string, mode fs.FileMode) error {
	if runtime.GOOS == windowsHost && mode.Perm()&0077 == 0 {
		return privatefs.EnsureDir(path)
	}
	return os.MkdirAll(path, mode)
}

func Mkdir(path string, mode fs.FileMode) error {
	if runtime.GOOS == windowsHost && mode.Perm()&0077 == 0 {
		if _, err := os.Lstat(path); err == nil {
			return &os.PathError{Op: "mkdir", Path: path, Err: fs.ErrExist}
		} else if !os.IsNotExist(err) {
			return err
		}
		return privatefs.EnsureDir(path)
	}
	return os.Mkdir(path, mode)
}

func WriteFile(path string, data []byte, mode fs.FileMode) error {
	if runtime.GOOS == windowsHost && mode.Perm()&0077 == 0 {
		return privatefs.Replace(filepath.Dir(path), filepath.Base(path), data)
	}
	return os.WriteFile(path, data, mode)
}
