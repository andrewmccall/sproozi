package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	claudeResultID = "3c1f0e52-7a4b-4d8e-9f61-2b5c8d7e9a01"
	claudeTypeKey  = "type"
	claudeContent  = "content"
)

// claudeRow is one primary-chain transcript row in the shape the pinned client
// writes; assistant rows carry the API message ID that groups one reply's
// content blocks.
func claudeRow(kind, id, parent, message string, content any) []byte {
	var parentID any
	if parent != "" {
		parentID = parent
	}
	body := map[string]any{"role": kind, claudeContent: content}
	if message != "" {
		body["id"] = message
	}
	row, _ := json.Marshal(map[string]any{claudeTypeKey: kind, "sessionId": claudeResultID, "version": "2.1.318",
		"uuid": id, "parentUuid": parentID, "isSidechain": false, "cwd": "/workspace", "message": body})
	return append(row, '\n')
}

func claudeText(text string) []map[string]any {
	return []map[string]any{{claudeTypeKey: "text", "text": text}}
}

func claudeResultArtifact(t *testing.T, rows ...[]byte) []byte {
	t.Helper()
	repository, _, _ := source(t)
	path, err := harness.ClaudeProjectPath(repository, claudeResultID)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	state := harness.ClaudeState{ThreadID: claudeResultID, Workspace: repository, Path: path,
		Transcript: []byte(strings.Join(func() []string {
			s := make([]string, len(rows))
			for i, r := range rows {
				s[i] = string(r)
			}
			return s
		}(), ""))}
	if err = harness.RestoreClaudeState(home, repository, state, checkpointLimit); err != nil {
		t.Fatal(err)
	}
	data, _, err := CaptureStopped(t.Context(), harness.NativeClaude, home, repository, claudeResultID, nil, checkpointLimit)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const (
	claudeUserRow  = "11111111-1111-4111-8111-111111111111"
	claudeEarlier  = "22222222-2222-4222-8222-222222222222"
	claudeTool     = "33333333-3333-4333-8333-333333333333"
	claudeResult   = "44444444-4444-4444-8444-444444444444"
	claudeFirst    = "55555555-5555-4555-8555-555555555555"
	claudeSecond   = "66666666-6666-4666-8666-666666666666"
	claudeToolCall = "toolu_fixture"
)

// The answer is the selected leaf's reply: every text block of the final API
// message, in order, and nothing from an earlier reply in the turn.
func TestClaudeFinalTextIsTheSelectedLeafReply(t *testing.T) {
	toolResult, _ := json.Marshal(map[string]any{claudeTypeKey: "user", "sessionId": claudeResultID, "version": "2.1.318",
		"uuid": claudeResult, "parentUuid": claudeTool, "isSidechain": false, "cwd": "/workspace",
		"sourceToolAssistantUUID": claudeTool, "toolUseResult": map[string]any{"interrupted": false},
		"message": map[string]any{"role": "user", claudeContent: []map[string]any{
			{claudeTypeKey: "tool_result", "tool_use_id": claudeToolCall, claudeContent: "ok"}}}})
	data := claudeResultArtifact(t,
		claudeRow("user", claudeUserRow, "", "", "Fix the fixture"),
		claudeRow("assistant", claudeEarlier, claudeUserRow, "msg_one", claudeText("Let me look.")),
		claudeRow("assistant", claudeTool, claudeEarlier, "msg_one", []map[string]any{
			{claudeTypeKey: "tool_use", "id": claudeToolCall, "name": "Bash", "input": map[string]any{"command": "true"}}}),
		append(toolResult, '\n'),
		claudeRow("assistant", claudeFirst, claudeResult, "msg_two", claudeText("Fixed the fixture.")),
		claudeRow("assistant", claudeSecond, claudeFirst, "msg_two", claudeText("Tests pass.")),
	)
	text, truncated, err := FinalText(data, checkpointLimit)
	if err != nil || truncated || text != "Fixed the fixture.\nTests pass." {
		t.Fatalf("final text %q truncated=%v: %v", text, truncated, err)
	}
}

func TestClaudeFinalTextIsBoundedAndEmptyWhenClaudeRecordedNoContent(t *testing.T) {
	long := strings.Repeat("é", 20<<10)
	data := claudeResultArtifact(t,
		claudeRow("user", claudeUserRow, "", "", "Write a lot"),
		claudeRow("assistant", claudeFirst, claudeUserRow, "msg_one", claudeText(long)))
	text, truncated, err := FinalText(data, checkpointLimit)
	if err != nil || !truncated || len(text) > 32<<10 || !strings.HasPrefix(long, text) || text == "" {
		t.Fatalf("bounded final text: %d bytes truncated=%v: %v", len(text), truncated, err)
	}
	// Claude Code records an empty model reply as this placeholder.
	data = claudeResultArtifact(t,
		claudeRow("user", claudeUserRow, "", "", "Say nothing"),
		claudeRow("assistant", claudeFirst, claudeUserRow, "msg_one", claudeText("(no content)")))
	text, truncated, err = FinalText(data, checkpointLimit)
	if err != nil || truncated || text != "" {
		t.Fatalf("empty final text %q truncated=%v: %v", text, truncated, err)
	}
}
