//go:build windows

package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func assertRestoredMode(t *testing.T, path string, _ os.FileMode) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCaptureUsesIndexExecutableModeAndSafeNewFileMode(t *testing.T) {
	root := newFixture(t)
	writeFixture(t, root, "tracked-tool", []byte("tool"), 0644)
	fixtureGit(t, root, "add", "tracked-tool")
	fixtureGit(t, root, "update-index", "--chmod=+x", "tracked-tool")
	writeFixture(t, root, "untracked-tool", []byte("new"), 0755)

	data, err := Capture(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := decode(data, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]int64{}
	for _, entry := range m.Files {
		modes[entry.Path] = entry.Mode
	}
	if modes["tracked-tool"] != 0755 || modes["untracked-tool"] != 0644 {
		t.Fatalf("captured modes=%#v", modes)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err = Restore(t.Context(), data, target, testLimit); err != nil {
		t.Fatal(err)
	}
	if stage := fixtureGit(t, target, "ls-files", "--stage", "tracked-tool"); !strings.HasPrefix(stage, "100755 ") {
		t.Fatalf("tracked executable index mode lost: %q", stage)
	}
}

func TestWindowsCaptureRejectsJunction(t *testing.T) {
	root := newFixture(t)
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "selected"), []byte("selected"), 0600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "junction")
	cmd := exec.Command("cmd", "/c", "mklink", "/J", junction, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("junction setup unavailable: %v: %s", err, output)
	}
	if _, err := Capture(t.Context(), root, nil, testLimit); err == nil {
		t.Fatal("accepted workspace junction")
	}
}

func TestWindowsRestoreRejectsJunctionParent(t *testing.T) {
	data, err := Capture(t.Context(), newFixture(t), nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	realParent := t.TempDir()
	junction := filepath.Join(t.TempDir(), "junction")
	cmd := exec.Command("cmd", "/c", "mklink", "/J", junction, realParent)
	if output, junctionErr := cmd.CombinedOutput(); junctionErr != nil {
		t.Skipf("junction setup unavailable: %v: %s", junctionErr, output)
	}
	if err = Restore(t.Context(), data, filepath.Join(junction, "restored"), testLimit); err == nil {
		t.Fatal("restored through junction parent")
	}
	if entries, readErr := os.ReadDir(realParent); readErr != nil || len(entries) != 0 {
		t.Fatalf("junction target changed: entries=%v err=%v", entries, readErr)
	}
}
