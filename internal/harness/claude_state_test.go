package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeProofID = "26386254-894d-4504-9bac-23ab929077cd"
const claudeProofWorkspace = "/proof/claude-state-obkimeub/1/workspace"

// Captured from stock 2.1.27 against the credential-free fixture. This literal
// preserves the observed primary chain, null stop reasons and genuine Bash pair.
const claudeProofTranscript = `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-10-11T16:07:25` +
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
	"\n" +
	`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-10-11T16:07:25` +
	`.984Z","sessionId":"26386254-894d-4504-9bac-23ab929077cd"}` +
	"\n" +
	`{"parentUuid":"9d2ec943-c3d0-436d-b599-6f62917a9e0a","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-obkimeub/1/workspace","sessionId":"263` +
	`86254-894d-4504-9bac-23ab929077cd","version":"2.1.27","gitBranch":"","type":"use` +
	`r","message":{"role":"user","content":"Use Bash to print TOOL_FACT=ochre-mole-39` +
	`2, then remember that fact."},"uuid":"9f0fd19f-70be-4d83-b059-3733df000142","tim` +
	`estamp":"2026-10-11T16:07:25.989Z","permissionMode":"bypassPermissions"}` +
	"\n" +
	`{"parentUuid":"9f0fd19f-70be-4d83-b059-3733df000142","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-obkimeub/1/workspace","sessionId":"263` +
	`86254-894d-4504-9bac-23ab929077cd","version":"2.1.27","gitBranch":"","message":{` +
	`"id":"msg_claude_fixture_2","type":"message","role":"assistant","model":"claude-` +
	`sonnet-4-5","content":[{"type":"tool_use","id":"toolu_claude_fixture","name":"Ba` +
	`sh","input":{"command":"printf 'TOOL_FACT=ochre-mole-392\\n'","description":"Pri` +
	`nt deterministic fixture fact"}}],"stop_reason":null,"stop_sequence":null,"usage` +
	`":{"input_tokens":32,"output_tokens":0}},"type":"assistant","uuid":"53571eff-32d` +
	`2-4ea3-b323-6f2645ccb87d","timestamp":"2026-10-11T16:07:26.007Z"}` +
	"\n" +
	`{"parentUuid":"53571eff-32d2-4ea3-b323-6f2645ccb87d","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-obkimeub/1/workspace","sessionId":"263` +
	`86254-894d-4504-9bac-23ab929077cd","version":"2.1.27","gitBranch":"","type":"use` +
	`r","message":{"role":"user","content":[{"tool_use_id":"toolu_claude_fixture","ty` +
	`pe":"tool_result","content":"TOOL_FACT=ochre-mole-392","is_error":false}]},"uuid` +
	`":"5be45849-cc0a-42fd-aa34-c9592252c788","timestamp":"2026-10-11T16:07:26.051Z",` +
	`"toolUseResult":{"stdout":"TOOL_FACT=ochre-mole-392","stderr":"","interrupted":` +
	`false,"isImage":false},"sourceToolAssistantUUID":"53571eff-32d2-4ea3-b323-6f2645c` +
	`cb87d"}` +
	"\n" +
	`{"parentUuid":"5be45849-cc0a-42fd-aa34-c9592252c788","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-obkimeub/1/workspace","sessionId":"263` +
	`86254-894d-4504-9bac-23ab929077cd","version":"2.1.27","gitBranch":"","message":{` +
	`"id":"msg_claude_fixture_5","type":"message","role":"assistant","model":"claude-` +
	`sonnet-4-5","content":[{"type":"text","text":"CLAUDE_TOOL_COMPLETE"}],"stop_reas` +
	`on":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":0}},"ty` +
	`pe":"assistant","uuid":"96353baf-7fa3-49b3-8eb9-015a41b188c6","timestamp":"2026-` +
	`10-11T16:07:26.067Z"}` +
	"\n"

