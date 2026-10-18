//go:build !windows

package privatefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRejectsBroadPermissionsWithoutRepair(t *testing.T) {
	dir := testDirectory(t)
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, false); err == nil {
		t.Fatal("accepted readable credential")
	}
	if _, err := ReadFile(path, 64); err == nil {
		t.Fatal("read readable credential")
	}
	if f, err := Lock(path); err == nil {
		_ = UnlockClose(f)
		t.Fatal("locked readable credential")
	}
	if err := Replace(dir, "credential", []byte("changed")); err == nil {
		t.Fatal("replaced unsafe credential")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("unexpected permission repair: %v %v", info, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err == nil {
		t.Fatal("accepted public directory")
	}
	if f, err := Lock(filepath.Join(dir, "owner.lock")); err == nil {
		_ = UnlockClose(f)
		t.Fatal("created lock in public directory")
	}
}
