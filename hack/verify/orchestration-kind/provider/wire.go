// Provider wire fixtures intentionally spell protocol fields literally for review.
//
//nolint:goconst,lll // Constants and wrapping would obscure these fixed protocol payloads.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func (f *fixture) model(w http.ResponseWriter, r *http.Request, selected string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	f.mu.Lock()
	f.requests++

	if r.Header.Get("Proxy-Authorization") != "" {
		f.violations = append(f.violations, "proxy authentication header reached upstream")
	}
	credential := r.Header.Get("Authorization")
	if selected == claudeClient {
		credential = r.Header.Get("X-Api-Key")
	}
	if credential != providerKey && credential != "Bearer "+providerKey {
		f.violations = append(f.violations, "gateway provider credential missing")
		f.records = append(f.records, map[string]any{"kind": "rejected-credential", "path": r.URL.Path,
			"method": r.Method, "credentialPresent": credential != "", "client": selected})
		f.mu.Unlock()
		http.Error(w, "provider credential required", http.StatusUnauthorized)
		return
	}
	f.mu.Unlock()
	if strings.Contains(string(body), "SPROOZI_HOLD") {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Minute):
			http.Error(w, "fixture hold expired", http.StatusGatewayTimeout)
			return
		}
	}
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	caseID := requestCase(request)
	expected := answer + " " + caseID
	if namespace(request) != "" {
		expected = "orchestration-observed"
	}
	consumed := returnedToolResult(request, expected)
	search := nativeToolSearch(request) && f.toolName(request) == "" && !consumed
	codeMode := nativeCodeMode(request) && !consumed
	if f.toolName(request) == "" && !consumed && !search && !codeMode {
		f.mu.Lock()
		diagnostic := string(body)
		truncated := len(diagnostic) > 32<<10
		if truncated {
			diagnostic = diagnostic[:32<<10]
		}
		rejected := map[string]any{"kind": "rejected-model", "client": selected,
			"path": r.URL.Path, "case": caseID, "requestBody": diagnostic, "truncated": truncated}
		f.records = append(f.records, rejected)
		// Acceptance bodies contain fixture prompts only; never log HTTP credential headers.
		_ = json.NewEncoder(os.Stderr).Encode(rejected)
		f.mu.Unlock()
		http.Error(w, "native approved MCP tool was not advertised or result was absent", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.records = append(f.records, map[string]any{"kind": "model", "client": selected, "path": r.URL.Path, "case": caseID, "toolResultConsumed": consumed, "nativeToolSearch": search, "nativeCodeMode": codeMode})
	f.mu.Unlock()
	if selected == claudeClient {
		f.anthropic(w, request)
	} else {
		f.openAI(w, request)
	}
}

func event(w io.Writer, name string, data any) {
	encoded, _ := json.Marshal(data)
	if name != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", name)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
}

func (f *fixture) anthropic(w http.ResponseWriter, request map[string]any) {
	if request["stream"] != true {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant",
			"model": request["model"], "content": []any{map[string]any{"type": "text", "text": workerAnswer(request)}},
			"stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	modelName, _ := request["model"].(string)
	name := f.toolName(request)
	arguments, _ := json.Marshal(modelToolArguments(request))
	event(w, "message_start", map[string]any{"type": "message_start",
		"message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "content": []any{},
			"model": modelName, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10,
				"output_tokens": 0}}})
	if name != "" {
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_fixture", "name": name, "input": map[string]any{}}})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(arguments)}})
		event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		event(w, "message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 5}})
		event(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": workerAnswer(request)}})
	event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event(w, "message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 5}})
	event(w, "message_stop", map[string]any{"type": "message_stop"})
}

func (f *fixture) openAI(w http.ResponseWriter, request map[string]any) {
	if request["stream"] != true {
		name := f.toolName(request)
		f.sendOpenAI(w, request, name, modelToolArguments(request), workerAnswer(request), "/responses")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	response := map[string]any{"id": "resp_fixture", "object": "response", "created_at": time.Now().Unix(),
		"status": "in_progress", "model": "fixture-model", "output": []any{}}
	event(w, "", map[string]any{"type": "response.created", "response": response})
	if nativeCodeMode(request) && !returnedToolResult(request, answer+" "+requestCase(request)) {
		arguments, _ := json.Marshal(modelToolArguments(request))
		code := `const tool = ALL_TOOLS.find(t => t.name.includes("fixture") && t.name.includes("verify")); ` +
			`if (!tool) throw new Error("Approved fixture MCP tool was not discovered"); ` +
			`text(await tools[tool.name](` + string(arguments) + `));`
		item := map[string]any{"id": "ctc_fixture", "type": "custom_tool_call", "call_id": "exec_fixture",
			"status": "completed", "namespace": "functions", "name": "exec", "input": code}
		event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		event(w, "", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		response["status"] = "completed"
		response["output"] = []any{item}
		response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
		event(w, "", map[string]any{"type": "response.completed", "response": response})
		return
	}
	if nativeToolSearch(request) && f.toolName(request) == "" {
		item := map[string]any{"id": "tsc_fixture", "type": "tool_search_call", "call_id": "search_fixture",
			"status": "completed", "execution": "client", "arguments": map[string]any{"query": "fixture verify", "limit": 1}}
		event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		event(w, "", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		response["status"] = "completed"
		response["output"] = []any{item}
		response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
		event(w, "", map[string]any{"type": "response.completed", "response": response})
		return
	}
	if name := f.toolName(request); name != "" {
		arguments, _ := json.Marshal(modelToolArguments(request))
		item := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture", "name": name,
			"arguments": "", "status": "in_progress"}
		if ns := approvedToolNamespace(request, name); ns != "" {
			item["namespace"] = ns
		}
		event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		event(w, "", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_fixture",
			"output_index": 0, "delta": string(arguments)})
		event(w, "", map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_fixture",
			"output_index": 0, "arguments": string(arguments)})
		item["arguments"] = string(arguments)
		item["status"] = "completed"
		event(w, "", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		response["status"] = "completed"
		response["output"] = []any{item}
		response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
		event(w, "", map[string]any{"type": "response.completed", "response": response})
		return
	}
	item := map[string]any{"id": "msg_fixture", "type": "message", "status": "in_progress", "role": "assistant",
		"content": []any{}}
	event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	event(w, "", map[string]any{"type": "response.content_part.added", "item_id": "msg_fixture", "output_index": 0,
		"content_index": 0, "part": part})
	event(w, "", map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0,
		"content_index": 0, "delta": workerAnswer(request)})
	event(w, "", map[string]any{"type": "response.output_text.done", "item_id": "msg_fixture", "output_index": 0,
		"content_index": 0, "text": workerAnswer(request)})
	part["text"] = workerAnswer(request)
	item["status"] = "completed"
	item["content"] = []any{part}
	event(w, "", map[string]any{"type": "response.content_part.done", "item_id": "msg_fixture", "output_index": 0,
		"content_index": 0, "part": part})
	event(w, "", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	response["status"] = "completed"
	response["output"] = []any{item}
	response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
	event(w, "", map[string]any{"type": "response.completed", "response": response})
}

func (f *fixture) toolName(request map[string]any) string {
	if !f.tool {
		return ""
	}
	expected := answer + " " + requestCase(request)
	if namespace(request) != "" {
		expected = "orchestration-observed"
	}
	if returnedToolResult(request, expected) {
		f.mu.Lock()
		f.receivedToolResult = true
		f.mu.Unlock()
		return ""
	}
	name, _ := approvedTool(request)
	if name == "" {
		name, _ = workerDeferredCall(request)
	}
	return name
}
