package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// Exact headless first completed exchange from the pinned credential-free proof.
const claudeCheckpointTranscript = `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-10-11T16:07:25` +
	`.305Z","sessionId":"26386254-894d-4504-9bac-23ab929077cd"}` +
	"\n" +
	`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/proof/claud` +
	`e-state-obkimeub/0/workspace","sessionId":"26386254-894d-4504-9bac-23ab929077cd"` +
	`,"version":"2.1.27","gitBranch":"","type":"user","message":{"role":"user","conte` +
	`nt":"Remember the fixture fact silver-tern-681."},"uuid":"eef9de82-f31f-4d88-afe` +
	`9-5c117626c025","timestamp":"2026-10-11T16:07:25.316Z","permissionMode":"bypassP` +
	`ermissions"}` +
	"\n" +
	`{"parentUuid":"eef9de82-f31f-4d88-afe9-5c117626c025","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-obkimeub/0/workspace","sessionId":"263` +
	`86254-894d-4504-9bac-23ab929077cd","version":"2.1.27","gitBranch":"","message":{` +
	`"id":"msg_claude_fixture_1","type":"message","role":"assistant","model":"claude-` +
	`sonnet-4-5","content":[{"type":"text","text":"CLAUDE_FIRST_COMPLETE"}],"stop_rea` +
	`son":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":0}},"t` +
	`ype":"assistant","uuid":"9d2ec943-c3d0-436d-b599-6f62917a9e0a","timestamp":"2026` +
	`-10-11T16:07:25.340Z"}` +
	"\n"

func TestClaudeCheckpointPreservesExplicitHarnessAndNativeBytes(t *testing.T) {
	repository, _, _ := source(t)
	home := t.TempDir()
	const id = "26386254-894d-4504-9bac-23ab929077cd"
	path, err := harness.ClaudeProjectPath(repository, id)
	if err != nil {
		t.Fatal(err)
	}
	state := harness.ClaudeState{ThreadID: id, Workspace: repository, Path: path, Transcript: []byte(claudeCheckpointTranscript)}
	if err = harness.RestoreClaudeState(home, repository, state, checkpointLimit); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("nonportable"), 0600); err != nil {
		t.Fatal(err)
	}
	data, meta, err := CaptureStopped(context.Background(), harness.NativeClaude, home, repository, id, nil, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Harness != harness.NativeClaude || meta.ThreadID != id || meta.TurnID != "9d2ec943-c3d0-436d-b599-6f62917a9e0a" {
		t.Fatalf("incorrect continuity metadata: %+v", meta)
	}
	if err = ValidateContinuity(data, harness.NativeCodex, strings.Repeat("0", 64), checkpointLimit); err == nil {
		t.Fatal("Claude artifact admitted for Codex")
	}
	if err = ValidateContinuity(data, harness.NativeClaude, strings.Repeat("0", 64), checkpointLimit); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "session")
	restored, err := Restore(context.Background(), data, target, checkpointLimit)
	if err != nil || restored != meta {
		t.Fatalf("atomic restore: %v", err)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	native, err := harness.CaptureClaudeState(filepath.Join(target, "claude"), filepath.Join(target, "workspace"), id, checkpointLimit)
	if err != nil || !bytes.Equal(native.Transcript, state.Transcript) {
		t.Fatalf("native bytes changed: %v", err)
	}
	if _, err = os.Stat(filepath.Join(target, "claude", ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("credential included in portable state")
	}
	if _, err = os.Stat(filepath.Join(target, "codex")); !os.IsNotExist(err) {
		t.Fatal("wrong harness directory published")
	}
}

func TestExplicitLegacyCodexArtifactAndDiscriminatorConflicts(t *testing.T) {
	repository, home, _ := source(t)
	data, _, err := CaptureStopped(t.Context(), harness.NativeCodex, home, repository, fixtureThread, nil, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	_, artifact, err := inspect(data, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	old, err := json.Marshal(codexArtifact{Version: 1, Native: *artifact.Codex, Workspace: artifact.Workspace})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := Inspect(old, checkpointLimit)
	if err != nil || meta.Harness != harness.NativeCodex {
		t.Fatalf("explicit v1 compatibility: %v", err)
	}
	for _, kind := range []harness.NativeKind{"", harness.NativeClaude, "unknown"} {
		changed := artifact
		changed.Harness = kind
		encoded, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Inspect(encoded, checkpointLimit); err == nil {
			t.Fatalf("inconsistent kind %q accepted", kind)
		}
	}
	artifact.Claude = &harness.ClaudeState{}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(encoded, checkpointLimit); err == nil {
		t.Fatal("two native states accepted")
	}
}