// Exact first two completed native TUI turns, before the failed /quit diagnostic.
const claudeTUITranscript = `{"type":"file-history-snapshot","messageId":"182184c8-ce35-4a1b-9aca-cd170d00035` +
	`b","snapshot":{"messageId":"182184c8-ce35-4a1b-9aca-cd170d00035b","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T16:39:29.495Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"type":"file-history-snapshot","messageId":"3785d0c0-7b0c-4f1b-865f-b8268925cb0` +
	`7","snapshot":{"messageId":"3785d0c0-7b0c-4f1b-865f-b8268925cb07","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T16:39:29.576Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/proof/claud` +
	`e-state-aq9oxx9l/tui-0/workspace","sessionId":"f711784b-2bf9-46a5-bb11-85faf9beb` +
	`f8b","version":"2.1.27","gitBranch":"","type":"user","message":{"role":"user","c` +
	`ontent":"Remember the fixture fact silver-tern-681."},"uuid":"3785d0c0-7b0c-4f1b` +
	`-865f-b8268925cb07","timestamp":"2026-10-11T16:39:29.527Z","thinkingMetadata":{"` +
	`maxThinkingTokens":31999},"todos":[],"permissionMode":"bypassPermissions"}` +
	"\n" +
	`{"parentUuid":"3785d0c0-7b0c-4f1b-865f-b8268925cb07","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_2","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"text","text":"CLAUDE_FIRST_COMPLETE"}],"stop` +
	`_reason":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":0}` +
	`},"type":"assistant","uuid":"0405daa0-f5ee-4027-9dbb-90948e71c3bb","timestamp":"` +
	`2026-10-11T16:39:29.627Z"}` +
	"\n" +
	`{"parentUuid":"0405daa0-f5ee-4027-9dbb-90948e71c3bb","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"Stop","hookName":"Stop","` +
	`command":"python3 /verify.py --record-hook /evidence/hooks.private.jsonl"},"pare` +
	`ntToolUseID":"cdd4a5cd-e962-49ee-88fd-d06cd055b83f","toolUseID":"cdd4a5cd-e962-4` +
	`9ee-88fd-d06cd055b83f","timestamp":"2026-10-11T16:39:29.643Z","uuid":"232fee1d-0` +
	`1d8-4f3e-966f-bacc49146118"}` +
	"\n" +
	`{"parentUuid":"232fee1d-01d8-4f3e-966f-bacc49146118","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"system","subtype":"stop_hook_summary","hookCount":1,"hookInfos":[{"command":"py` +
	`thon3 /verify.py --record-hook /evidence/hooks.private.jsonl"}],"hookErrors":[],` +
	`"preventedContinuation":false,"stopReason":"","hasOutput":false,"level":"suggest` +
	`ion","timestamp":"2026-10-11T16:39:29.700Z","uuid":"3a28889c-c647-4452-8408-15e1` +
	`1a07c5ca","toolUseID":"cdd4a5cd-e962-49ee-88fd-d06cd055b83f"}` +
	"\n" +
	`{"type":"file-history-snapshot","messageId":"7ea281b2-9174-4695-9dc7-a408ee4d9e7` +
	`0","snapshot":{"messageId":"7ea281b2-9174-4695-9dc7-a408ee4d9e70","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T16:39:30.069Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"parentUuid":"3a28889c-c647-4452-8408-15e11a07c5ca","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"user","message":{"role":"user","content":"Use Bash to print TOOL_FACT=ochre-mol` +
	`e-392, then remember that fact."},"uuid":"7ea281b2-9174-4695-9dc7-a408ee4d9e70",` +
	`"timestamp":"2026-10-11T16:39:30.023Z","thinkingMetadata":{"maxThinkingTokens":3` +
	`1999},"todos":[],"permissionMode":"bypassPermissions"}` +
	"\n" +
	`{"parentUuid":"7ea281b2-9174-4695-9dc7-a408ee4d9e70","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_4","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_claude_fixture","name"` +
	`:"Bash","input":{"command":"printf 'TOOL_FACT=ochre-mole-392\\n'","description":` +
	`"Print deterministic fixture fact"}}],"stop_reason":null,"stop_sequence":null,"u` +
	`sage":{"input_tokens":32,"output_tokens":0}},"type":"assistant","uuid":"ce6cbee3` +
	`-d957-4962-bcfe-e7ec9184a5e1","timestamp":"2026-10-11T16:39:30.097Z"}` +
	"\n" +
	`{"parentUuid":"ce6cbee3-d957-4962-bcfe-e7ec9184a5e1","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"PreToolUse","hookName":"P` +
	`reToolUse:Bash","command":"python3 /verify.py --record-hook /evidence/hooks.priv` +
	`ate.jsonl"},"toolUseID":"toolu_claude_fixture","parentToolUseID":"toolu_claude_f` +
	`ixture","uuid":"99c20e57-cef4-45a6-a8e7-02da09656ccb","timestamp":"2026-10-11T16` +
	`:39:30.110Z"}` +
	"\n" +
	`{"parentUuid":"ce6cbee3-d957-4962-bcfe-e7ec9184a5e1","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"user","message":{"role":"user","content":[{"tool_use_id":"toolu_claude_fixture"` +
	`,"type":"tool_result","content":"TOOL_FACT=ochre-mole-392","is_error":false}]},"` +
	`uuid":"b61cc52a-c97b-4de4-83e1-5c55962e1182","timestamp":"2026-10-11T16:39:30.19` +
	`6Z","toolUseResult":{"stdout":"TOOL_FACT=ochre-mole-392","stderr":"","interrupte` +
	`d":false,"isImage":false},"sourceToolAssistantUUID":"ce6cbee3-d957-4962-bcfe-e7e` +
	`c9184a5e1"}` +
	"\n" +
	`{"parentUuid":"b61cc52a-c97b-4de4-83e1-5c55962e1182","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"PostToolUse","hookName":"` +
	`PostToolUse:Bash","command":"python3 /verify.py --record-hook /evidence/hooks.pr` +
	`ivate.jsonl"},"parentToolUseID":"toolu_claude_fixture","toolUseID":"toolu_claude` +
	`_fixture","timestamp":"2026-10-11T16:39:30.197Z","uuid":"189d7f49-25b0-4c87-a81e` +
	`-228de4d098b6"}` +
	"\n" +
	`{"parentUuid":"189d7f49-25b0-4c87-a81e-228de4d098b6","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_7","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"text","text":"CLAUDE_TOOL_COMPLETE"}],"stop_` +
	`reason":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":0}}` +
	`,"type":"assistant","uuid":"615526d8-792c-4c8b-bd53-e74739c5f541","timestamp":"2` +
	`026-10-11T16:39:30.335Z"}` +
	"\n" +
	`{"parentUuid":"615526d8-792c-4c8b-bd53-e74739c5f541","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"Stop","hookName":"Stop","` +
	`command":"python3 /verify.py --record-hook /evidence/hooks.private.jsonl"},"pare` +
	`ntToolUseID":"d170efc0-e104-47c0-ae48-d192f9a1d15a","toolUseID":"d170efc0-e104-4` +
	`7c0-ae48-d192f9a1d15a","timestamp":"2026-10-11T16:39:30.345Z","uuid":"f67db2b1-d` +
	`e27-4944-ae95-33ac3e0c4eae"}` +
	"\n" +
	`{"parentUuid":"f67db2b1-de27-4944-ae95-33ac3e0c4eae","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-aq9oxx9l/tui-0/workspace","sessionId":` +
	`"f711784b-2bf9-46a5-bb11-85faf9bebf8b","version":"2.1.27","gitBranch":"","type":` +
	`"system","subtype":"stop_hook_summary","hookCount":1,"hookInfos":[{"command":"py` +
	`thon3 /verify.py --record-hook /evidence/hooks.private.jsonl"}],"hookErrors":[],` +
	`"preventedContinuation":false,"stopReason":"","hasOutput":false,"level":"suggest` +
	`ion","timestamp":"2026-10-11T16:39:30.380Z","uuid":"74e06007-3f2d-4274-8f48-ae00` +
	`40cf0862","toolUseID":"d170efc0-e104-47c0-ae48-d192f9a1d15a"}` +
	"\n"

