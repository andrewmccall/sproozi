//go:build !windows

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func assertRestoredMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("mode: %o, want %o", info.Mode().Perm(), want)
	}
}

func TestRestoreClearsInheritedSetgidOnlyOnOwnedStaging(t *testing.T) {
	source := newFixture(t)
	data, err := Capture(t.Context(), source, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	parentMode := os.FileMode(0700) | os.ModeSetgid | os.ModeSticky
	if err = os.Chmod(parent, parentMode); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "restored")
	if err = Restore(t.Context(), data, target, testLimit); err != nil {
		t.Fatalf("restore under setgid parent: %v", err)
	}
	info, err := os.Stat(parent)
	if err != nil || info.Mode()&(os.ModeSetgid|os.ModeSticky) != parentMode&(os.ModeSetgid|os.ModeSticky) {
		t.Fatalf("restore changed parent volume permissions: %v", err)
	}
	err = filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		if info.Mode()&(os.ModeSetgid|os.ModeSetuid|os.ModeSticky) != 0 {
			t.Errorf("owned restored path retained inherited special modes: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "note.txt", []byte("unstaged\n"))
	if got := fixtureGit(t, target, "show", ":note.txt"); got != fixtureStagedNote {
		t.Fatalf("setgid staging lost index: %q", got)
	}
}

func TestExcludedIgnoredLayoutsDoNotBlockCheckpoint(t *testing.T) {
	root := newFixture(t)
	writeFixture(t, root, ".gitignore", []byte("ignored.dat\nignored-layout/\n"), 0644)
	ignored := filepath.Join(root, "ignored-layout")
	if err := os.Mkdir(ignored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(ignored, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(ignored, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(ignored, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, nested, "init", "--quiet", "--template=")
	writeFixture(t, nested, "note", []byte("ignored nested repository"), 0600)
	data, err := Capture(context.Background(), root, nil, testLimit)
	if err != nil {
		t.Fatalf("excluded ignored state blocked capture: %v", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err = Restore(context.Background(), data, target, testLimit); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(target, "ignored-layout")); !os.IsNotExist(err) {
		t.Fatalf("excluded ignored layout transferred: %v", err)
	}
	for _, selected := range []string{"ignored-layout/link", "ignored-layout/pipe", "ignored-layout/nested/note"} {
		if _, err = Capture(context.Background(), root, []string{selected}, testLimit); err == nil {
			t.Fatalf("selected unsupported layout accepted: %s", selected)
		}
	}
}
