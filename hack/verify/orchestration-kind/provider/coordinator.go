// Native protocol fixtures use literal fields for review.
//
//nolint:goconst,lll // Keep the fixed JSON payloads readable.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The fixture chooses only tools actually advertised by Hermes. It does not
// call the task server itself: stock Hermes performs each native MCP call.
func (f *fixture) coordinator(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	encoded, _ := json.Marshal(request)
	marker := "SPROOZI_COORDINATOR_REQUEST="
	var submit map[string]any
	var reference map[string]any
	// Decode the request marker from a string, not from escaped JSON source.
	visit(request, func(m map[string]any) {
		for _, v := range m {
			if s, ok := v.(string); ok {
				if _, after, ok := strings.Cut(s, marker); ok {
					decoder := json.NewDecoder(strings.NewReader(after))
					_ = decoder.Decode(&submit)
				}
				if _, after, ok := strings.Cut(s, "SPROOZI_COORDINATOR_REFERENCE="); ok {
					decoder := json.NewDecoder(strings.NewReader(after))
					_ = decoder.Decode(&reference)
				}
			}
		}
	})
	var view map[string]any
	visitToolPayloads(request, func(m map[string]any) {
		if m["id"] != nil && m["uid"] != nil && m["phase"] != nil {
			view = m
		}
	})
	name := ""
	action := ""
	args := map[string]any{}
	if view == nil && reference != nil {
		action = "status"
		args = reference
	} else if view == nil && submit != nil {
		action = "submit"
		args = submit
	} else if view != nil && view["phase"] != "Succeeded" && view["phase"] != "Failed" && view["phase"] != "Cancelled" && view["phase"] != "TimedOut" {
		action = "wait"
		args = map[string]any{"id": view["id"], "uid": view["uid"], "seconds": 30}
	}
	if action != "" {
		name = findTool(request, action)
		if name == "" {
			name, args = hermesDeferredCall(request, args, func(candidate string) bool {
				return strings.Contains(candidate, "sproozi_tasks") && strings.HasSuffix(candidate, "__"+action)
			})
		}
	}
	text := "SPROOZI_COORDINATOR_COMPLETE"
	if view != nil {
		textBytes, _ := json.Marshal(view)
		text += " " + string(textBytes)
	}
	if name == "" && view == nil {
		text = "FIXTURE_ERROR: no advertised submit tool or marker; " + string(encoded[:min(len(encoded), 160)])
	}
	record := map[string]any{"client": "coordinator", "path": r.URL.Path,
		"model": request["model"], "action": name, "nativeTaskResult": view != nil}
	if view != nil {
		record["taskUID"] = view["uid"]
		record["phase"] = view["phase"]
	}
	f.mu.Lock()
	f.records = append(f.records, record)
	f.mu.Unlock()
	f.sendOpenAI(w, request, name, args, text, r.URL.Path)
}
func visit(value any, fn func(map[string]any)) {
	switch v := value.(type) {
	case map[string]any:
		fn(v)
		for _, c := range v {
			visit(c, fn)
		}
	case []any:
		for _, c := range v {
			visit(c, fn)
		}
	}
}
func findTool(request map[string]any, suffix string) string {
	tools, _ := request["tools"].([]any)
	for _, v := range tools {
		m, _ := v.(map[string]any)
		if child, ok := m["function"].(map[string]any); ok {
			m = child
		}
		n, _ := m["name"].(string)
		if n == suffix || strings.HasSuffix(n, "_"+suffix) {
			return n
		}
	}
	return ""
}
func (f *fixture) sendOpenAI(w http.ResponseWriter, request map[string]any, name string, args map[string]any,
	text, path string) {
	argument, _ := json.Marshal(args)
	id := fmt.Sprintf("fixture_%d", time.Now().UnixNano())
	if strings.Contains(path, "chat/completions") {
		message := map[string]any{"role": "assistant", "content": text}
		finish := "stop"
		if name != "" {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": string(argument)}}}
			finish = "tool_calls"
		}
		response := map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(),
			"model": request["model"], "choices": []any{map[string]any{"index": 0, "message": message,
				"finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
		if request["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"role": "assistant", "content": text}
		if name != "" {
			delta["content"] = nil
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": string(argument)}}}
		}
		event(w, "", map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": request["model"], "choices": []any{map[string]any{"index": 0, "delta": delta,
				"finish_reason": nil}}})
		event(w, "", map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": request["model"], "choices": []any{map[string]any{"index": 0, "delta": map[string]any{},
				"finish_reason": finish}}, "usage": response["usage"]})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	item := map[string]any{"id": id, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	if name != "" {
		item = map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name,
			"arguments": string(argument), "status": "completed"}
	}
	response := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": request["model"], "output": []any{item},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
	if request["stream"] != true {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event(w, "", map[string]any{"type": "response.created", "response": response})
	event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	event(w, "", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	event(w, "", map[string]any{"type": "response.completed", "response": response})
}