func claudeProofState(t *testing.T) ClaudeState {
	t.Helper()
	path, err := ClaudeProjectPath(claudeProofWorkspace, claudeProofID)
	if err != nil {
		t.Fatal(err)
	}
	return ClaudeState{ThreadID: claudeProofID, Workspace: claudeProofWorkspace,
		Path: path, Transcript: []byte(claudeProofTranscript)}
}

func TestClaudeActualCapturedTranscriptLeaf(t *testing.T) {
	state := claudeProofState(t)
	leaf, err := ClaudeTranscriptLeaf(state, 8192)
	if err != nil || leaf != "96353baf-7fa3-49b3-8eb9-015a41b188c6" {
		t.Fatalf("actual pinned transcript rejected: leaf=%q, error=%v", leaf, err)
	}
	if state.Path != "projects/-proof-claude-state-obkimeub-1-workspace/"+claudeProofID+".jsonl" {
		t.Fatalf("incorrect project path: %q", state.Path)
	}
	if bytes.Contains(state.Transcript, []byte("INITIAL_CLAUDE_111")) {
		t.Fatal("fixture unexpectedly contains approved instruction evidence")
	}
}

func TestClaudeExactTranscriptCaptureAndFreshProjectRestore(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	state := claudeProofState(t)
	if err := RestoreClaudeState(source, state.Workspace, state, 8192); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte("not-portable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte("not-portable"), 0600); err != nil {
		t.Fatal(err)
	}
	// A different native ID in the same project must not be adopted.
	other := filepath.Join(source, filepath.Dir(state.Path), "00000000-0000-0000-0000-000000000000.jsonl")
	if err := os.WriteFile(other, []byte("other-thread"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureClaudeState(source, state.Workspace, state.ThreadID, 8192)
	if err != nil || !bytes.Equal(captured.Transcript, state.Transcript) {
		t.Fatalf("exact capture failed: %v", err)
	}
	const destinationWorkspace = "/workspace/session/workspace"
	if err = RestoreClaudeState(destination, destinationWorkspace, captured, 8192); err != nil {
		t.Fatal(err)
	}
	expected := "projects/-workspace-session-workspace/" + claudeProofID + ".jsonl"
	got, err := os.ReadFile(filepath.Join(destination, expected))
	if err != nil || !bytes.Equal(got, state.Transcript) {
		t.Fatalf("opaque history changed: %v", err)
	}
	for _, excluded := range []string{".credentials.json", "settings.json", filepath.Base(other)} {
		if _, err = os.Stat(filepath.Join(destination, excluded)); !os.IsNotExist(err) {
			t.Fatalf("non-selected file transferred: %s, %v", excluded, err)
		}
	}
	files := 0
	if err = filepath.WalkDir(destination, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	}); err != nil || files != 1 {
		t.Fatalf("restore copied extra files: %d, %v", files, err)
	}
	if err = RestoreClaudeState(destination, destinationWorkspace, captured, 8192); err == nil {
		t.Fatal("existing transcript overwritten")
	}
}

func TestClaudeIncompleteForeignOrUnsupportedStateRejected(t *testing.T) {
	cases := map[string]func(*ClaudeState){
		"path traversal": func(s *ClaudeState) { s.Path = "../" + s.Path },
		"wrong project":  func(s *ClaudeState) { s.Workspace = "/other/workspace" },
		"foreign ID":     func(s *ClaudeState) { s.ThreadID = "00000000-0000-0000-0000-000000000000" },
		"missing version": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript, []byte("2.1.27"), []byte(""))
		},
		"sidechain": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript, []byte(`"isSidechain":false`), []byte(`"isSidechain":true`))
		},
		"broken parent": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript,
				[]byte(`"parentUuid":"eef9de82-f31f-4d88-afe9-5c117626c025"`), []byte(`"parentUuid":null`))
		},
		"pending provider": func(s *ClaudeState) {
			i := bytes.LastIndex(s.Transcript[:len(s.Transcript)-1], []byte{'\n'})
			s.Transcript = s.Transcript[:i+1]
		},
		"partial final line": func(s *ClaudeState) { s.Transcript = s.Transcript[:len(s.Transcript)-1] },
		"unknown record": func(s *ClaudeState) {
			s.Transcript = bytes.Replace(s.Transcript, []byte(`"type":"queue-operation"`), []byte(`"type":"system"`), 1)
		},
		"unmatched tool result": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript,
				[]byte(`"tool_use_id":"toolu_claude_fixture"`), []byte(`"tool_use_id":"unknown"`))
		},
		"wrong tool source": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript,
				[]byte(`"sourceToolAssistantUUID":"53571eff-32d2-4ea3-b323-6f2645ccb87d"`),
				[]byte(`"sourceToolAssistantUUID":"unknown"`))
		},
		"background tool": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript,
				[]byte(`"input":{"command"`), []byte(`"input":{"run_in_background":true,"command"`))
		},
		"unproven tool": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript, []byte(`"name":"Bash"`), []byte(`"name":"Task"`))
		},
		"interrupted tool": func(s *ClaudeState) {
			s.Transcript = bytes.ReplaceAll(s.Transcript, []byte(`"interrupted":false`), []byte(`"interrupted":true`))
		},
		"new queued turn": func(s *ClaudeState) {
			s.Transcript = append(s.Transcript,
				[]byte(`{"type":"queue-operation","operation":"dequeue","sessionId":"`+claudeProofID+`"}`+"\n")...)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			state := claudeProofState(t)
			change(&state)
			home := t.TempDir()
			if err := RestoreClaudeState(home, "/workspace", state, 8192); err == nil {
				t.Fatal("unsupported transcript restored")
			}
			files, err := os.ReadDir(home)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid restore modified home: %v, %v", files, err)
			}
		})
	}
	if _, err := ClaudeTranscriptLeaf(claudeProofState(t), 32); err == nil {
		t.Fatal("oversized transcript accepted")
	}
}

