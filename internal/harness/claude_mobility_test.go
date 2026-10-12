package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

const claudeMobilityID = "1b5815b2-a537-4f86-a591-39405edfdd85"
const claudeMobilityWorkspace = "/workspace/session/workspace"
const claudeMobilityLeaf = "9fb36aec-a07e-4849-8d15-fa85b00e1940"

func claudeMobilityFixture(t *testing.T) ClaudeState {
	t.Helper()
	// Exact production Docker -> Kubernetes selected transcript, captured after
	// a genuine foreground Bash effect and Stop. All content is fixture data.
	data, err := os.ReadFile("testdata/claude-2.1.27-mobility-retry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != "dc7422b4707e2757a73a87bc53592d707705a1b49da7fc8047079766169e9576" {
		t.Fatal("captured native transcript changed")
	}
	path, err := ClaudeProjectPath(claudeMobilityWorkspace, claudeMobilityID)
	if err != nil {
		t.Fatal(err)
	}
	return ClaudeState{ThreadID: claudeMobilityID, Workspace: claudeMobilityWorkspace, Path: path, Transcript: data}
}

func TestClaudeActualMobilityTranscriptAfterRecoveredRetry(t *testing.T) {
	state := claudeMobilityFixture(t)
	leaf, err := ClaudeTranscriptLeaf(state, 16384)
	if err != nil || leaf != claudeMobilityLeaf {
		t.Fatalf("actual recovered native turn: leaf=%s error=%v", leaf, err)
	}
	rows := bytes.Split(state.Transcript, []byte{'\n'})
	// The API retry is row 11; it cannot reuse the first turn's assistant leaf.
	state.Transcript = append(bytes.Join(rows[:12], []byte{'\n'}), '\n')
	if _, err = ClaudeTranscriptLeaf(state, 16384); err == nil {
		t.Fatal("API retry reused an earlier completed assistant leaf")
	}
}

func TestClaudeRecoveredRetryShapeAndOpaqueMessage(t *testing.T) {
	original := claudeMobilityFixture(t)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"opaque error message", func(row map[string]any) {
			claudeRetryDetail(row)["message"] = "Opaque native provider diagnostic"
		}, true},
		{"missing attempt", func(row map[string]any) { delete(row, "retryAttempt") }, false},
		{"zero attempt", func(row map[string]any) { row["retryAttempt"] = 0 }, false},
		{"exhausted attempts", func(row map[string]any) { row["retryAttempt"] = 11 }, false},
		{"unknown retry limit", func(row map[string]any) { row["maxRetries"] = 11 }, false},
		{"missing retry delay", func(row map[string]any) { delete(row, "retryInMs") }, false},
		{"negative retry delay", func(row map[string]any) { row["retryInMs"] = -1 }, false},
		{"excessive retry delay", func(row map[string]any) { row["retryInMs"] = 60001 }, false},
		{"unknown error type", func(row map[string]any) { claudeRetryDetail(row)["type"] = "unknown_error" }, false},
		{"unknown status", func(row map[string]any) { row["error"].(map[string]any)["status"] = 503 }, false},
		{"unknown error fields", func(row map[string]any) { claudeRetryDetail(row)["extra"] = true }, false},
		{"unknown system subtype", func(row map[string]any) { row["subtype"] = "unknown_system" }, false},
		{"foreign primary parent", func(row map[string]any) { row["parentUuid"] = claudeMobilityID }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := bytes.Split(original.Transcript, []byte{'\n'})
			var row map[string]any
			if err := json.Unmarshal(rows[11], &row); err != nil {
				t.Fatal(err)
			}
			tc.change(row)
			data, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			rows[11] = data
			state := original
			state.Transcript = bytes.Join(rows, []byte{'\n'})
			leaf, err := ClaudeTranscriptLeaf(state, 16384)
			if tc.valid {
				if err != nil || leaf != claudeMobilityLeaf {
					t.Fatalf("opaque diagnostic changed continuity: leaf=%s error=%v", leaf, err)
				}
			} else if err == nil {
				t.Fatal("unsupported native retry record accepted")
			}
		})
	}
}

func claudeRetryDetail(row map[string]any) map[string]any {
	return row["error"].(map[string]any)["error"].(map[string]any)["error"].(map[string]any)
}
