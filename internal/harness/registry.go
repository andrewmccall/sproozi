package harness

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Harness is one registered agent program and every fact Sproozi's Go code
// needs about it. The registry below is the single list of harnesses: the
// AgentRun tier is every entry, the managed tier is the entries with a
// Native kind. Guard tests compare it with the CRD enum and with each
// package that still keeps its own per-harness code, and name the file to
// change. docs/contributing/add-a-harness.md is the checklist.
type Harness struct {
	// Selector is the AgentRuntime spec.clientConfig.harness value.
	Selector string
	// MCP renders the run's MCP connections, including the sproozi control
	// connection, in the harness's own configuration format. Every harness
	// has one: ADR 0039 requires an MCP client.
	MCP MCPFormat
	// LaunchFiles are extra contract files, by ConfigMap key, that the
	// harness's launch command reads.
	LaunchFiles map[string]string
	// RequestFiles adds instructions.txt and request.json to the contract for
	// launch commands that read the request from files.
	RequestFiles bool
	// Native is the managed-session kind, or empty when the harness runs only
	// as an AgentRun.
	Native NativeKind
	// NativeDirectory is the managed kind's native home under
	// /workspace/session and in checkpoints.
	NativeDirectory string
	// Protocol is the model API the managed kind speaks through the gateway.
	Protocol ModelProtocol
}

// MCPFormat is a harness's MCP configuration file. A JSON file holds a map,
// under Field, from connection name to Entry's value; an empty Field selects
// Codex's TOML tables.
type MCPFormat struct {
	File  string
	Field string
	Entry func(name, address string) (string, any)
}

// ModelProtocol names the provider API a managed harness calls.
type ModelProtocol string

const (
	ProtocolOpenAI    ModelProtocol = "openai"
	ProtocolAnthropic ModelProtocol = "anthropic"
)

// InspectionHost is the provider host the gateway inspects for the protocol.
func (p ModelProtocol) InspectionHost() string {
	if p == ProtocolAnthropic {
		return "api.anthropic.com"
	}
	return "api.openai.com"
}

var registry = []Harness{
	{Selector: string(NativeCodex), Native: NativeCodex, NativeDirectory: "codex", Protocol: ProtocolOpenAI,
		MCP: MCPFormat{File: "codex-mcp.toml"}},
	{Selector: string(NativeClaude), Native: NativeClaude, NativeDirectory: "claude", Protocol: ProtocolAnthropic,
		RequestFiles: true,
		MCP: MCPFormat{File: "claude-mcp.json", Field: "mcpServers", Entry: func(name, address string) (string, any) {
			return name, map[string]any{connectionType: httpTransport, connectionURL: address}
		}}},
	{Selector: "opencode", RequestFiles: true, LaunchFiles: map[string]string{"opencode-launch.py": OpenCodeLaunch},
		MCP: MCPFormat{File: "opencode-mcp.json", Field: "mcp", Entry: func(name, address string) (string, any) {
			return name, map[string]any{connectionType: "remote", connectionURL: address, "enabled": true, "oauth": false}
		}}},
	{Selector: "hermes", RequestFiles: true, LaunchFiles: map[string]string{"hermes-launch.py": HermesLaunch},
		MCP: MCPFormat{File: "hermes-mcp.json", Field: "mcp_servers", Entry: func(name, address string) (string, any) {
			// Native toolset aliases must not collide with built-ins such as all or terminal.
			return "sproozi-mcp-" + name, map[string]any{connectionURL: address, "enabled": true,
				"strict_redirect_headers": true, "connect_timeout": 15, "timeout": 60}
		}}},
}

// Lookup returns the registered harness for an AgentRuntime selector.
func Lookup(selector string) (Harness, error) {
	i := slices.IndexFunc(registry, func(h Harness) bool { return h.Selector == selector })
	if i < 0 {
		return Harness{}, fmt.Errorf("unsupported harness %q; registered harnesses are %s", selector,
			strings.Join(Selectors(), ", "))
	}
	return registry[i], nil
}

// Selectors lists every registered selector: the AgentRun tier.
func Selectors() []string {
	out := make([]string, 0, len(registry))
	for _, h := range registry {
		out = append(out, h.Selector)
	}
	return out
}

// ManagedKinds lists the kinds of the managed tier, in registry order.
func ManagedKinds() []NativeKind {
	var out []NativeKind
	for _, h := range registry {
		if h.Native != "" {
			out = append(out, h.Native)
		}
	}
	return out
}

// Managed returns the registered harness of a managed kind.
func Managed(kind NativeKind) (Harness, error) {
	i := slices.IndexFunc(registry, func(h Harness) bool { return h.Native != "" && h.Native == kind })
	if i < 0 {
		return Harness{}, fmt.Errorf("unsupported native harness %q", kind)
	}
	return registry[i], nil
}

// render writes connections, by name and address, in the harness's format.
func (f MCPFormat) render(names []string, address func(string) string) (map[string]string, error) {
	if f.Field == "" {
		var output strings.Builder
		for _, name := range names {
			_, _ = fmt.Fprintf(&output, "[mcp_servers.%s]\nurl = %s\nrequired = true\n\n", strconv.Quote(name),
				strconv.Quote(address(name)))
		}
		return map[string]string{f.File: output.String()}, nil
	}
	if f.Entry == nil || f.File == "" {
		return nil, fmt.Errorf("harness MCP format %q has no entry renderer", f.File)
	}
	connections := make(map[string]any, len(names))
	for _, name := range names {
		key, value := f.Entry(name, address(name))
		connections[key] = value
	}
	document, err := jsonDocument(map[string]any{f.Field: connections})
	if err != nil {
		return nil, err
	}
	return map[string]string{f.File: document}, nil
}
