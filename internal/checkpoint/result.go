package checkpoint

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// FinalText reads bounded untrusted workload output from a validated checkpoint.
// It does not establish successful execution, writer fencing or account usage.
func FinalText(data []byte, max int64) (string, bool, error) {
	if _, err := Inspect(data, max); err != nil {
		return "", false, err
	}
	a, err := decodeArtifact(data)
	if err != nil {
		return "", false, err
	}
	var text string
	if a.Codex != nil {
		for line := range bytes.SplitSeq(a.Codex.Rollout, []byte{'\n'}) {
			var row struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
					Last string `json:"last_agent_message"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "event_msg" && row.Payload.Type == "task_complete" {
				text = row.Payload.Last
			}
		}
	}
	if a.Copilot != nil {
		// The main agent's last answer that requested no tools.
		for line := range bytes.SplitSeq(a.Copilot.Events, []byte{'\n'}) {
			var row struct {
				Type string `json:"type"`
				Data struct {
					Content      string            `json:"content"`
					ToolRequests []json.RawMessage `json:"toolRequests"`
					Parent       string            `json:"parentToolCallId"`
				} `json:"data"`
			}
			if json.Unmarshal(line, &row) == nil && row.Type == "assistant.message" && row.Data.Parent == "" &&
				len(row.Data.ToolRequests) == 0 {
				text = row.Data.Content
			}
		}
	}
	if a.Claude != nil {
		// The reply at the transcript leaf the checkpoint selected.
		if text, err = harness.ClaudeFinalText(*a.Claude, max); err != nil {
			return "", false, err
		}
	}
	const limit = 32 << 10
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "")
		return text, true, nil
	}
	return text, false, nil
}