func TestClaudeCaptureAndRestoreRejectSymlinks(t *testing.T) {
	state := claudeProofState(t)
	for _, operation := range []string{"capture", "restore"} {
		t.Run(operation, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(outside, filepath.Join(home, "projects")); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "capture" {
				_, err = CaptureClaudeState(home, state.Workspace, state.ThreadID, 8192)
			} else {
				err = RestoreClaudeState(home, state.Workspace, state, 8192)
			}
			if err == nil {
				t.Fatal("symlink directory accepted")
			}
			files, err := os.ReadDir(outside)
			if err != nil || len(files) != 0 {
				t.Fatalf("outside directory changed: %v, %v", files, err)
			}
		})
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, filepath.Dir(state.Path)), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(outside, []byte(claudeProofTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, state.Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureClaudeState(home, state.Workspace, state.ThreadID, 8192); err == nil {
		t.Fatal("symlink transcript accepted")
	}
	if _, err := ClaudeProjectPath("../workspace", claudeProofID); err == nil {
		t.Fatal("relative workspace accepted")
	}
	if _, err := ClaudeProjectPath("/workspace/../other", claudeProofID); err == nil {
		t.Fatal("unclean workspace accepted")
	}
	if strings.Contains(claudeProofTranscript, "INITIAL_CLAUDE_111") {
		t.Fatal("invented instruction evidence")
	}
}

func TestClaudeCapturedFixtureDigests(t *testing.T) {
	for name, fixture := range map[string]struct{ data, digest string }{
		"headless": {claudeProofTranscript, "8ef566b7a0c322b5318dbcfc1f332e123f137e09d10d06f5fc7136837da71f64"},
		"tui":      {claudeTUITranscript, "bd236438336d24b5f1c83cc7a46769168c8b405e04ce3df409d094d7f4bcb062"},
	} {
		digest := sha256.Sum256([]byte(fixture.data))
		if hex.EncodeToString(digest[:]) != fixture.digest {
			t.Fatalf("%s literal differs from captured native bytes: %x", name, digest)
		}
	}
}

func claudeTUIState(t *testing.T) ClaudeState {
	t.Helper()
	const id = "f711784b-2bf9-46a5-bb11-85faf9bebf8b"
	const workspace = "/proof/claude-state-aq9oxx9l/tui-0/workspace"
	path, err := ClaudeProjectPath(workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	return ClaudeState{ThreadID: id, Workspace: workspace, Path: path, Transcript: []byte(claudeTUITranscript)}
}

func TestClaudeActualTUITranscriptCaptureAndFreshRestore(t *testing.T) {
	state := claudeTUIState(t)
	leaf, err := ClaudeTranscriptLeaf(state, 32768)
	if err != nil || leaf != "615526d8-792c-4c8b-bd53-e74739c5f541" {
		t.Fatalf("actual TUI chain rejected: leaf=%q error=%v", leaf, err)
	}
	source, destination := t.TempDir(), t.TempDir()
	if err = RestoreClaudeState(source, state.Workspace, state, 32768); err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureClaudeState(source, state.Workspace, state.ThreadID, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if err = RestoreClaudeState(destination, "/workspace/session/workspace", captured, 32768); err != nil {
		t.Fatal(err)
	}
	path, err := ClaudeProjectPath("/workspace/session/workspace", state.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, path))
	if err != nil || !bytes.Equal(got, state.Transcript) {
		t.Fatalf("native bytes changed: %v", err)
	}
}

func TestClaudeTUIUnsupportedMetadataAndIncompleteChains(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"unknown-progress": func(s string) string {
			return strings.Replace(s, `"type":"hook_progress"`, `"type":"unknown_progress"`, 1)
		},
		"unproven-file-backup": func(s string) string {
			return strings.Replace(s, `"trackedFileBackups":{}`, `"trackedFileBackups":{"a":{"backupFileName":"outside"}}`, 1)
		},
		"snapshot-update": func(s string) string {
			return strings.Replace(s, `"isSnapshotUpdate":false`, `"isSnapshotUpdate":true`, 1)
		},
		"prevented-stop": func(s string) string {
			return strings.Replace(s, `"preventedContinuation":false`, `"preventedContinuation":true`, 1)
		},
		"hook-error": func(s string) string { return strings.Replace(s, `"hookErrors":[]`, `"hookErrors":["failed"]`, 1) },
		"unknown-tool-hook": func(s string) string {
			return strings.Replace(s, `"hookName":"PreToolUse:Bash"`, `"hookName":"PreToolUse:Task"`, 1)
		},
		"api-error": func(s string) string {
			return strings.Replace(s, `"type":"assistant"`, `"isApiErrorMessage":true,"type":"assistant"`, 1)
		},
		"missing-assistant-leaf": func(s string) string {
			lines := strings.SplitAfter(s, "\n")
			return strings.Join(append(lines[:12], lines[13:]...), "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := claudeTUIState(t)
			changed := change(string(state.Transcript))
			if changed == string(state.Transcript) {
				t.Fatal("negative control did not change actual fixture")
			}
			state.Transcript = []byte(changed)
			if _, err := ClaudeTranscriptLeaf(state, 32768); err == nil {
				t.Fatal("unsupported or incomplete TUI chain accepted")
			}
		})
	}
}

// Exact successful controlled-stop and fresh-home TUI resume artifact.
const claudeResumedTUITranscript = `{"type":"file-history-snapshot","messageId":"3d46d5a0-19d6-4036-a2fd-fcffbeeebd1` +
	`d","snapshot":{"messageId":"3d46d5a0-19d6-4036-a2fd-fcffbeeebd1d","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T17:06:32.760Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"type":"file-history-snapshot","messageId":"cb436c79-8550-40d5-b4f1-6732aa086ab` +
	`6","snapshot":{"messageId":"cb436c79-8550-40d5-b4f1-6732aa086ab6","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T17:06:32.855Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/proof/claud` +
	`e-state-wlqb37ic/tui-0/workspace","sessionId":"b0169770-4519-40a6-879f-bf4f3ee7e` +
	`7fe","version":"2.1.27","gitBranch":"","type":"user","message":{"role":"user","c` +
	`ontent":"Remember the fixture fact silver-tern-681."},"uuid":"cb436c79-8550-40d5` +
	`-b4f1-6732aa086ab6","timestamp":"2026-10-11T17:06:32.793Z","thinkingMetadata":{"` +
	`maxThinkingTokens":31999},"todos":[],"permissionMode":"bypassPermissions"}` +
	"\n" +
	`{"parentUuid":"cb436c79-8550-40d5-b4f1-6732aa086ab6","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_2","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"text","text":"CLAUDE_FIRST_COMPLETE"}],"stop` +
	`_reason":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":0}` +
	`},"type":"assistant","uuid":"bd599d5c-06b9-4d23-8b4b-ac6b063b7757","timestamp":"` +
	`2026-10-11T17:06:32.907Z"}` +
	"\n" +
	`{"parentUuid":"bd599d5c-06b9-4d23-8b4b-ac6b063b7757","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"Stop","hookName":"Stop","` +
	`command":"python3 /verify.py --record-hook /evidence/hooks.private.jsonl"},"pare` +
	`ntToolUseID":"9a998c56-c5b0-46ea-a7f3-beee4c1a56ba","toolUseID":"9a998c56-c5b0-4` +
	`6ea-a7f3-beee4c1a56ba","timestamp":"2026-10-11T17:06:32.919Z","uuid":"4601c82c-a` +
	`9b1-4081-87ba-869b969efb33"}` +
	"\n" +
	`{"parentUuid":"4601c82c-a9b1-4081-87ba-869b969efb33","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"system","subtype":"stop_hook_summary","hookCount":1,"hookInfos":[{"command":"py` +
	`thon3 /verify.py --record-hook /evidence/hooks.private.jsonl"}],"hookErrors":[],` +
	`"preventedContinuation":false,"stopReason":"","hasOutput":false,"level":"suggest` +
	`ion","timestamp":"2026-10-11T17:06:32.971Z","uuid":"0dd20bb1-31d0-417c-9dfe-73a3` +
	`67f522a1","toolUseID":"9a998c56-c5b0-46ea-a7f3-beee4c1a56ba"}` +
	"\n" +
	`{"type":"file-history-snapshot","messageId":"b8c23949-115e-4682-a29e-3f009b98ae9` +
	`2","snapshot":{"messageId":"b8c23949-115e-4682-a29e-3f009b98ae92","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T17:06:33.347Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"parentUuid":"0dd20bb1-31d0-417c-9dfe-73a367f522a1","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"user","message":{"role":"user","content":"Use Bash to print TOOL_FACT=ochre-mol` +
	`e-392, then remember that fact."},"uuid":"b8c23949-115e-4682-a29e-3f009b98ae92",` +
	`"timestamp":"2026-10-11T17:06:33.294Z","thinkingMetadata":{"maxThinkingTokens":3` +
	`1999},"todos":[],"permissionMode":"bypassPermissions"}` +
	"\n" +
	`{"parentUuid":"b8c23949-115e-4682-a29e-3f009b98ae92","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_4","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"tool_use","id":"toolu_claude_fixture","name"` +
	`:"Bash","input":{"command":"printf 'TOOL_FACT=ochre-mole-392\\n'","description":` +
	`"Print deterministic fixture fact"}}],"stop_reason":null,"stop_sequence":null,"u` +
	`sage":{"input_tokens":32,"output_tokens":0}},"type":"assistant","uuid":"1b9b7f1e` +
	`-6193-48b9-8b4d-a09a36f71463","timestamp":"2026-10-11T17:06:33.368Z"}` +
	"\n" +
	`{"parentUuid":"1b9b7f1e-6193-48b9-8b4d-a09a36f71463","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"PreToolUse","hookName":"P` +
	`reToolUse:Bash","command":"python3 /verify.py --record-hook /evidence/hooks.priv` +
	`ate.jsonl"},"toolUseID":"toolu_claude_fixture","parentToolUseID":"toolu_claude_f` +
	`ixture","uuid":"e530af68-37f3-4a26-8b42-0e1babbc15c7","timestamp":"2026-10-11T17` +
	`:06:33.376Z"}` +
	"\n" +
	`{"parentUuid":"1b9b7f1e-6193-48b9-8b4d-a09a36f71463","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"user","message":{"role":"user","content":[{"tool_use_id":"toolu_claude_fixture"` +
	`,"type":"tool_result","content":"TOOL_FACT=ochre-mole-392","is_error":false}]},"` +
	`uuid":"56b70c17-c705-47da-9961-341b392ca90e","timestamp":"2026-10-11T17:06:33.44` +
	`3Z","toolUseResult":{"stdout":"TOOL_FACT=ochre-mole-392","stderr":"","interrupte` +
	`d":false,"isImage":false},"sourceToolAssistantUUID":"1b9b7f1e-6193-48b9-8b4d-a09` +
	`a36f71463"}` +
	"\n" +
	`{"parentUuid":"56b70c17-c705-47da-9961-341b392ca90e","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"PostToolUse","hookName":"` +
	`PostToolUse:Bash","command":"python3 /verify.py --record-hook /evidence/hooks.pr` +
	`ivate.jsonl"},"parentToolUseID":"toolu_claude_fixture","toolUseID":"toolu_claude` +
	`_fixture","timestamp":"2026-10-11T17:06:33.444Z","uuid":"4a62debd-a143-4a72-b32d` +
	`-c7308b2e179e"}` +
	"\n" +
	`{"parentUuid":"4a62debd-a143-4a72-b32d-c7308b2e179e","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_7","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"text","text":"CLAUDE_TOOL_COMPLETE"}],"stop_` +
	`reason":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":0}}` +
	`,"type":"assistant","uuid":"f1b76d63-dd1a-48a7-8ba0-2a03e2a132c7","timestamp":"2` +
	`026-10-11T17:06:33.529Z"}` +
	"\n" +
	`{"parentUuid":"f1b76d63-dd1a-48a7-8ba0-2a03e2a132c7","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-0/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"Stop","hookName":"Stop","` +
	`command":"python3 /verify.py --record-hook /evidence/hooks.private.jsonl"},"pare` +
	`ntToolUseID":"56cd43e6-f183-4d35-8483-3fd7fdf8b333","toolUseID":"56cd43e6-f183-4` +
	`d35-8483-3fd7fdf8b333","timestamp":"2026-10-11T17:06:33.541Z","uuid":"06356060-1` +
	`694-443c-afb8-71e37c58808d"}` +
	"\n" +
	`{"type":"file-history-snapshot","messageId":"1fc4ab97-436b-4b80-bede-5ecfd14e4d6` +
	`4","snapshot":{"messageId":"1fc4ab97-436b-4b80-bede-5ecfd14e4d64","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T17:06:34.257Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"type":"file-history-snapshot","messageId":"6d397913-d66e-41c8-bc69-8127d6fc8e0` +
	`9","snapshot":{"messageId":"6d397913-d66e-41c8-bc69-8127d6fc8e09","trackedFileBa` +
	`ckups":{},"timestamp":"2026-10-11T17:06:34.315Z"},"isSnapshotUpdate":false}` +
	"\n" +
	`{"parentUuid":"06356060-1694-443c-afb8-71e37c58808d","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-1/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"user","message":{"role":"user","content":"Continue using both facts from this c` +
	`onversation."},"uuid":"6d397913-d66e-41c8-bc69-8127d6fc8e09","timestamp":"2026-1` +
	`0-11T17:06:34.281Z","thinkingMetadata":{"maxThinkingTokens":31999},"permissionMo` +
	`de":"bypassPermissions"}` +
	"\n" +
	`{"parentUuid":"6d397913-d66e-41c8-bc69-8127d6fc8e09","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-1/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","messag` +
	`e":{"id":"msg_claude_fixture_9","type":"message","role":"assistant","model":"cla` +
	`ude-sonnet-4-5","content":[{"type":"text","text":"CLAUDE_RESUMED_COMPLETE"}],"st` +
	`op_reason":null,"stop_sequence":null,"usage":{"input_tokens":32,"output_tokens":` +
	`0}},"type":"assistant","uuid":"f85e025f-40ec-4f72-8cae-c80072a5313c","timestamp"` +
	`:"2026-10-11T17:06:34.365Z"}` +
	"\n" +
	`{"parentUuid":"f85e025f-40ec-4f72-8cae-c80072a5313c","isSidechain":false,"userTy` +
	`pe":"external","cwd":"/proof/claude-state-wlqb37ic/tui-1/workspace","sessionId":` +
	`"b0169770-4519-40a6-879f-bf4f3ee7e7fe","version":"2.1.27","gitBranch":"","type":` +
	`"progress","data":{"type":"hook_progress","hookEvent":"Stop","hookName":"Stop","` +
	`command":"python3 /verify.py --record-hook /evidence/hooks.private.jsonl"},"pare` +
	`ntToolUseID":"92f4a3a1-e07b-4b9f-b48b-26f8fbc45717","toolUseID":"92f4a3a1-e07b-4` +
	`b9f-b48b-26f8fbc45717","timestamp":"2026-10-11T17:06:34.374Z","uuid":"62a7f9b3-0` +
	`39b-40de-a8aa-1c525b8ce2bd"}` +
	"\n"

func TestClaudeActualControlledStopFreshHomeResumeLeaf(t *testing.T) {
	const id = "b0169770-4519-40a6-879f-bf4f3ee7e7fe"
	const workspace = "/proof/claude-state-wlqb37ic/tui-1/workspace"
	digest := sha256.Sum256([]byte(claudeResumedTUITranscript))
	if hex.EncodeToString(digest[:]) != "85447730756939c92836faeda2276de0081eabfe08d9b12c738b7ac69a27df89" {
		t.Fatalf("resumed native literal changed: %x", digest)
	}
	path, err := ClaudeProjectPath(workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	state := ClaudeState{ThreadID: id, Workspace: workspace, Path: path, Transcript: []byte(claudeResumedTUITranscript)}
	leaf, err := ClaudeTranscriptLeaf(state, 32768)
	if err != nil || leaf != "f85e025f-40ec-4f72-8cae-c80072a5313c" {
		t.Fatalf("actual resumed leaf=%q error=%v", leaf, err)
	}
}

func TestClaudeConversationCanSpanClientVersions(t *testing.T) {
	state := claudeProofState(t)
	state.Transcript = bytes.Replace(state.Transcript, []byte(`"version":"2.1.27"`), []byte(`"version":"upstream-default"`), 1)
	if _, err := ClaudeTranscriptLeaf(state, 8192); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := RestoreClaudeState(destination, claudeProofWorkspace, state, 8192); err != nil {
		t.Fatal(err)
	}
}
