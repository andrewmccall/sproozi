package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const fixtureThread = "01a126e8-288d-7350-9b52-627e16048343"
const checkpointLimit = int64(8 << 20)

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid"}
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git: %v: %s", err, data)
	}
	return strings.TrimSpace(string(data))
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func source(t *testing.T) (string, string, []byte) {
	t.Helper()
	repository, home := t.TempDir(), t.TempDir()
	git(t, repository, "init", "--quiet", "--template=", "-b", "main")
	write(t, filepath.Join(repository, "note"), "committed\n")
	write(t, filepath.Join(repository, ".gitignore"), "ignored\n")
	git(t, repository, "add", ".")
	git(t, repository, "commit", "--quiet", "-m", "Fixture")
	write(t, filepath.Join(repository, "note"), "staged\n")
	git(t, repository, "add", "note")
	write(t, filepath.Join(repository, "note"), "working\n")
	write(t, filepath.Join(repository, "untracked"), "unfinished\n")
	write(t, filepath.Join(repository, "ignored"), "explicit only\n")
	state := harness.CodexState{
		ThreadID: fixtureThread, Path: "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + fixtureThread + ".jsonl",
		Rollout: []byte(`{"type":"session_meta","payload":{"id":"01a126e8-288d-7350-9b52-627e16048343","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture-turn"}}
{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"opaque remembered fact"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture-turn"}}
`),
	}
	if err := harness.RestoreCodexState(home, state, checkpointLimit); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "auth.json"), "source authority")
	return repository, home, state.Rollout
}

func TestNativeAndUnfinishedWorkspaceRestoreTogether(t *testing.T) {
	for _, includeIgnored := range []bool{false, true} {
		t.Run(stringBool(includeIgnored), func(t *testing.T) {
			repository, home, native := source(t)
			var selection []string
			if includeIgnored {
				selection = []string{"ignored"}
			}
			data, meta, err := CaptureStopped(context.Background(), harness.NativeCodex, home, repository, fixtureThread, selection, checkpointLimit)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "inert")
			restored, err := Restore(context.Background(), data, target, checkpointLimit)
			if err != nil || restored != meta || meta.ThreadID != fixtureThread || meta.TurnID != "fixture-turn" {
				t.Fatalf("checkpoint restore metadata: %+v, %v", restored, err)
			}
			repo := filepath.Join(target, "workspace")
			if got := git(t, repo, "show", ":note"); got != "staged" {
				t.Fatalf("lost staged state: %q", got)
			}
			for name, want := range map[string]string{"note": "working\n", "untracked": "unfinished\n"} {
				got, err := os.ReadFile(filepath.Join(repo, name))
				if err != nil || string(got) != want {
					t.Fatalf("lost %s working bytes: %q, %v", name, got, err)
				}
			}
			_, err = os.Stat(filepath.Join(repo, "ignored"))
			if includeIgnored && err != nil || !includeIgnored && !os.IsNotExist(err) {
				t.Fatalf("ignored selection not preserved: %v", err)
			}
			captured, err := harness.CaptureCodexState(filepath.Join(target, "codex"), fixtureThread, checkpointLimit)
			if err != nil || string(captured.Rollout) != string(native) {
				t.Fatalf("native conversation changed: %v", err)
			}
			if _, err = os.Stat(filepath.Join(target, "codex", "auth.json")); !os.IsNotExist(err) {
				t.Fatalf("native authority copied: %v", err)
			}
			if _, err = Restore(context.Background(), data, target, checkpointLimit); err == nil {
				t.Fatal("replaced an existing placement")
			}
		})
	}
}

func stringBool(value bool) string {
	if value {
		return "explicit-ignored"
	}
	return "default-selection"
}

func TestNativeWorkspaceRestoreUnderSetgidVolumeCreatesPrivateDirectories(t *testing.T) {
	repository, home, _ := source(t)
	data, _, err := CaptureStopped(t.Context(), harness.NativeCodex, home, repository, fixtureThread, nil, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err = os.Chmod(parent, 0700|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "placement")
	if _, err = Restore(t.Context(), data, target, checkpointLimit); err != nil {
		t.Fatalf("checkpoint restore under setgid parent: %v", err)
	}
	for _, name := range []string{target, filepath.Join(target, "codex"), filepath.Join(target, "workspace")} {
		info, statErr := os.Stat(name)
		if statErr != nil || info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetgid|os.ModeSetuid|os.ModeSticky) != 0 {
			t.Fatalf("restored directory not private: %s %v", name, statErr)
		}
	}
	info, err := os.Stat(parent)
	if err != nil || info.Mode()&(os.ModeSetgid|os.ModeSticky) != os.ModeSetgid|os.ModeSticky {
		t.Fatalf("checkpoint changed parent volume permissions: %v", err)
	}
	if got := git(t, filepath.Join(target, "workspace"), "show", ":note"); got != "staged" {
		t.Fatalf("native checkpoint lost workspace index: %q", got)
	}
	if _, err = harness.CaptureCodexState(filepath.Join(target, "codex"), fixtureThread, checkpointLimit); err != nil {
		t.Fatalf("native checkpoint lost selected rollout: %v", err)
	}
}

func TestFailedComponentNeverPublishesPlacement(t *testing.T) {
	repository, home, _ := source(t)
	data, _, err := CaptureStopped(context.Background(), harness.NativeCodex, home, repository, fixtureThread, nil, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	var artifact nativeArtifact
	if err = json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	artifact.Workspace = []byte("corrupt Git checkpoint")
	corrupt, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if _, err = Restore(context.Background(), corrupt, filepath.Join(parent, "placement"), checkpointLimit); err == nil {
		t.Fatal("published a corrupt workspace")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed restore left continuity state: %v, %v", entries, err)
	}
	if _, err = Inspect(data, int64(len(data)-1)); err == nil {
		t.Fatal("accepted oversized checkpoint")
	}
}
