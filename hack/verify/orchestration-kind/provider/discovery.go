//nolint:goconst // Spell the native Responses tool discovery protocol literally.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

var hermesCatalogName = regexp.MustCompile(`(?m)^- (mcp__[A-Za-z0-9_]+):`)

// Hermes 0.21.6 advertises deferred MCP names in the native search bridge's
// catalog. Describe the visible name first, then invoke only after the real
// bridge returned its schema. Never infer a tool from user prompt text.
func hermesDeferredCall(
	request map[string]any, arguments map[string]any, approved func(string) bool,
) (string, map[string]any) {
	tools, _ := request["tools"].([]any)
	describe, call, target := false, false, ""
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if fn, ok := tool["function"].(map[string]any); ok {
			tool = fn
		}
		switch tool["name"] {
		case "tool_describe":
			describe = true
		case "tool_call":
			call = true
		case "hermes_tool_search", "tool_search":
			catalog, _ := tool["description"].(string)
			for _, match := range hermesCatalogName.FindAllStringSubmatch(catalog, -1) {
				if approved(match[1]) && target == "" {
					target = match[1]
				}
			}
		}
	}
	if target == "" || !describe || !call {
		return "", nil
	}
	described := false
	visitToolPayloads(request, func(document map[string]any) {
		tools, _ := document["tools"].(map[string]any)
		definition, _ := tools[target].(map[string]any)
		if _, ok := definition["parameters"].(map[string]any); ok {
			described = true
		}
	})
	if !described {
		return "tool_describe", map[string]any{"names": []string{target}}
	}
	return "tool_call", map[string]any{"calls": []any{map[string]any{"name": target, "arguments": arguments}}}
}

func workerDeferredCall(request map[string]any) (string, map[string]any) {
	return hermesDeferredCall(request, toolArguments(request), func(name string) bool {
		if namespace(request) != "" {
			return strings.Contains(name, "kubernetes_list_pods")
		}
		return strings.Contains(name, "fixture") && strings.HasSuffix(name, "__verify")
	})
}

func modelToolArguments(request map[string]any) map[string]any {
	if name, arguments := workerDeferredCall(request); name != "" {
		return arguments
	}
	return toolArguments(request)
}

// Stock Codex 0.161 defaults to code mode. Its actual enabled executor schema
// is an additional_tools input item; MCP metadata stays in native ALL_TOOLS.
func nativeCodeMode(request map[string]any) bool {
	input, _ := request["input"].([]any)
	enabled := false
	for _, value := range input {
		item, _ := value.(map[string]any)
		if item["type"] == "custom_tool_call_output" {
			return false // A failed native executor must not retry to fabricate success.
		}
		if item["type"] != "additional_tools" {
			continue
		}
		tools, _ := item["tools"].([]any)
		for _, value := range tools {
			ns, _ := value.(map[string]any)
			if ns["type"] != "namespace" || ns["name"] != "functions" {
				continue
			}
			children, _ := ns["tools"].([]any)
			for _, child := range children {
				tool, _ := child.(map[string]any)
				if tool["type"] == "custom" && tool["name"] == "exec" {
					enabled = true
				}
			}
		}
	}
	return enabled
}

// nativeToolSearch follows Codex 0.161's mandatory client-side deferred discovery.
// A search that exposes no approved tool must fail rather than fabricate success.
func nativeToolSearch(request map[string]any) bool {
	searched := false
	visit(request["input"], func(item map[string]any) {
		if item["type"] == "tool_search_output" {
			searched = true
		}
	})
	if searched {
		return false
	}
	tools, _ := request["tools"].([]any)
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if tool["type"] == "tool_search" && tool["execution"] == "client" {
			return true
		}
	}
	return false
}

func approvedTool(request map[string]any) (name, selectedNamespace string) {
	var scan func(any, string)
	scan = func(value any, ns string) {
		tools, _ := value.([]any)
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			if tool["type"] == "namespace" {
				childNamespace, _ := tool["name"].(string)
				scan(tool["tools"], childNamespace)
				continue
			}
			if child, ok := tool["function"].(map[string]any); ok {
				tool = child
			}
			candidate, _ := tool["name"].(string)
			qualified := ns + " " + candidate
			approved := namespace(request) != "" && strings.Contains(qualified, "kubernetes_list_pods") ||
				namespace(request) == "" && strings.Contains(qualified, "fixture") && strings.Contains(candidate, "verify")
			if approved && name == "" {
				name, selectedNamespace = candidate, ns
			}
		}
	}
	scan(request["tools"], "")
	// Only native discovery output may introduce a schema, never arbitrary prompt data.
	visit(request["input"], func(item map[string]any) {
		if item["type"] == "tool_search_output" {
			scan(item["tools"], "")
		}
	})
	return name, selectedNamespace
}

func approvedToolNamespace(request map[string]any, name string) string {
	selectedName, ns := approvedTool(request)
	if selectedName == name {
		return ns
	}
	return ""
}

// Read data only from native tool results. Hermes wraps external MCP text with
// a warning and untrusted_tool_result delimiters before sending it to the model.
// The fixture consumes that data; the native transcript and wrapper stay intact.
func visitToolPayloads(request map[string]any, fn func(map[string]any)) {
	var decode func(any)
	decode = func(value any) {
		switch data := value.(type) {
		case string:
			if strings.HasPrefix(data, "<untrusted_tool_result ") {
				_, payload, ok := strings.Cut(data, "\n\n")
				if !ok || !strings.HasSuffix(payload, "\n</untrusted_tool_result>") {
					return
				}
				data = strings.TrimSuffix(payload, "\n</untrusted_tool_result>")
			}
			var parsed any
			if json.Unmarshal([]byte(data), &parsed) == nil {
				decode(parsed)
			}
		case map[string]any:
			fn(data)
			for _, child := range data {
				decode(child)
			}
		case []any:
			for _, child := range data {
				decode(child)
			}
		}
	}
	visit(request, func(item map[string]any) {
		if item["type"] == "function_call_output" || item["role"] == "tool" {
			decode(item["output"])
			decode(item["content"])
		}
	})
}
