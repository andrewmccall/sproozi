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
package execution_test

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/execution"
)

func TestParseEventsReturnsTypes(t *testing.T) {
	raw := []byte(`{"type":"agent_start"}
{"type":"tool_call","tool":"bash","input":"kubectl get pods","output":"NAME   READY   STATUS"}
{"type":"agent_end"}`)
	events := execution.ParseEvents(raw)
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	if events[0].Type != "agent_start" {
		t.Errorf("events[0].Type = %q", events[0].Type)
	}
	if events[2].Type != "agent_end" {
		t.Errorf("events[2].Type = %q", events[2].Type)
	}
}

func TestParseEventsSkipsNonJSON(t *testing.T) {
	raw := []byte(`not json
{"type":"tool_call"}
also not json`)
	events := execution.ParseEvents(raw)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
}

func TestParseEventsEmpty(t *testing.T) {
	events := execution.ParseEvents(nil)
	if len(events) != 0 {
		t.Errorf("want 0 events, got %d", len(events))
	}
}

func TestParseEventsNoPayloadRetained(t *testing.T) {
	// Verify only Type is exposed — not the raw tool input/output.
	raw := []byte(`{"type":"tool_call","tool":"bash","input":"secret-token echo","output":"secret-value"}`)
	events := execution.ParseEvents(raw)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	// Only Type should be accessible — payload fields should not be in the struct.
	if events[0].Type != "tool_call" {
		t.Errorf("Type = %q", events[0].Type)
	}
}
