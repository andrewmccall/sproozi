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
	// Copilot's model list, model choice and account requests aren't model
	// calls; they are checked for the credential but not counted.
	metadata := selected == copilotGitHub && (r.Method == http.MethodGet || r.URL.Path == "/auto")
	if !metadata {
		f.requests++
	}
	if r.Header.Get("Proxy-Authorization") != "" || strings.Contains(string(body), proxyToken) {
		f.violations = append(f.violations, "projected token reached upstream")
	}
	credential, expected := r.Header.Get("Authorization"), providerKey
	if selected == "claude-code" {
		credential = r.Header.Get("X-Api-Key")
	}
	if selected == copilotGitHub {
		// The gateway forwards Copilot's own GitHub sign-in.
		expected = copilotFixtureToken
	}
	if credential != expected && credential != "Bearer "+expected {
		f.violations = append(f.violations, "gateway provider credential missing")
	}
	f.mu.Unlock()
	if selected == copilotGitHub && f.copilotMetadata(w, r) {
		return
	}
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
	name, input := f.toolCall(request)
	messageID := fmt.Sprintf("msg_fixture_%d", time.Now().UnixNano())
	toolID := fmt.Sprintf("toolu_fixture_%d", time.Now().UnixNano())
	encodedInput, _ := json.Marshal(input)
	if request["stream"] != true {
		content := []any{map[string]any{"type": "text", "text": answer}}
		stop := "end_turn"
		if name != "" {
			content = []any{map[string]any{"type": "tool_use", "id": toolID, "name": name, "input": input}}
			stop = "tool_use"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": messageID, "type": "message", "role": "assistant", "model": request["model"], "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	modelName, _ := request["model"].(string)
	event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": modelName, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}}})
	if name != "" {
		event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": toolID, "name": name, "input": map[string]any{}}})
		event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(encodedInput)}})
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
	if name, input := f.toolCall(request); name != "" {
		encoded, _ := json.Marshal(input)
		arguments := string(encoded)
		callID := fmt.Sprintf("call_fixture_%d", time.Now().UnixNano())
		item := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": callID, "name": name, "arguments": "", "status": "in_progress"}
		event(w, "", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		event(w, "", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_fixture", "output_index": 0, "delta": arguments})
		event(w, "", map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_fixture", "output_index": 0, "arguments": arguments})
		item["arguments"] = arguments
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

// toolCall scripts the next model turn: the success scenario calls the
// fixture verify tool; the diagnose scenario fetches a destination the gateway
// refuses, then asks the sproozi control connection why.
func (f *fixture) toolCall(request map[string]any) (string, map[string]any) {
	if !f.tool {
		return "", nil
	}
	results := toolResults(request)
	if f.scenario == diagnoseScenario {
		return f.diagnoseCall(request, results)
	}
	if slices.ContainsFunc(results, func(r string) bool { return strings.Contains(r, answer) }) {
		f.mu.Lock()
		f.receivedToolResult = true
		f.mu.Unlock()
		return "", nil
	}
	if name := matchingTool(request, "fixture", "verify"); name != "" {
		return name, map[string]any{}
	}
	if offeredTool(request, "ToolSearch") != "" {
		return "ToolSearch", map[string]any{"query": "select:mcp__fixture__verify", "max_results": 1}
	}
	return "", nil
}

func (f *fixture) diagnoseCall(request map[string]any, results []string) (string, map[string]any) {
	// Any diagnose result ends the turn, so not_observed fails acceptance
	// instead of looping.
	for _, r := range results {
		if plain := strings.ReplaceAll(r, `\`, ""); strings.Contains(plain, `"status":"`) {
			f.mu.Lock()
			f.receivedToolResult = true
			f.diagnosed = strings.Contains(plain, `"status":"denied"`) && strings.Contains(plain, deniedHost)
			f.mu.Unlock()
			return "", nil
		}
	}
	if len(results) == 0 {
		shell := offeredTool(request, "Bash", "bash")
		if shell == "" {
			return "", nil
		}
		return shell, map[string]any{"description": "Fetch the documentation",
			"command": `curl -sS --max-time 10 --proxy-cacert "$SSL_CERT_FILE" https://` + deniedHost + `/`}
	}
	if name := matchingTool(request, "sproozi", "diagnose"); name != "" {
		return name, map[string]any{"destination": deniedHost}
	}
	if offeredTool(request, "ToolSearch") != "" && len(results) == 1 {
		return "ToolSearch", map[string]any{"query": "select:mcp__sproozi__diagnose", "max_results": 1}
	}
	return "", nil
}

// offeredTool returns the first offered tool named exactly one of names.
func offeredTool(request map[string]any, names ...string) string {
	tools, _ := request["tools"].([]any)
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if name, _ := tool["name"].(string); slices.Contains(names, name) {
			return name
		}
	}
	return ""
}

// matchingTool returns the first offered tool whose name contains every part.
func matchingTool(request map[string]any, parts ...string) string {
	tools, _ := request["tools"].([]any)
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		name, _ := tool["name"].(string)
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(name, part) }) {
			return name
		}
	}
	return ""
}

// toolResults returns each tool result block in the request, JSON encoded.
func toolResults(value any) []string {
	switch data := value.(type) {
	case map[string]any:
		if data["type"] == "function_call_output" || data["type"] == "tool_result" {
			encoded, _ := json.Marshal(data)
			return []string{string(encoded)}
		}
		var found []string
		for _, child := range data {
			found = append(found, toolResults(child)...)
		}
		return found
	case []any:
		var found []string
		for _, child := range data {
			found = append(found, toolResults(child)...)
		}
		return found
	}
	return nil
}

// copilotMetadata answers the model list and automatic model choice a
// GitHub-signed-in Copilot asks for before its model call.
func (f *fixture) copilotMetadata(w http.ResponseWriter, r *http.Request) bool {
	var body any
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/models":
		body = map[string]any{"object": "list", "data": []any{map[string]any{
			"id": fixtureModel, "name": "Fixture", "object": "model", "vendor": "OpenAI", "version": fixtureModel,
			"preview": false, "model_picker_enabled": true, "is_chat_default": true, "is_chat_fallback": true,
			"supported_endpoints": []string{"/responses"},
			"policy":              map[string]any{"state": "enabled", "terms": ""},
			"capabilities": map[string]any{"family": "gpt-5", "object": "model_capabilities", "type": "chat",
				"tokenizer": "o200k_base",
				"limits": map[string]any{"max_context_window_tokens": 200000, "max_prompt_tokens": 128000,
					"max_output_tokens": 8192, "max_non_streaming_output_tokens": 8192},
				"supports": map[string]any{"streaming": true, "tool_calls": true, "parallel_tool_calls": true,
					"structured_outputs": true, "vision": false}}}}}
	case r.Method == http.MethodPost && r.URL.Path == "/auto":
		body = map[string]any{"model": fixtureModel}
	case r.Method == http.MethodGet && r.URL.Path == "/copilot_internal/user":
		// The fields Copilot reads, with the policy flags of a free plan.
		body = map[string]any{"login": "fixture", "copilot_plan": "individual", "access_type_sku": "free_limited_copilot",
			"chat_enabled": true, "cli_enabled": true, "is_mcp_enabled": true,
			"endpoints": map[string]any{"api": "https://api.individual.githubcopilot.com",
				"telemetry": "https://telemetry.individual.githubcopilot.com",
				"proxy":     "https://proxy.individual.githubcopilot.com"}}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/copilot_internal/"):
		w.WriteHeader(http.StatusNotFound)
		return true
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
	return true
}
