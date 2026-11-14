package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

const claudeToolsWorkspace = "/workspace/session/workspace"

// claudeToolsFixture is a transcript the pinned stock client (2.1.318) wrote in
// its interactive terminal under the managed launch's tools, settings and
// hooks, stopped by killing its process group after Stop as the supervisor
// does. One turn ran a foreground Bash command and then an MCP tool. Host paths
// are replaced with the managed ones and opaque context payloads are elided;
// the chain, tool and metadata rows are otherwise as written.
func claudeToolsFixture(t *testing.T, name, id, sum string) ClaudeState {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != sum {
		t.Fatal("captured native transcript changed")
	}
	path, err := ClaudeProjectPath(claudeToolsWorkspace, id)
	if err != nil {
		t.Fatal(err)
	}
	return ClaudeState{ThreadID: id, Workspace: claudeToolsWorkspace, Path: path, Transcript: data}
}

func claudeMCPToolState(t *testing.T) ClaudeState {
	return claudeToolsFixture(t, "claude-2.1.318-mcp-tool.jsonl", "06156ccc-e04a-4750-a9b0-0fdfaa381b46",
		"ff7fc5199ed3917b6ce7b2ea8c28b000d5114b86d19347623a689d11975b59cf")
}

// The same turn with a Bash command that exited non-zero and an MCP tool that
// reported an error.
func claudeFailedToolsState(t *testing.T) ClaudeState {
	return claudeToolsFixture(t, "claude-2.1.318-failed-tools.jsonl", "1ee13846-2aec-4e50-b2b5-ea96addc98ca",
		"6d6f49e1cd4c9b3843dd31b3886f6a137638f74955ffb44324f741af7476ba62")
}

func TestClaudeStockTranscriptWithMCPAndFailedToolsSettles(t *testing.T) {
	for name, tc := range map[string]struct {
		state ClaudeState
		leaf  string
	}{
		"mcp tool":     {claudeMCPToolState(t), "35679228-da5b-4dc4-8b83-caab4177ee75"},
		"failed tools": {claudeFailedToolsState(t), "ad6eb5bc-8ae9-40c8-bd52-cfe0626431b6"},
	} {
		t.Run(name, func(t *testing.T) {
			leaf, err := ClaudeTranscriptLeaf(tc.state, 1<<20)
			if err != nil || leaf != tc.leaf {
				t.Fatalf("stock transcript: leaf=%s error=%v", leaf, err)
			}
			if text, err := ClaudeFinalText(tc.state, 1<<20); err != nil || text != "FINAL_ANSWER" {
				t.Fatalf("final text %q: %v", text, err)
			}
			home := t.TempDir()
			if err = RestoreClaudeState(home, claudeToolsWorkspace, tc.state, 1<<20); err != nil {
				t.Fatal(err)
			}
			restored, err := CaptureClaudeState(home, claudeToolsWorkspace, tc.state.ThreadID, 1<<20)
			if err != nil || !bytes.Equal(restored.Transcript, tc.state.Transcript) {
				t.Fatalf("native history changed: %v", err)
			}
		})
	}
}

func TestClaudeUnsupportedOrUnsettledToolsRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		state    func(*testing.T) ClaudeState
		old, new string
	}{
		"subagent tool": {claudeMCPToolState, `"name":"mcp__fixture__verify"`, `"name":"Task"`},
		"malformed mcp name": {claudeMCPToolState, `"name":"mcp__fixture__verify"`,
			`"name":"mcp____verify"`},
		"mcp input not an object": {claudeMCPToolState, `"name":"mcp__fixture__verify","input":{}`,
			`"name":"mcp__fixture__verify","input":[]`},
		"mcp result for another call": {claudeMCPToolState, `"tool_use_id":"toolu_`, `"tool_use_id":"other_`},
		"mcp result with no content": {claudeMCPToolState, `"toolUseResult":[{"type":"text","text":"FIXTURE_MARKER"}]`,
			`"toolUseResult":{"interrupted":true}`},
		"interrupted failure": {claudeFailedToolsState, `"content":"FIXTURE_FAILED"`,
			`"content":"[Request interrupted by user for tool use]"`},
		"failure without its error text": {claudeFailedToolsState, `"toolUseResult":"Error: FIXTURE_FAILED"`,
			`"toolUseResult":"FIXTURE_FAILED"`},
		"unsupported tool used": {claudeMCPToolState, `"tools":["Bash","mcp__fixture__verify"]`,
			`"tools":["Bash","Agent"]`},
		"no tools used": {claudeMCPToolState, `"tools":["Bash"]`, `"tools":[]`},
	} {
		t.Run(name, func(t *testing.T) {
			state := tc.state(t)
			changed := bytes.Replace(state.Transcript, []byte(tc.old), []byte(tc.new), 1)
			if bytes.Equal(changed, state.Transcript) {
				t.Fatal("negative control did not change the captured fixture")
			}
			state.Transcript = changed
			if _, err := ClaudeTranscriptLeaf(state, 1<<20); err == nil {
				t.Fatal("unsupported or unsettled tool accepted")
			}
		})
	}
	// The pending MCP call: the transcript ends at its tool_use row.
	state := claudeMCPToolState(t)
	end := bytes.Index(state.Transcript, []byte(`"name":"mcp__fixture__verify"`))
	state.Transcript = state.Transcript[:end+bytes.IndexByte(state.Transcript[end:], '\n')+1]
	if _, err := ClaudeTranscriptLeaf(state, 1<<20); err == nil {
		t.Fatal("transcript with an MCP call still running settled")
	}
}

func TestClaudeSupportedToolNames(t *testing.T) {
	for name, want := range map[string]bool{
		"Bash": true, "mcp__sproozi__diagnose": true, "mcp__my-server__a_tool": true,
		"mcp__a__b__c": true, "Task": false, "Agent": false, "Skill": false, "BashOutput": false,
		"mcp__": false, "mcp__a__": false, "mcp____b": false, "mcp__a_b__c": false, "mcp__a b__c": false,
		"mcp__a__c\n": false,
	} {
		if ClaudeSupportedTool(name) != want {
			t.Errorf("ClaudeSupportedTool(%q) = %v, want %v", name, !want, want)
		}
	}
}
