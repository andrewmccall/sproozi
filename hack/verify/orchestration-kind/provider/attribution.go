//nolint:goconst // Keep literal JSON protocol and evidence fields reviewable.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

var casePattern = regexp.MustCompile(`SPROOZI_CASE=([A-Za-z0-9_-]+)`)
var namespacePattern = regexp.MustCompile(`SPROOZI_KUBERNETES_NAMESPACE=([a-z0-9-]+)`)

func requestCase(request map[string]any) string {
	encoded, _ := json.Marshal(request)
	match := casePattern.FindSubmatch(encoded)
	if len(match) == 2 {
		return string(match[1])
	}
	return ""
}
func namespace(request map[string]any) string {
	encoded, _ := json.Marshal(request)
	match := namespacePattern.FindSubmatch(encoded)
	if len(match) == 2 {
		return string(match[1])
	}
	if requestCase(request) == "hermes-kubernetes" {
		return "sproozi-kubernetes-proof"
	}
	return ""
}
func toolArguments(request map[string]any) map[string]any {
	if ns := namespace(request); ns != "" {
		return map[string]any{"namespace": ns}
	}
	return map[string]any{caseField: requestCase(request)}
}
func returnedToolResult(value any, expected string) bool {
	switch data := value.(type) {
	case map[string]any:
		if data["type"] == "function_call_output" || data["type"] == "custom_tool_call_output" ||
			data["type"] == "tool_result" || data["role"] == "tool" {
			encoded, _ := json.Marshal(data)
			return strings.Contains(string(encoded), expected)
		}
		for _, child := range data {
			if returnedToolResult(child, expected) {
				return true
			}
		}
	case []any:
		for _, child := range data {
			if returnedToolResult(child, expected) {
				return true
			}
		}
	}
	return false
}
func workerAnswer(request map[string]any) string {
	if namespace(request) != "" {
		return answer + " orchestration-observed"
	}
	return answer
}
