package placement

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
)

// exitStatus is an exec whose command ended with that code, as the
// Kubernetes exec reports it (k8s.io/client-go/util/exec.CodeExitError).
type exitStatus int

func (e exitStatus) Error() string {
	return fmt.Sprintf("command terminated with exit code %d", int(e))
}
func (e exitStatus) ExitStatus() int { return int(e) }

// unfinishedCodexCapture runs the helper's capture of a stopped Codex home
// whose turn started and never completed, as the helper does in its container.
func unfinishedCodexCapture(t *testing.T) func(context.Context) ([]byte, error) {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "fact.txt"}, {"commit", "-q", "-m", "fixture"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(repo, "fact.txt"), []byte("fixture fact\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
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
		data, _, err := checkpoint.CaptureStopped(ctx, harness.NativeCodex, home, repo, fixtureThread, nil, fixtureMax)
		return data, err
	}
}

// What the helper's exit code makes the Kubernetes stop of an exited visit
// keep: a refused state keeps what the visit started from, once and logged.
// A capture the helper could not carry out, a killed helper, and code 1 from
// a helper older than the codes, fail the stop, and its retry captures.
func TestKubernetesStopReadsTheCaptureHelpersExitCode(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		code     int
		fallBack bool
	}{
		{"state refused", checkpoint.ExitStateRefused, true},
		{"could not be carried out", checkpoint.ExitCaptureFailed, false},
		{"older helper", 1, false},
		{"killed helper", 137, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setup(t)
			var log bytes.Buffer
			p.config.Log = &log
			i := activeVisit(t, p, l, data)
			work, meta := cachedFixtureArtifact(t, harness.NativeCodex, "work of this visit\n")
			e.data, e.captureExit = work, test.code
			endNative(t, p, kube, l, 1)
			e.logs = exitedReport(1, "")
			stopped, err := p.Stop(t.Context(), l, i)
			if !test.fallBack {
				if err == nil {
					t.Fatalf("exit code %d kept what the visit started from", test.code)
				}
				e.captureExit = 0
				stopped, err = p.Stop(t.Context(), l, i)
				if err != nil || stopped.CheckpointDigest != meta.SHA256 || log.Len() != 0 {
					t.Fatalf("retry: digest %q, log %q: %v", stopped.CheckpointDigest, log.String(), err)
				}
				return
			}
			if err != nil || stopped.CheckpointDigest != l.CheckpointDigest || stopped.Exit == nil || e.captures != 1 ||
				!strings.Contains(log.String(), "Discarded the captured state of stopped visit "+l.AllocationID) {
				t.Fatalf("fallback: digest %q, %d captures, log %q: %v", stopped.CheckpointDigest, e.captures, log.String(), err)
			}
			e.captureExit = 0
			if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != l.CheckpointDigest || e.captures != 1 {
				t.Fatalf("stop replay: digest %q, %d captures: %v", again.CheckpointDigest, e.captures, err)
			}
		})
	}
}

// The case ADR 0062 exists for: a Codex agent that exited by itself with its
// turn unfinished. The helper refuses that state, and the stop finishes,
// keeping the checkpoint the visit started from and logging the discard.
func TestExitedKubernetesCodexVisitWithAnUnfinishedTurnFinishesItsStop(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	var log bytes.Buffer
	p.config.Log = &log
	i := activeVisit(t, p, l, data)
	e.captureFrom = unfinishedCodexCapture(t)
	endNative(t, p, kube, l, 0)
	e.logs = exitedReport(0, "")
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.CheckpointDigest != l.CheckpointDigest || stopped.Exit == nil || stopped.Receipt == nil {
		t.Fatalf("unfinished turn: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
	}
	if !strings.Contains(log.String(), "kept the checkpoint it started from") {
		t.Fatalf("the discard was not logged: %q", log.String())
	}
}
