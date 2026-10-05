/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package execution

import (
	"bytes"
	"encoding/json"
)

// CodexEvent is a single structured event from the Codex event stream.
// Only the event type is retained — raw payloads (tool inputs, outputs,
// file contents) are discarded for audit safety.
type CodexEvent struct {
	// Type identifies the event kind (e.g. "agent_start", "tool_call", "agent_end").
	Type string `json:"type"`
}

// ParseEvents parses the event stream from raw Codex output bytes.
// It returns one CodexEvent per JSON line that has a non-empty "type" field.
// Lines that are not valid JSON objects or lack a "type" field are silently skipped.
func ParseEvents(raw []byte) []CodexEvent {
	if len(raw) == 0 {
		return nil
	}
	var events []CodexEvent
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e CodexEvent
		if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
			continue
		}
		events = append(events, e)
	}
	return events
}
