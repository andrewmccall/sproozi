package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness/mcpfake"
)

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
	if len(h.Loader.Runtime) == 0 {
		return []string{fmt.Sprintf("harness %q: MCP bridge %s has no Loader.Runtime, such as []string{\"node\"}, in %s",
			h.Selector, h.Loader.Bridge, registryFile)}
	}
	runtime, err := exec.LookPath(h.Loader.Runtime[0])
	if err != nil {
		message := fmt.Sprintf("harness %q: bridge runtime %q is not on PATH, so its bridge was not run; install it "+
			"to test the bridge", h.Selector, h.Loader.Runtime[0])
		if os.Getenv("CI") != "" {
			return []string{message}
		}
		_, _ = fmt.Fprintln(os.Stderr, "NOTICE: "+message)
		t.Skip(message)
	}
	server := httptest.NewServer(mcpfake.Handler(nil))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	path := filepath.Join(dir, h.Loader.Bridge)
	if err = os.WriteFile(path, []byte(bridge), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(names ...string) (string, string, error) {
		files, err := h.MCP.render(names, func(name string) string { return server.URL + "/mcp/" + name })
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(dir, h.MCP.File)
		if err = os.WriteFile(config, []byte(files[h.MCP.File]), 0600); err != nil {
			t.Fatal(err)
		}
		args := append(slices.Clone(h.Loader.Runtime[1:]), path, "--sproozi-check", config)
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(t.Context(), runtime, args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		return stdout.String(), strings.TrimSpace(stderr.String()), err
	}
	var problems []string
	out, stderr, err := run("github", "sproozi")
	var report struct {
		Tools    []string `json:"tools"`
		Diagnose string   `json:"diagnose"`
	}
	if err != nil || json.Unmarshal([]byte(out), &report) != nil {
		problems = append(problems, fmt.Sprintf("harness %q: bridge %s --sproozi-check CONFIG must print "+
			`{"tools":[...],"diagnose":"..."}`+" (%v): stdout %q, stderr %q; see Writing an MCP bridge in "+
			"docs/contributing/add-a-harness.md", h.Selector, h.Loader.Bridge, err, out, stderr))
	}
	for _, want := range []string{"github__list_issues", "sproozi__diagnose"} {
		if !slices.Contains(report.Tools, want) {
			problems = append(problems, fmt.Sprintf("harness %q: bridge %s did not load %s from %s; it reported %v; "+
				"stderr %q", h.Selector, h.Loader.Bridge, want, h.MCP.File, report.Tools, stderr))
		}
	}
	if report.Diagnose != mcpfake.DiagnoseResult {
		problems = append(problems, fmt.Sprintf("harness %q: bridge %s did not call sproozi__diagnose through MCP "+
			"tools/call: it reported %q, want %q; stderr %q", h.Selector, h.Loader.Bridge, report.Diagnose,
			mcpfake.DiagnoseResult, stderr))
	}
	if _, _, err = run("github"); err == nil {
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

// The hook itself: the example bridge in testdata passes, a bridge that loads
// nothing fails, and a bridge without a runtime is a problem, not a panic.
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
	if problems := bridgeProblems(t, example, `console.log('{"tools":[]}')`); len(problems) != 4 {
		t.Fatalf("a do-nothing bridge passed the hook: %v", problems)
	}
	example.Loader.Runtime = nil
	if problems := bridgeProblems(t, example, string(bridge)); len(problems) != 1 ||
		!strings.Contains(problems[0], "Loader.Runtime") {
		t.Fatalf("a bridge without a runtime: %v", problems)
	}
}
