package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// exitCoded is a docker exec whose command ended with that code, as
// *exec.ExitError reports it.
type exitCoded int

func (e exitCoded) Error() string {
	return fmt.Sprintf("docker fixed operation failed: exit status %d", int(e))
}
func (e exitCoded) ExitCode() int { return int(e) }

// unfinishedCodexCapture runs the helper's capture of a stopped Codex home
// whose turn started and never completed, as the helper does in its container.
func unfinishedCodexCapture(t *testing.T) func(context.Context) ([]byte, error) {
	t.Helper()
	root := testutil.PrivateDir(t)
	repo, home := filepath.Join(root, "repository"), filepath.Join(root, "codex")
	for _, dir := range []string{repo, home} {
		if err := testutil.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "fact.txt"}, {"commit", "-q", "-m", "fixture"}} {
		if args[0] == "add" {
			if err := testutil.WriteFile(filepath.Join(repo, "fact.txt"), []byte("fixture fact\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v %s", err, out)
		}
	}
	dir := filepath.Join(home, "sessions", "2026", "10", "10")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rows := `{"type":"session_meta","payload":{"id":"` + fixtureThread + `","cli_version":"0.161.0","cwd":"/workspace"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"unfinished"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-10T18-41-51-"+fixtureThread+".jsonl"), []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context) ([]byte, error) {
		data, _, err := checkpoint.CaptureStopped(ctx, harness.NativeCodex, home, repo, fixtureThread, nil, 8<<20)
		return data, err
	}
}

// What the helper's exit code makes the Docker stop of an exited visit keep:
// a refused state, and code 1 from a helper older than the codes, keep what
// the visit started from, once and logged; a capture the helper could not
// carry out fails the stop, and its retry captures.
func TestDockerStopReadsTheCaptureHelpersExitCode(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		code     int
		fallBack bool
	}{
		{"state refused", checkpoint.ExitStateRefused, true},
		{"older helper", 1, true},
		{"could not be carried out", checkpoint.ExitCaptureFailed, false},
		{"killed helper", 137, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, f, l, i, data := exitedDockerVisit(t)
			var log bytes.Buffer
			p.SetLog(&log)
			f.captureExit = test.code
			stopped, err := p.Stop(t.Context(), l, i)
			if !test.fallBack {
				if err == nil {
					t.Fatal("a capture the helper could not carry out kept what the visit started from")
				}
				f.captureExit = 0
				stopped, err = p.Stop(t.Context(), l, i)
				if err != nil || !bytes.Equal(stopped.Checkpoint, data) || log.Len() != 0 {
					t.Fatalf("retry: digest %q, log %q: %v", stopped.CheckpointDigest, log.String(), err)
				}
				return
			}
			if err != nil || stopped.CheckpointDigest != "" || stopped.Exit == nil ||
				!strings.Contains(log.String(), "Discarded the captured state of stopped visit "+l.AllocationID) {
				t.Fatalf("fallback: digest %q, log %q: %v", stopped.CheckpointDigest, log.String(), err)
			}
			f.captureExit = 0
			if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != "" {
				t.Fatalf("stop replay: digest %q: %v", again.CheckpointDigest, err)
			}
		})
	}
}

// A stop that finds the agent container already stopped by its own quiesce,
// as when the stop that sent it lost the reply and ended before it saw the
// container stop, is a graceful stop of a harness whose state proves
// completion: the supervisor exited 0 and wrote no exit report. No exit is
// recorded, and a capture that fails is retried and keeps the conversation,
// as on Kubernetes.
func TestDockerStopAfterALostQuiesceReplyIsGraceful(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	p := openFixture(t, testutil.PrivateDir(t), f, &fakeAuthority{engine: f})
	l := fixtureLaunch(t, data)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	c := f.containers[i.ResourceUID]
	c.State.Running, c.State.Pid, c.State.Status = false, 0, exitedStatus
	f.captureExit = checkpoint.ExitCaptureFailed
	if _, err = p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("a failed capture after a lost reply was not retried")
	}
	f.captureExit = 0
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Exit != nil || !bytes.Equal(stopped.Checkpoint, data) {
		t.Fatalf("lost reply: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
	}
}

// The case ADR 0062 exists for: a Codex agent that exited by itself with its
// turn unfinished. The helper refuses that state, and the stop finishes,
// keeping what the visit started from and logging the discard.
func TestExitedDockerCodexVisitWithAnUnfinishedTurnFinishesItsStop(t *testing.T) {
	t.Parallel()
	p, f, l, i, _ := exitedDockerVisit(t)
	var log bytes.Buffer
	p.SetLog(&log)
	f.captureFrom = unfinishedCodexCapture(t)
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.CheckpointDigest != "" || stopped.Exit == nil || stopped.Receipt == nil {
		t.Fatalf("unfinished turn: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
	}
	if !strings.Contains(log.String(), "kept the checkpoint it started from") {
		t.Fatalf("the discard was not logged: %q", log.String())
	}
}
