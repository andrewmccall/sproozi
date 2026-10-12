package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const fixtureStagedNote = "staged"

const testLimit = int64(8 << 20)

func fixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func writeFixture(t *testing.T, root, name string, b []byte, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}
func newFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	fixtureGit(t, root, "init", "--quiet", "--template=", "-b", "main")
	writeFixture(t, root, ".gitignore", []byte("ignored.dat\n"), 0644)
	writeFixture(t, root, "note.txt", []byte("original\n"), 0644)
	writeFixture(t, root, "binary.dat", []byte{0, 1, 0, 1}, 0644)
	writeFixture(t, root, "staged-delete.txt", []byte("delete in index\n"), 0644)
	writeFixture(t, root, "work-delete.txt", []byte("delete in worktree\n"), 0644)
	fixtureGit(t, root, "add", ".")
	fixtureGit(t, root, "commit", "--quiet", "-m", "Initial fixture")
	fixtureGit(t, root, "branch", "other")
	fixtureGit(t, root, "tag", "v1")
	writeFixture(t, root, "note.txt", []byte("staged\n"), 0644)
	writeFixture(t, root, "binary.dat", []byte{0, 2, 0, 2}, 0644)
	writeFixture(t, root, "new.txt", []byte("staged new\n"), 0644)
	fixtureGit(t, root, "add", "note.txt", "binary.dat", "new.txt")
	fixtureGit(t, root, "rm", "--quiet", "staged-delete.txt")
	writeFixture(t, root, "note.txt", []byte("unstaged\n"), 0644)
	writeFixture(t, root, "binary.dat", []byte{0, 3, 0, 3}, 0644)
	writeFixture(t, root, "new.txt", []byte("unstaged new\n"), 0644)
	if err := os.Remove(filepath.Join(root, "work-delete.txt")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "loose/runner", []byte{0, 9, 8, 7}, 0755)
	writeFixture(t, root, "ignored.dat", []byte("private opt-in\n"), 0755)
	return root
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

