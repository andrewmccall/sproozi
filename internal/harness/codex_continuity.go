package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ValidateCodexContinuity validates the supported completed-turn shape, initial
// approved developer block and resolved supported tool activity. It supplies no
// authority and is never evidence that every native/workspace writer stopped.
func ValidateCodexContinuity(state CodexState, instructionDigest string, maxBytes int64) error {
	if _, err := CompletedCodexTurn(state, maxBytes); err != nil {
		return err
	}
	return validateCodexToolsAndInstructions(state.Rollout, instructionDigest)
}

func validateCodexToolsAndInstructions(data []byte, digest string) error {
	pending := map[string]bool{}
	instructionFound, initialBlock := false, true
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				CallID  string `json:"call_id"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		p := row.Payload
		if row.Type == "response_item" {
			if p.Type == "message" && p.Role == "developer" && initialBlock {
				for _, c := range p.Content {
					if fmt.Sprintf("%x", sha256.Sum256([]byte(c.Text))) == digest {
						instructionFound = true
					}
				}
			}
			if p.Type == "message" && (p.Role == claudeUser || p.Role == "assistant") {
				initialBlock = false
			}
			switch p.Type {
			case "function_call", "custom_tool_call":
				if p.CallID == "" {
					return errors.New("native tool call has no identity")
				}
				pending["response:"+p.CallID] = true
			case "function_call_output", "custom_tool_call_output":
				if p.CallID == "" || !pending["response:"+p.CallID] {
					return errors.New("unmatched native tool output")
				}
				delete(pending, "response:"+p.CallID)
			default:
				if strings.HasSuffix(p.Type, "_call") {
					return errors.New("unsupported native tool call")
				}
			}
		}
		if row.Type == "event_msg" {
			switch p.Type {
			case "exec_command_begin", "mcp_tool_call_begin", "patch_apply_begin":
				if p.CallID == "" {
					return errors.New("native tool activity has no identity")
				}
				pending["event:"+p.CallID] = true
			case "exec_command_end", "mcp_tool_call_end", "patch_apply_end":
				if p.CallID == "" || !pending["event:"+p.CallID] {
					return errors.New("unmatched native tool activity")
				}
				delete(pending, "event:"+p.CallID)
			}
		}
	}
	if !instructionFound {
		return errors.New("native initial instructions do not match trusted launch")
	}
	if len(pending) > 0 {
		return errors.New("native tools remain pending")
	}
	return nil
}
