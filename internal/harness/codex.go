// Package harness renders supported native client configuration at launch.
package harness

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	connectionURL  = "url"
	connectionType = "type"
	// httpTransport is Claude Code's MCP transport name for Streamable HTTP.
	httpTransport = "http"
)

// MCPConfig describes the requested broker connections and the Sproozi control
// connection in the selected CLI's native format. Empty selection leaves
// configuration to the administrator; an unregistered one is an error. It
// contains no upstream addresses or credentials.
func MCPConfig(selected string, capabilities []string, gatewayEndpoint string) (map[string]string, error) {
	if selected == "" {
		return nil, nil
	}
	h, err := Lookup(selected)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(gatewayEndpoint)
	if err != nil || endpoint.Scheme != nativeHTTPScheme || endpoint.Host == "" || endpoint.User != nil ||
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
	// Every selected harness receives the control connection; MCPServerName
	// never admits its reserved name as a grant, so it cannot duplicate.
	names = append(slices.Compact(names), api.ControlMCPServer)
	return h.MCP.render(names, func(name string) string { return gatewayEndpoint + "/mcp/" + name })
}

// ContractFiles are the selected harness's files in an AgentRun contract: its
// MCP configuration, the Sproozi integration, its launch files and, when its
// launch reads them, the instructions and request. Empty selection adds none.
func ContractFiles(selected string, capabilities []string, gatewayEndpoint, instructions, request string) (map[string]string, error) {
	files, err := MCPConfig(selected, capabilities, gatewayEndpoint)
	if err != nil || selected == "" {
		return files, err
	}
	h, err := Lookup(selected)
	if err != nil {
		return nil, err
	}
	maps.Copy(files, ManagedContract())
	maps.Copy(files, h.LaunchFiles)
	if h.RequestFiles {
		files["instructions.txt"] = instructions
		files["request.json"] = request
	}
	return files, nil
}

func jsonDocument(value any) (string, error) {
	document, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(document) + "\n", nil
}