func TestRestoreIncludesTheDestinationDefaultBranch(t *testing.T) {
	probe := t.TempDir()
	fixtureGit(t, probe, "init", "--quiet", "--template=")
	branch := fixtureGit(t, probe, "symbolic-ref", "--short", "HEAD")
	source := newFixture(t)
	fixtureGit(t, source, "branch", "-m", branch)
	data, err := Capture(t.Context(), source, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err = Restore(t.Context(), data, target, testLimit); err != nil {
		t.Fatal(err)
	}
	if got := fixtureGit(t, target, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Fatalf("restored branch=%q want=%q", got, branch)
	}
	if got := fixtureGit(t, target, "show", ":note.txt"); got != fixtureStagedNote {
		t.Fatalf("restored index=%q want staged", got)
	}
	content, err := os.ReadFile(filepath.Join(target, "note.txt"))
	if err != nil || string(content) != "unstaged\n" {
		t.Fatalf("restored working file=%q: %v", content, err)
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		result[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func assertFile(t *testing.T, root, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}
func TestRoundTripPreservesGitAndUnfinishedState(t *testing.T) {
	root := newFixture(t)
	head := fixtureGit(t, root, "rev-parse", "HEAD")
	// Commands configured by an untrusted repository must never run at capture.
	marker := filepath.Join(t.TempDir(), "unexpected-hook")
	fixtureGit(t, root, "config", "core.fsmonitor", "touch "+marker)
	fixtureGit(t, root, "config", "filter.hostile.clean", "touch "+marker)
	fixtureGit(t, root, "config", "filter.hostile.smudge", "touch "+marker)
	writeFixture(t, root, ".gitattributes", []byte("*.dat filter=hostile\n"), 0644)
	before := snapshot(t, root)
	data, err := Capture(context.Background(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, root)
	if len(after) != len(before) {
		t.Fatal("capture changed source files")
	}
	for p, want := range before {
		if after[p] != want {
			t.Errorf("capture changed source %s", p)
		}
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("repository command executed")
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err = Restore(context.Background(), data, dst, testLimit); err != nil {
		t.Fatal(err)
	}
	if got := fixtureGit(t, dst, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD changed: %s", got)
	}
	if got := fixtureGit(t, dst, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("branch: %s", got)
	}
	if got := fixtureGit(t, dst, "for-each-ref", "--format=%(refname)"); got != "refs/heads/main\nrefs/heads/other\nrefs/tags/v1" {
		t.Errorf("refs: %s", got)
	}
	if got := fixtureGit(t, dst, "show", ":note.txt"); got != fixtureStagedNote {
		t.Errorf("index: %s", got)
	}
	if got := fixtureGit(t, dst, "show", ":new.txt"); got != "staged new" {
		t.Errorf("new index: %s", got)
	}
	cmd := exec.Command("git", "show", ":binary.dat")
	cmd.Dir = dst
	binary, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary, []byte{0, 2, 0, 2}) {
		t.Errorf("binary index: %v", binary)
	}
	assertFile(t, dst, "note.txt", []byte("unstaged\n"))
	assertFile(t, dst, "binary.dat", []byte{0, 3, 0, 3})
	assertFile(t, dst, "new.txt", []byte("unstaged new\n"))
	assertFile(t, dst, "loose/runner", []byte{0, 9, 8, 7})
	for _, p := range []string{"staged-delete.txt", "work-delete.txt", "ignored.dat"} {
		if _, err = os.Stat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("unexpected restored %s", p)
		}
	}
	info, err := os.Stat(filepath.Join(dst, "loose/runner"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("executable mode: %o", info.Mode().Perm())
	}
	if got := fixtureGit(t, dst, "status", "--porcelain=v1", "--untracked-files=all"); got != "MM binary.dat\nAM new.txt\nMM note.txt\nD  staged-delete.txt\n D work-delete.txt\n?? .gitattributes\n?? loose/runner" {
		t.Errorf("Git status: %q", got)
	}
	config, err := os.ReadFile(filepath.Join(dst, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "hostile") {
		t.Fatal("source config exported")
	}
	fixtureGit(t, dst, "fsck", "--full", "--strict")
}
func TestIgnoredSelectionAndDetachedHead(t *testing.T) {
	root := newFixture(t)
	fixtureGit(t, root, "checkout", "--quiet", "--detach")
	data, err := Capture(context.Background(), root, []string{"ignored.dat"}, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err = Restore(context.Background(), data, dst, testLimit); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dst, "ignored.dat", []byte("private opt-in\n"))
	info, err := os.Stat(filepath.Join(dst, "ignored.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("ignored mode: %o", info.Mode().Perm())
	}
	if head, err := os.ReadFile(filepath.Join(dst, ".git", "HEAD")); err != nil || strings.HasPrefix(string(head), "ref:") {
		t.Fatalf("not detached: %s %v", head, err)
	}
	for _, p := range []string{"../escape", "/absolute", ".git/config", "note.txt", "loose"} {
		if _, err = Capture(context.Background(), root, []string{p}, testLimit); err == nil {
			t.Errorf("accepted ignored selection %q", p)
		}
	}
}
func TestInvalidArchiveNeverPublishesDestination(t *testing.T) {
	data, err := Capture(context.Background(), newFixture(t), nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	m, bundle, files, err := decode(data, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*manifest, *[]byte, *[][]byte){
		"traversal":    func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[0].Path = "../escape" },
		"absolute":     func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[0].Path = "/tmp/escape" },
		"control":      func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[0].Path = "nested/.git/config" },
		"digest":       func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[0].Digest = strings.Repeat("0", 64) },
		"special mode": func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[0].Mode = 04755 },
		"git payload": func(m *manifest, b *[]byte, _ *[][]byte) {
			*b = []byte("not a Git bundle")
			m.Bundle = descriptor("repository.bundle", *b, 0600)
		},
		"duplicate path":           func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[1].Path = m.Files[0].Path },
		"file directory collision": func(m *manifest, _ *[]byte, _ *[][]byte) { m.Files[1].Path = m.Files[0].Path + "/child" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			copyM := m
			copyM.Files = append([]entry(nil), m.Files...)
			copyBundle := append([]byte(nil), bundle...)
			copyFiles := append([][]byte(nil), files...)
			mutate(&copyM, &copyBundle, &copyFiles)
			bad, err := encode(copyM, copyBundle, copyFiles, testLimit)
			if err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			dst := filepath.Join(parent, "target")
			writeFixture(t, parent, "sentinel", []byte("unchanged"), 0600)
			if err = Restore(context.Background(), bad, dst, testLimit); err == nil {
				t.Fatal("accepted invalid archive")
			}
			assertFile(t, parent, "sentinel", []byte("unchanged"))
			if _, err = os.Lstat(dst); !os.IsNotExist(err) {
				t.Fatal("destination created on failure")
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatal("staging left behind")
			}
		})
	}
	dst := filepath.Join(t.TempDir(), "target")
	if err = Restore(context.Background(), data, dst, int64(len(data)-1)); err == nil {
		t.Fatal("accepted oversized archive")
	}
	if _, err = os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatal("oversize published target")
	}
	existing := t.TempDir()
	writeFixture(t, existing, "sentinel", []byte("original"), 0600)
	if err = Restore(context.Background(), data, existing, testLimit); err == nil {
		t.Fatal("overwrote existing destination")
	}
	assertFile(t, existing, "sentinel", []byte("original"))
}
func TestUnsupportedSourceFailsExplicitly(t *testing.T) {
	t.Run("unborn", func(t *testing.T) {
		root := t.TempDir()
		fixtureGit(t, root, "init", "--quiet", "--template=")
		if _, err := Capture(context.Background(), root, nil, testLimit); err == nil {
			t.Fatal("accepted unborn repository")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := newFixture(t)
		if err := os.Symlink("note.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := Capture(context.Background(), root, nil, testLimit); err == nil {
			t.Fatal("accepted symlink")
		}
	})
	t.Run("conflict", func(t *testing.T) {
		root := newFixture(t)
		blob := fixtureGit(t, root, "rev-parse", "HEAD:note.txt")
		cmd := exec.Command("git", "update-index", "--index-info")
		cmd.Dir = root
		cmd.Stdin = strings.NewReader("0 " + strings.Repeat("0", 40) + "\tnote.txt\n100644 " + blob + " 1\tnote.txt\n100644 " + blob + " 2\tnote.txt\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("conflict setup: %v %s", err, out)
		}
		if _, err := Capture(context.Background(), root, nil, testLimit); err == nil {
			t.Fatal("accepted conflict")
		}
	})
	t.Run("submodule index", func(t *testing.T) {
		root := newFixture(t)
		head := fixtureGit(t, root, "rev-parse", "HEAD")
		fixtureGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",module")
		if _, err := Capture(context.Background(), root, nil, testLimit); err == nil {
			t.Fatal("accepted submodule index")
		}
	})
	t.Run("byte limit", func(t *testing.T) {
		if _, err := Capture(context.Background(), newFixture(t), nil, 512); err == nil {
			t.Fatal("accepted undersized byte limit")
		}
	})
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
