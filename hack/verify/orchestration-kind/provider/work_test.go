//nolint:goconst,lll // Keep native protocol fields literal.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func workResponse(t *testing.T, request map[string]any) (map[string]any, *fixture) {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{}
	response := httptest.NewRecorder()
	f.coordinator(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(data))))
	var out map[string]any
	if err = json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	choices := out["choices"].([]any)
	return choices[0].(map[string]any)["message"].(map[string]any), f
}

func TestHermesWorkDispatchUsesAdvertisedNativeTool(t *testing.T) {
	intent := `{"requestId":"fixture-request","repository":"sproozi","harness":"codex","executor":"remote","task":"A task"}`
	request := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "SPROOZI_WORK_REQUEST=" + intent}},
		"tools":    []any{map[string]any{"function": map[string]any{"name": "mcp__sproozi_work__sproozi_run"}}},
	}
	message, _ := workResponse(t, request)
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("No native work invocation: %#v", message)
	}
	call := calls[0].(map[string]any)["function"].(map[string]any)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call["arguments"].(string)), &arguments); err != nil {
		t.Fatal(err)
	}
	if call["name"] != "mcp__sproozi_work__sproozi_run" || arguments["task"] != "A task" || arguments["requestId"] != "fixture-request" {
		t.Fatalf("Changed native intent: %#v", call)
	}
	request["tools"] = []any{}
	message, _ = workResponse(t, request)
	if message["tool_calls"] != nil || !strings.HasPrefix(message["content"].(string), "FIXTURE_ERROR:") {
		t.Fatalf("Invented unavailable work tool: %#v", message)
	}
}

func TestHermesWorkObservationRequiresActualToolResult(t *testing.T) {
	view := `{"id":"session","executionId":"execution","phase":"retained","threadId":"native"}`
	request := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "SPROOZI_WORK_REFERENCE={\"id\":\"session\"} " + view},
	}, "tools": []any{map[string]any{"function": map[string]any{"name": "mcp__sproozi_work__sproozi_status"}}}}
	message, _ := workResponse(t, request)
	if message["tool_calls"] == nil {
		t.Fatalf("Prompt text fabricated retained work: %#v", message)
	}
	request["messages"] = append(request["messages"].([]any), map[string]any{"role": "tool", "content": view})
	message, f := workResponse(t, request)
	if message["content"] != `SPROOZI_WORK_OBSERVED {"executionId":"execution","id":"session","phase":"retained","threadId":"native"}` || message["tool_calls"] != nil ||
		f.records[0]["sessionId"] != "session" || f.records[0]["executionId"] != "execution" {
		t.Fatalf("Missing actual work result/identity: %#v, %#v", message, f.records)
	}
}
