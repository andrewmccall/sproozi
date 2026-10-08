// Package harness renders supported native client configuration at launch.
package harness

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	codexName    = "codex"
	claudeName   = "claude-code"
	openCodeName = "opencode"
)

// MCPConfig describes only requested broker connections in the selected CLI's
// native format. Empty selection leaves configuration to the administrator.
// It contains no upstream addresses or credentials.
func MCPConfig(selected string, capabilities []string, gatewayEndpoint string) (map[string]string, error) {
	if selected == "" {
		return nil, nil
	}
	if selected != codexName && selected != claudeName && selected != openCodeName {
		return nil, fmt.Errorf("unsupported harness %q", selected)
	}
	endpoint, err := url.Parse(gatewayEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.ForceQuery {
		return nil, fmt.Errorf("invalid MCP gateway endpoint")
	}
	var names []string
	for _, capability := range capabilities {
		if name, mcp := api.CapabilityKind(capability).MCPServerName(); mcp {
			names = append(names, name)
		} else if strings.HasPrefix(capability, "mcp.") {
			return nil, fmt.Errorf("invalid MCP capability")
		}
	}
	slices.Sort(names)
	var output strings.Builder
	connections := make(map[string]any)
	for _, name := range slices.Compact(names) {
		address := gatewayEndpoint + "/mcp/" + name
		switch selected {
		case codexName:
			_, _ = fmt.Fprintf(&output, "[mcp_servers.%s]\nurl = %s\nrequired = true\n\n", strconv.Quote(name), strconv.Quote(address))
		case claudeName:
			connections[name] = map[string]any{"type": "http", "url": address}
		case openCodeName:
			connections[name] = map[string]any{"type": "remote", "url": address, "enabled": true, "oauth": false}
		}
	}
	if selected == codexName {
		return map[string]string{"codex-mcp.toml": output.String()}, nil
	}
	key, field := "claude-mcp.json", "mcpServers"
	if selected == openCodeName {
		key, field = "opencode-mcp.json", "mcp"
	}
	document, err := json.Marshal(map[string]any{field: connections})
	if err != nil {
		return nil, err
	}
	return map[string]string{key: string(document) + "\n"}, nil
}
