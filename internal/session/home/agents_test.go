package home

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

func agentsRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(testutil.PrivateDir(t), "agents")
	for _, dir := range []string{root, filepath.Join(root, "shared"), filepath.Join(root, "codex"), filepath.Join(root, "claude")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUserInstructionsComposeSharedThenHarnessFiles(t *testing.T) {
	root := agentsRoot(t)
	for name, text := range map[string]string{"shared/AGENTS.md": "Shared rule.\n", "codex/AGENTS.md": "Codex rule.", "claude/CLAUDE.md": "Claude rule."} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	codex, err := UserInstructions(root, harness.NativeCodex)
	if err != nil || codex != "Shared rule.\n\nCodex rule." {
		t.Fatalf("codex %q %v", codex, err)
	}
	claude, err := UserInstructions(root, harness.NativeClaude)
	if err != nil || claude != "Shared rule.\n\nClaude rule." {
		t.Fatalf("claude %q %v", claude, err)
	}
	if empty, err := UserInstructions("", harness.NativeCodex); err != nil || empty != "" {
		t.Fatal("no agents directory must add nothing")
	}
}

func TestUserInstructionsRejectLinksOversizeAndBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	root := agentsRoot(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "codex", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if text, err := UserInstructions(root, harness.NativeCodex); err == nil || strings.Contains(text, "host secret") {
		t.Fatalf("followed a symbolic link: %q %v", text, err)
	}
	if err := os.Remove(filepath.Join(root, "codex", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"oversize": []byte(strings.Repeat("x", maxAgentFile+1)), "binary": {0xff, 0xfe, 0}} {
		if err := os.WriteFile(filepath.Join(root, "codex", "AGENTS.md"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := UserInstructions(root, harness.NativeCodex); err == nil {
			t.Fatalf("%s instructions accepted", name)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "codex", "AGENTS.md"), []byte("ok"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "codex", "AGENTS.md"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := UserInstructions(root, harness.NativeCodex); err == nil {
		t.Fatal("world-writable instructions accepted")
	}
}

func TestUnsupportedAgentEntriesAreReported(t *testing.T) {
	root := agentsRoot(t)
	for _, name := range []string{"shared/AGENTS.md", "codex/config.toml", "claude/settings.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "codex", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	ignored, err := UnsupportedAgentEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"codex/config.toml": true, "claude/settings.json": true, "codex/skills": true}
	if len(ignored) != len(want) {
		t.Fatalf("ignored %v", ignored)
	}
	for _, name := range ignored {
		if !want[name] {
			t.Fatalf("unexpected %s in %v", name, ignored)
		}
	}
}
