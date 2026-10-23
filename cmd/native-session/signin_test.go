package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
)

func TestSignInPathsComeFromTheRegistry(t *testing.T) {
	for kind, want := range map[harness.NativeKind]string{
		harness.NativeClaude: "/workspace/session/claude/.credentials.json",
		harness.NativeCodex:  "/workspace/session/codex/auth.json",
	} {
		stored, native, ok := signInPaths(config{Harness: kind, SignInDirectory: api.SignInDirectory})
		if !ok || native != want || filepath.Dir(stored) != api.SignInDirectory {
			t.Fatalf("%s: stored=%s native=%s ok=%v", kind, stored, native, ok)
		}
	}
	if _, _, ok := signInPaths(config{Harness: harness.NativeClaude}); ok {
		t.Fatal("a launch without a sign-in directory restored a sign-in")
	}
}

func TestCopyPrivateRestoresAndSavesTheHarnessLogin(t *testing.T) {
	stored, home := t.TempDir(), t.TempDir()
	storedFile, homeFile := filepath.Join(stored, "auth.json"), filepath.Join(home, "auth.json")
	// Nothing stored yet: the harness asks the person to sign in.
	if err := copyPrivate(storedFile, homeFile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(homeFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a missing sign-in created a file")
	}
	if err := os.WriteFile(storedFile, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyPrivate(storedFile, homeFile); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(homeFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restored file: %v %v", info, err)
	}
	// The harness refreshes its sign-in; saving writes it back.
	if err := os.WriteFile(homeFile, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyPrivate(homeFile, storedFile); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(storedFile); string(data) != `{"v":2}` {
		t.Fatalf("saved %s", data)
	}
	link := filepath.Join(home, "link.json")
	if err := os.Symlink(storedFile, link); err != nil {
		t.Fatal(err)
	}
	if err := copyPrivate(link, filepath.Join(stored, "other.json")); err == nil {
		t.Fatal("a linked sign-in was copied")
	}
}

func TestCopyPrivateIntoAReadOnlyDirectoryIsAPermissionError(t *testing.T) {
	source, target := filepath.Join(t.TempDir(), "auth.json"), t.TempDir()
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	if err := copyPrivate(source, filepath.Join(target, "auth.json")); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want a permission error that saveSignIn skips", err)
	}
}
