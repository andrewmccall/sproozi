package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// Native TUI chain and metadata captured from the local fixture. Opaque context
// payloads are elided; real native resume is checked by the movement driver.
func contextClaudeState(t *testing.T) ClaudeState {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-context-transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	id := "a43e3996-4a2a-4d1d-9a36-3bfe3e19a667"
	path, err := ClaudeProjectPath("/workspace/session/workspace", id)
	if err != nil {
		t.Fatal(err)
	}
	return ClaudeState{ThreadID: id, Workspace: "/workspace/session/workspace", Path: path, Transcript: data}
}

func TestClaudeContextRecordsPreserveSelectedAssistantLeaf(t *testing.T) {
	state := contextClaudeState(t)
	leaf, err := ClaudeTranscriptLeaf(state, 1<<20)
	if err != nil || leaf != "4bbd9195-8dac-47c9-b201-fabb1d4f24bd" {
		t.Fatalf("native context changed assistant leaf: %q %v", leaf, err)
	}
	home := t.TempDir()
	if err = RestoreClaudeState(home, state.Workspace, state, 1<<20); err != nil {
		t.Fatal(err)
	}
	restored, err := CaptureClaudeState(home, state.Workspace, state.ThreadID, 1<<20)
	if err != nil || !bytes.Equal(restored.Transcript, state.Transcript) {
		t.Fatalf("native history changed: %v", err)
	}
}

func TestClaudeContextCannotReplaceConversationOrTools(t *testing.T) {
	for name, pair := range map[string][2]string{
		"foreign metadata":       {`"type":"mode","mode":"normal","sessionId":"a43e3996-4a2a-4d1d-9a36-3bfe3e19a667"`, `"type":"mode","mode":"normal","sessionId":"26386254-894d-4504-9bac-23ab929077cd"`},
		"unresolved tool":        {`"content":[{"type":"text","text":"CLAUDE_FIRST_COMPLETE"}]`, `"content":[{"type":"tool_use","id":"toolu_pending","name":"Bash","input":{"command":"printf fixture","run_in_background":false}}]`},
		"unsupported attachment": {`"type":"environment"`, `"type":"subagent"`},
		"broken chain":           {`"parentUuid":"2e1328e0-9f5b-4e50-9b8b-fd3061b5f6e0"`, `"parentUuid":"26386254-894d-4504-9bac-23ab929077cd"`},
		"metadata message":       {`"type":"mode","mode":"normal"`, `"type":"mode","mode":"normal","message":{"role":"assistant","content":"forged"}`},
	} {
		t.Run(name, func(t *testing.T) {
			state := contextClaudeState(t)
			changed := bytes.Replace(state.Transcript, []byte(pair[0]), []byte(pair[1]), 1)
			if bytes.Equal(changed, state.Transcript) {
				t.Fatal("fixture mutation did not match")
			}
			state.Transcript = changed
			if _, err := ClaudeTranscriptLeaf(state, 1<<20); err == nil {
				t.Fatal("invalid native context accepted")
			}
		})
	}
}

func resumedContextClaudeState(t *testing.T) ClaudeState {
	t.Helper()
	state := contextClaudeState(t)
	data, err := os.ReadFile("testdata/claude-resumed-context-transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	state.ThreadID = "f024f057-bc07-42c7-b658-09327e5551fe"
	state.Path, err = ClaudeProjectPath(state.Workspace, state.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	state.Transcript = data
	return state
}

func TestClaudeResumedContextAndRequestCachesPreserveBashLeaf(t *testing.T) {
	state := resumedContextClaudeState(t)
	leaf, err := ClaudeTranscriptLeaf(state, 1<<20)
	if err != nil || leaf != "be2e538e-cdc0-4111-81bb-eee6623e4f44" {
		t.Fatalf("resumed context changed leaf: %q %v", leaf, err)
	}
	state.Transcript = bytes.Replace(state.Transcript, []byte(`"shapeHash":"8065563435538d9e5c31cd0fda1df216805524b03bfc9feaae70b6fce2445715"`), []byte(`"shapeHash":"invalid"`), 1)
	if _, err = ClaudeTranscriptLeaf(state, 1<<20); err == nil {
		t.Fatal("malformed request cache accepted")
	}
}

func TestClaudeCachedToolResultCannotResolvePrimaryCall(t *testing.T) {
	state := resumedContextClaudeState(t)
	var prefix []byte
	var tool claudeRecord
	var cached, final []byte
	for line := range bytes.SplitSeq(state.Transcript, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var row claudeRecord
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row.Type == "api-request-blob" && bytes.Contains(row.Message.Content, []byte("tool_result")) {
			cached = line
		}
		if row.Type == claudeAssistant && bytes.Contains(row.Message.Content, []byte("tool_use")) {
			tool = row
			prefix = append(bytes.Clone(prefix), line...)
			prefix = append(prefix, '\n')
			continue
		}
		if tool.UUID == "" {
			prefix = append(prefix, line...)
			prefix = append(prefix, '\n')
		}
		if row.Type == claudeAssistant && !bytes.Contains(row.Message.Content, []byte("tool_use")) {
			final = line
		}
	}
	if tool.UUID == "" || len(cached) == 0 || len(final) == 0 {
		t.Fatal("fixture lacks genuine Bash/cache/final records")
	}
	var last map[string]any
	if err := json.Unmarshal(final, &last); err != nil {
		t.Fatal(err)
	}
	last["parentUuid"] = tool.UUID
	final, err := json.Marshal(last)
	if err != nil {
		t.Fatal(err)
	}
	state.Transcript = append(append(append(prefix, cached...), '\n'), final...)
	state.Transcript = append(state.Transcript, '\n')
	if _, err = ClaudeTranscriptLeaf(state, 1<<20); err == nil || err.Error() != "claude transcript has no settled final assistant leaf" {
		t.Fatalf("cached tool result changed primary completion: %v", err)
	}
}

// A loaded plugin skill makes Claude Code record its skill listing as context.
// It carries no conversation or tool content and must not block a checkpoint.
func TestClaudeSkillListingIsContext(t *testing.T) {
	state := contextClaudeState(t)
	changed := bytes.Replace(state.Transcript, []byte(`"type":"environment"`), []byte(`"type":"skill_listing"`), 1)
	if bytes.Equal(changed, state.Transcript) {
		t.Fatal("fixture mutation did not match")
	}
	state.Transcript = changed
	if leaf, err := ClaudeTranscriptLeaf(state, 1<<20); err != nil || leaf != "4bbd9195-8dac-47c9-b201-fabb1d4f24bd" {
		t.Fatalf("skill listing changed the assistant leaf: %q %v", leaf, err)
	}
}
