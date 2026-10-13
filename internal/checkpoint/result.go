package checkpoint

import (
	"bytes"
	"encoding/json"
	"strings"
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
	// Claude final-answer extraction stays separate from its completion observer.
	if a.Claude != nil {
		return "", false, nil
	}
	const limit = 32 << 10
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "")
		return text, true, nil
	}
	return text, false, nil
}
