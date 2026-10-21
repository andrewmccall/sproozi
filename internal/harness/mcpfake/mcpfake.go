// Package mcpfake is a minimal Streamable HTTP MCP server for testing how a
// harness loads its rendered MCP configuration, without a gateway. Each
// connection is the path /mcp/<connection>; sproozi offers diagnose and every
// other connection offers list_issues. tools/call answers with the text
// DiagnoseResult or "called <connection>/<tool>".
package mcpfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	// Session is the Mcp-Session-Id every initialize returns.
	Session = "fake-session"
	// DiagnoseResult is the text sproozi's diagnose returns.
	DiagnoseResult = "fake diagnose: no recent refusals"
	protocol       = "2025-06-18"
	toolsField     = "tools"
)

// Handler serves every connection. log, when not nil, receives one line per
// request so a person can watch a real client connect.
func Handler(log io.Writer) http.Handler {
	var mu sync.Mutex
	logf := func(format string, args ...any) {
		if log != nil {
			mu.Lock()
			defer mu.Unlock()
			_, _ = fmt.Fprintf(log, format+"\n", args...)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if !ok || connection == "" || r.Method != http.MethodPost || json.Unmarshal(data, &request) != nil {
			logf("%s %s: refused", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		logf("%s: %s %s", connection, request.Method, request.Params.Name)
		if request.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		tool := "list_issues"
		if connection == "sproozi" {
			tool = "diagnose"
		}
		var result any
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", Session)
			result = map[string]any{"protocolVersion": protocol, "capabilities": map[string]any{toolsField: map[string]any{}},
				"serverInfo": map[string]any{"name": "sproozi-mcpfake", "version": "1"}}
		case "tools/list", "tools/call":
			if r.Header.Get("Mcp-Session-Id") != Session {
				http.Error(w, "missing Mcp-Session-Id from initialize", http.StatusBadRequest)
				return
			}
			if request.Method == "tools/list" {
				result = map[string]any{toolsField: []any{map[string]any{"name": tool,
					"inputSchema": map[string]any{"type": "object"}}}}
				break
			}
			if request.Params.Name != tool {
				http.Error(w, "unknown tool", http.StatusBadRequest)
				return
			}
			text := "called " + connection + "/" + tool
			if tool == "diagnose" {
				text = DiagnoseResult
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			http.Error(w, "unsupported method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": result})
	})
}
