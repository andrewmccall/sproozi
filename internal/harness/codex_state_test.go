package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nativeThread = "01a126e8-288d-7350-9b52-627e16048343"

func completedNativeState() CodexState {
	return CodexState{
		ThreadID: nativeThread,
		Path:     "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + nativeThread + ".jsonl",
		Rollout: []byte(`{"type":"session_meta","payload":{"id":"01a126e8-288d-7350-9b52-627e16048343","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-one"}}
{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"amber-otter-481"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-one"}}
`),
	}
}

func TestCodexConversationCaptureAndRestore(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	state := completedNativeState()
	if err := RestoreCodexState(source, state, 8192); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte("not-portable"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureCodexState(source, nativeThread, 8192)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := CompletedCodexTurn(captured, 8192)
	if err != nil || turn != "turn-one" || captured.Workspace != "/workspace" || string(captured.Rollout) != string(state.Rollout) {
		t.Fatalf("unexpected checkpoint: %+v, turn=%q, err=%v", captured, turn, err)
	}
	if err = RestoreCodexState(destination, captured, 8192); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, state.Path))
	if err != nil || string(data) != string(state.Rollout) {
		t.Fatalf("conversation changed: %s, %v", data, err)
	}
	if _, err = os.Stat(filepath.Join(destination, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth transferred: %v", err)
	}
	if err = RestoreCodexState(destination, captured, 8192); err == nil {
		t.Fatal("existing conversation was replaced")
	}
}

func TestCodexIncompleteAndIncompatibleStateRejected(t *testing.T) {
	for _, change := range []func(*CodexState){
		func(s *CodexState) { s.Path = "sessions/x" },
		func(s *CodexState) { s.Path = "../" + s.Path },
		func(s *CodexState) { s.Rollout = []byte(strings.ReplaceAll(string(s.Rollout), "0.161.0", "")) },
		func(s *CodexState) {
			s.Rollout = append(s.Rollout, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"pending"}}`)...)
		},
		func(s *CodexState) {
			s.Rollout = []byte(strings.ReplaceAll(string(s.Rollout), `"type":"task_complete"`, `"type":"turn_aborted"`))
		},
		func(s *CodexState) {
			s.Rollout = []byte(strings.ReplaceAll(string(s.Rollout), `"type":"task_complete"`, `"error":{"message":"provider failed"},"type":"task_complete"`))
		},
		func(s *CodexState) { s.Workspace = "/different" },
		func(s *CodexState) { s.ThreadID = "00000000-0000-0000-0000-000000000000" },
	} {
		state, home := completedNativeState(), t.TempDir()
		change(&state)
		if err := RestoreCodexState(home, state, 8192); err == nil {
			t.Fatalf("accepted incompatible state: %+v", state)
		}
		files, err := os.ReadDir(home)
		if err != nil || len(files) != 0 {
			t.Fatalf("invalid restore modified home: %v, %v", files, err)
		}
	}
	state := completedNativeState()
	if _, err := CompletedCodexTurn(state, 32); err == nil {
		t.Fatal("oversized state accepted")
	}
}

func TestCodexRestoreRejectsSymlink(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := RestoreCodexState(home, completedNativeState(), 8192); err == nil {
		t.Fatal("restored through a symlink")
	}
	files, err := os.ReadDir(outside)
	if err != nil || len(files) != 0 {
		t.Fatalf("outside directory changed: %v, %v", files, err)
	}
}

func TestCodexRestoreUsesStateContractNotClientVersion(t *testing.T) {
	state := completedNativeState()
	state.Rollout = []byte(strings.ReplaceAll(string(state.Rollout), "0.161.0", "upstream-default"))
	home := t.TempDir()
	if err := RestoreCodexState(home, state, 8192); err != nil {
		t.Fatal(err)
	}
	restored, err := CaptureCodexState(home, nativeThread, 8192)
	if err != nil || string(restored.Rollout) != string(state.Rollout) {
		t.Fatalf("changed client version lost conversation: %v", err)
	}
}
