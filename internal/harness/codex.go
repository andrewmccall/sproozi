// Package harness renders supported native client configuration at launch.
package harness

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// CodexMCPConfig describes only requested broker connections. It contains no
// upstream addresses, provider credentials or custom tool manifest.
func CodexMCPConfig(capabilities []string, gatewayEndpoint string) (string, error) {
	endpoint, err := url.Parse(gatewayEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.ForceQuery {
		return "", fmt.Errorf("invalid MCP gateway endpoint")
	}
	var names []string
	for _, capability := range capabilities {
		if name, mcp := api.CapabilityKind(capability).MCPServerName(); mcp {
			names = append(names, name)
		} else if strings.HasPrefix(capability, "mcp.") {
			return "", fmt.Errorf("invalid MCP capability")
		}
	}
	slices.Sort(names)
	var output strings.Builder
	for _, name := range slices.Compact(names) {
		_, _ = fmt.Fprintf(&output, "[mcp_servers.%s]\nurl = %s\nrequired = true\n\n",
			strconv.Quote(name), strconv.Quote(gatewayEndpoint+"/mcp/"+name))
	}
	return output.String(), nil
}
