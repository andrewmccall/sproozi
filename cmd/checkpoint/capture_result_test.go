package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	resultThread = "01a126e8-288d-7350-9b52-627e16048343"
	otherThread  = "11111111-2222-4333-8444-555555555555"
)

// stoppedCodex is a stopped Codex home and Git workspace whose rollout holds
// rows, at the path named for thread, and a restored helper receipt.
func stoppedCodex(t *testing.T, thread, rows string) (home, repository string) {
	t.Helper()
	privateHelper(t)
	restored := restoreReceipt{Stage: stageRestored, Metadata: checkpoint.Metadata{Harness: harness.NativeCodex}}
	if err := writeReceipt(restored); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repository, home = t.TempDir(), t.TempDir()
	const gitAdd = "add"
	steps := [][]string{{"init", "-q", "-b", "main"}, {gitAdd, "README.md"}, {"commit", "-q", "-m", "fixture"}}
	for _, args := range steps {
		if args[0] == gitAdd {
			if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = repository
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	dir := filepath.Join(home, "sessions", "2026", "10", "10")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "rollout-2026-10-10T18-41-51-"+thread+".jsonl")
	if err := os.WriteFile(name, []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	return home, repository
}

const (
	sessionMeta = `{"type":"session_meta","payload":{"id":"` + resultThread +
		`","cli_version":"0.161.0","cwd":"/workspace"}}` + "\n"
	turnStarted  = `{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture-turn"}}` + "\n"
	turnComplete = `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture-turn"}}` + "\n"
)

// captureCode runs the helper's capture-stopped command as main does and
// returns the exit code it would end with.
func captureCode(t *testing.T, home, repository, thread string, limit int64) int {
	t.Helper()
	err := run([]string{captureStopped, "--harness", string(harness.NativeCodex), "--home", home,
		"--workspace", repository, "--thread", thread, "--max-bytes", strconv.FormatInt(limit, 10)}, nil, io.Discard)
	return exitCode([]string{captureStopped}, err)
}

// capture-stopped ends with a code that says why it failed: a stopped state
// it refuses is the same on every capture, while a capture it could not
// carry out may succeed when repeated.
func TestCaptureStoppedSaysWhyItFailed(t *testing.T) {
	completed := sessionMeta + turnStarted + turnComplete
	for _, test := range []struct {
		name   string
		thread string
		rows   string
		limit  int64
		setup  func(t *testing.T, home string)
		want   int
	}{
		{"completed turn", resultThread, completed, testLimit, nil, 0},
		{"turn cut mid-way", resultThread, sessionMeta + turnStarted, testLimit, nil, checkpoint.ExitStateRefused},
		{"rollout of another thread", otherThread, completed, testLimit, nil, checkpoint.ExitStateRefused},
		{"state over the checkpoint bound", resultThread, completed, 64, nil, checkpoint.ExitStateRefused},
		{"unreadable rollout", resultThread, completed, testLimit, func(t *testing.T, home string) {
			t.Helper()
			name := filepath.Join(home, "sessions", "2026", "10", "10", "rollout-2026-10-10T18-41-51-"+resultThread+".jsonl")
			if err := os.Chmod(name, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(name, 0600) })
			if os.Geteuid() == 0 {
				t.Skip("root reads a file with no permissions")
			}
		}, checkpoint.ExitCaptureFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, repository := stoppedCodex(t, test.thread, test.rows)
			if test.setup != nil {
				test.setup(t, home)
			}
			if got := captureCode(t, home, repository, test.thread, test.limit); got != test.want {
				t.Fatalf("exit code %d, want %d", got, test.want)
			}
		})
	}
}

// Only capture-stopped classifies its failures; every other command keeps
// exit code 1, which is also what a helper older than the codes returns.
func TestOtherCommandsKeepExitCodeOne(t *testing.T) {
	refused := checkpoint.RefuseState(errors.New("refused"))
	if got := exitCode([]string{"prepare"}, refused); got != 1 {
		t.Fatalf("prepare exit code %d", got)
	}
	if got := exitCode([]string{captureStopped}, nil); got != 0 {
		t.Fatalf("success exit code %d", got)
	}
}
