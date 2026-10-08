// Provider wire fixtures intentionally spell protocol fields literally for review.
//
//nolint:goconst,lll // Constants and wrapping would obscure these fixed protocol payloads.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

func (f *fixture) model(w http.ResponseWriter, r *http.Request, selected string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	f.mu.Lock()
	f.requests++
	if r.Header.Get("Proxy-Authorization") != "" || strings.Contains(string(body), proxyToken) {
		f.violations = append(f.violations, "projected token reached upstream")
	}
	credential := r.Header.Get("Authorization")
	if selected == "claude-code" {
		credential = r.Header.Get("X-Api-Key")
	}
	if credential != providerKey && credential != "Bearer "+providerKey {
		f.violations = append(f.violations, "gateway provider credential missing")
	}
	f.mu.Unlock()
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if selected == "claude-code" {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": request["model"], "content": []any{map[string]any{"type": "text", "text": answer}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	modelName, _ := request["model"].(string)
	name := f.toolName(request)
	event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "content": []any{}, "model": modelName, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}}})
	if name != "" {
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_fixture", "name": name, "input": map[string]any{}}})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}})
		event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		event(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}})
		event(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": answer}})
	event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}})
	event(w, "message_stop", map[string]any{"type": "message_stop"})
}

func (f *fixture) openAI(w http.ResponseWriter, request map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	response := map[string]any{"id": "resp_fixture", "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": "fixture-model", "output": []any{}}
	event(w, "", map[string]any{"type": "response.created", "response": response})
	if name := f.toolName(request); name != "" {
		item := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture", "name": name, "arguments": "", "status": "in_progress"}
		event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		event(w, "", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_fixture", "output_index": 0, "delta": "{}"})
		event(w, "", map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_fixture", "output_index": 0, "arguments": "{}"})
		item["arguments"] = "{}"
		item["status"] = "completed"
		event(w, "", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		response["status"] = "completed"
		response["output"] = []any{item}
		response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
		event(w, "", map[string]any{"type": "response.completed", "response": response})
		return
	}
	item := map[string]any{"id": "msg_fixture", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
	event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	event(w, "", map[string]any{"type": "response.content_part.added", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "part": part})
	event(w, "", map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": answer})
	event(w, "", map[string]any{"type": "response.output_text.done", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "text": answer})
	part["text"] = answer
	item["status"] = "completed"
	item["content"] = []any{part}
	event(w, "", map[string]any{"type": "response.content_part.done", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "part": part})
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
	if toolResult(request) {
		f.mu.Lock()
		f.receivedToolResult = true
		f.mu.Unlock()
		return ""
	}
	tools, _ := request["tools"].([]any)
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		name, _ := tool["name"].(string)
		if strings.Contains(name, "fixture") && strings.Contains(name, "verify") {
			return name
		}
	}
	return ""
}

func toolResult(value any) bool {
	switch data := value.(type) {
	case map[string]any:
		if data["type"] == "function_call_output" || data["type"] == "tool_result" {
			encoded, _ := json.Marshal(data)
			return strings.Contains(string(encoded), answer)
		}
		for _, child := range data {
			if toolResult(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(data, toolResult)
	}
	return false
}
