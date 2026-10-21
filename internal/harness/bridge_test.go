package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// toolsKey is the MCP tools capability and result field.
const toolsKey = "tools"

// fakeMCPServer answers Streamable HTTP initialize and tools/list with one
// tool per connection: diagnose for sproozi, list_issues otherwise.
func fakeMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		data, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || json.Unmarshal(data, &request) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fake-session")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{toolsKey: map[string]any{}},
				"serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "fake-session" {
				http.Error(w, "missing session", http.StatusBadRequest)
				return
			}
			tool := "list_issues"
			if strings.HasSuffix(r.URL.Path, "/sproozi") {
				tool = "diagnose"
			}
			result = map[string]any{toolsKey: []any{map[string]any{"name": tool, "inputSchema": map[string]any{"type": "object"}}}}
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server
}

// checkMCPBridge is the test hook for a harness that loads MCP through a
// bridge: it renders the harness's configuration against a fake MCP server,
// runs the bridge's --sproozi-check mode, and requires every connection's
// tools, named <connection>__<tool>, and a refusal without sproozi. It skips
// when the bridge's runtime is not installed.
func checkMCPBridge(t *testing.T, h Harness, bridge string) {
	t.Helper()
	for _, problem := range bridgeProblems(t, h, bridge) {
		t.Error(problem)
	}
}

func bridgeProblems(t *testing.T, h Harness, bridge string) []string {
	t.Helper()
	runtime, err := exec.LookPath(h.Loader.Runtime[0])
	if err != nil {
		t.Skipf("harness %q: bridge runtime %q is not on PATH, so its bridge was not run; install it to test the bridge",
			h.Selector, h.Loader.Runtime[0])
	}
	server := fakeMCPServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, h.Loader.Bridge)
	if err = os.WriteFile(path, []byte(bridge), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(names ...string) (string, error) {
		files, err := h.MCP.render(names, func(name string) string { return server.URL + "/mcp/" + name })
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(dir, h.MCP.File)
		if err = os.WriteFile(config, []byte(files[h.MCP.File]), 0600); err != nil {
			t.Fatal(err)
		}
		args := append(slices.Clone(h.Loader.Runtime[1:]), path, "--sproozi-check", config)
		out, err := exec.CommandContext(t.Context(), runtime, args...).Output()
		return string(out), err
	}
	var problems []string
	out, err := run("github", "sproozi")
	var report struct{ Tools []string }
	if err != nil || json.Unmarshal([]byte(out), &report) != nil {
		problems = append(problems, fmt.Sprintf("harness %q: bridge %s --sproozi-check CONFIG must print "+
			`{"tools":[...]}`+" (%v): got %q; see Writing an MCP bridge in docs/contributing/add-a-harness.md",
			h.Selector, h.Loader.Bridge, err, out))
	}
	for _, want := range []string{"github__list_issues", "sproozi__diagnose"} {
		if !slices.Contains(report.Tools, want) {
			problems = append(problems, fmt.Sprintf("harness %q: bridge %s did not load %s from %s; it reported %v",
				h.Selector, h.Loader.Bridge, want, h.MCP.File, report.Tools))
		}
	}
	if _, err = run("github"); err == nil {
		problems = append(problems, fmt.Sprintf("harness %q: bridge %s started without the sproozi control connection",
			h.Selector, h.Loader.Bridge))
	}
	return problems
}

// Every registered bridge loads its rendered configuration.
func TestEveryMCPBridgeLoadsItsConfiguration(t *testing.T) {
	for _, h := range registry {
		if h.Loader.Bridge != "" {
			t.Run(h.Selector, func(t *testing.T) { checkMCPBridge(t, h, h.LaunchFiles[h.Loader.Bridge]) })
		}
	}
}

// The hook itself: the example bridge in testdata passes, and a bridge that
// loads nothing fails.
func TestExampleMCPBridgePassesTheHook(t *testing.T) {
	bridge, err := os.ReadFile("testdata/example-mcp-bridge.mjs")
	if err != nil {
		t.Fatal(err)
	}
	example := Harness{Selector: "example", Loader: MCPLoader{Bridge: "example-mcp-bridge.mjs", Runtime: []string{"node"}},
		MCP: MCPFormat{File: "example-mcp.json", Field: mcpServersField, Entry: func(name, address string) (string, any) {
			return name, map[string]any{connectionURL: address}
		}}}
	checkMCPBridge(t, example, string(bridge))
	if problems := bridgeProblems(t, example, `console.log('{"tools":[]}')`); len(problems) != 3 {
		t.Fatalf("a do-nothing bridge passed the hook: %v", problems)
	}
}
