package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const testLimit = int64(8 << 20)

// privateHelper points the helper's private directory and target at
// temporary directories.
func privateHelper(t *testing.T) {
	t.Helper()
	previousDirectory, previousTarget := helperDirectory, helperTarget
	helperDirectory, helperTarget = t.TempDir(), filepath.Join(t.TempDir(), "session")
	t.Cleanup(func() { helperDirectory, helperTarget = previousDirectory, previousTarget })
	// hold creates its directory private.
	if err := os.Chmod(helperDirectory, 0700); err != nil {
		t.Fatal(err)
	}
}

// workspaceArtifact captures a one-file Git workspace.
func workspaceArtifact(t *testing.T) []byte {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"add", "README.md"}, {"commit", "-q", "-m", "fixture"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	data, err := workspace.CaptureLocal(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func initialMetadata(data []byte) checkpoint.Metadata {
	return checkpoint.Metadata{Harness: harness.NativeCodex, SHA256: artifactDigest(data), Bytes: len(data)}
}

func receiptStage(t *testing.T) string {
	t.Helper()
	r, ok, err := readReceipt()
	if err != nil || !ok {
		t.Fatalf("receipt: %v %v", ok, err)
	}
	return r.Stage
}

func assertPrepared(t *testing.T, data []byte, output []byte) {
	t.Helper()
	var meta checkpoint.Metadata
	if err := json.Unmarshal(output, &meta); err != nil || meta != initialMetadata(data) {
		t.Fatalf("acknowledgement %s: %v", output, err)
	}
	if stage := receiptStage(t); stage != stageRestored {
		t.Fatalf("receipt stage %q after prepare", stage)
	}
	if _, err := os.Stat(filepath.Join(helperTarget, "workspace", "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := validateRestoredPair(harness.NativeCodex); err != nil {
		t.Fatal(err)
	}
}

// The restoring receipt is in place before the target is published, so a
// crash during publication leaves a receipt naming its stage.
func TestPrepareRecordsRestoringBeforePublishing(t *testing.T) {
	privateHelper(t)
	data := workspaceArtifact(t)
	var during string
	var output bytes.Buffer
	err := restoreOnce(initialMetadata(data), "conflict", &output, func() error {
		during = receiptStage(t)
		return publishWorkspace(t.Context(), "codex", data, testLimit)
	})
	if err != nil || during != stageRestoring {
		t.Fatalf("publish saw stage %q: %v", during, err)
	}
	assertPrepared(t, data, output.Bytes())
	entries, err := os.ReadDir(helperDirectory)
	if err != nil || len(entries) != 1 || entries[0].Name() != helperReceiptName {
		t.Fatalf("helper directory holds %v: %v", entries, err)
	}
}

// A crash after publication but before the acknowledgement used to leave the
// target present with no receipt, which every retry refused. The restoring
// receipt now lets a retry redo the restore.
func TestPrepareRedoesARestoreInterruptedAfterPublishing(t *testing.T) {
	privateHelper(t)
	data := workspaceArtifact(t)
	if err := writeReceipt(restoreReceipt{Stage: stageRestoring, Metadata: initialMetadata(data)}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(helperTarget, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helperTarget, "partial"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := finishAllowed(); err == nil {
		t.Fatal("finish allowed during an unfinished restore")
	}
	var output bytes.Buffer
	if err := prepareWorkspace(t.Context(), harness.NativeCodex, data, testLimit, &output); err != nil {
		t.Fatal(err)
	}
	assertPrepared(t, data, output.Bytes())
	if _, err := os.Lstat(filepath.Join(helperTarget, "partial")); !os.IsNotExist(err) {
		t.Fatalf("unfinished restore's work kept: %v", err)
	}
	if err := finishAllowed(); err != nil {
		t.Fatal(err)
	}
}

// A crash before publication leaves only the restoring receipt; a retry
// completes the restore.
func TestPrepareCompletesARestoreInterruptedBeforePublishing(t *testing.T) {
	privateHelper(t)
	data := workspaceArtifact(t)
	if err := writeReceipt(restoreReceipt{Stage: stageRestoring, Metadata: initialMetadata(data)}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := prepareWorkspace(t.Context(), harness.NativeCodex, data, testLimit, &output); err != nil {
		t.Fatal(err)
	}
	assertPrepared(t, data, output.Bytes())
	// An identical retry returns the same acknowledgement.
	var again bytes.Buffer
	if err := prepareWorkspace(t.Context(), harness.NativeCodex, data, testLimit, &again); err != nil ||
		!bytes.Equal(again.Bytes(), output.Bytes()) {
		t.Fatalf("identical retry: %s %v", again.Bytes(), err)
	}
	// Another artifact conflicts, whatever the stage.
	if err := prepareWorkspace(t.Context(), harness.NativeClaude, data, testLimit, &again); err == nil ||
		!strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting artifact: %v", err)
	}
}

// A target this helper did not publish is never adopted or removed.
func TestPrepareRefusesATargetWithoutAReceipt(t *testing.T) {
	privateHelper(t)
	data := workspaceArtifact(t)
	if err := os.MkdirAll(helperTarget, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(t.Context(), harness.NativeCodex, data, testLimit, &bytes.Buffer{}); err == nil {
		t.Fatal("existing target adopted")
	}
	if _, ok, err := readReceipt(); ok || err != nil {
		t.Fatalf("receipt written for a refused target: %v %v", ok, err)
	}
}

// Capture needs an acknowledged restore; a receipt that is not one of ours
// fails closed.
func TestCaptureRequiresAnAcknowledgedRestore(t *testing.T) {
	privateHelper(t)
	c := command{kind: harness.NativeCodex, home: "/workspace/session/codex", repository: "/workspace/session/workspace",
		thread: "01234567-89ab-cdef-0123-456789abcdef", limit: testLimit}
	if err := c.capture(t.Context(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "acknowledged restore") {
		t.Fatalf("capture without a restore: %v", err)
	}
	meta := checkpoint.Metadata{Harness: harness.NativeCodex, SHA256: strings.Repeat("0", 64), Bytes: 1}
	if err := writeReceipt(restoreReceipt{Stage: stageRestoring, Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	if err := c.capture(t.Context(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "acknowledged restore") {
		t.Fatalf("capture during a restore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(helperDirectory, helperReceiptName), []byte(`{"sha256":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := finishAllowed(); err == nil {
		t.Fatal("unreadable receipt accepted")
	}
}
