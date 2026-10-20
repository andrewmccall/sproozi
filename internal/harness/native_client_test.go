package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestNativeMCPConnectionsRejectClientAuthorityAndUnsupportedHarness(t *testing.T) {
	for _, gateway := range []string{"https://user:credential@broker.example", "https://broker.example/mcp/other", "https://broker.example?token=secret", "http://broker.example"} {
		if err := (&NativeClientConfig{MCPGateway: gateway, MCPServers: []string{fixtureMCPName}}).Validate(NativeCodex); err == nil {
			t.Fatalf("accepted invalid broker binding %q", gateway)
		}
	}
	if err := (&NativeClientConfig{MCPGateway: "https://broker.example", MCPServers: []string{fixtureMCPName}}).Validate(NativeClaude); err == nil {
		t.Fatal("unproved native Claude MCP configuration admitted")
	}
}

func TestStockCodexParsesGeneratedNativeMCPConnections(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("stock Codex not installed")
	}
	c := &NativeClientConfig{MCPGateway: "https://sproozi-mcp.example", MCPServers: []string{fixtureMCPName}}
	if err := c.Validate(NativeCodex); err != nil {
		t.Fatal(err)
	}
	args := make([]string, 0, 3+2*len(c.CodexOptions()))
	args = append(args, "mcp", "list", "--json")
	for _, option := range c.CodexOptions() {
		args = append(args, "-c", option)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "CODEX_HOME=" + t.TempDir()}
	data, err := command.Output()
	if err != nil {
		t.Fatalf("stock Codex MCP configuration: %v", err)
	}
	var connections []struct {
		Name      string `json:"name"`
		Enabled   bool   `json:"enabled"`
		Transport struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"transport"`
	}
	if err = json.Unmarshal(data, &connections); err != nil || len(connections) != 2 || strings.Contains(string(data), "token_file") {
		t.Fatalf("stock connections=%s err=%v", data, err)
	}
	urls := map[string]string{}
	for _, connection := range connections {
		if connection.Enabled {
			urls[connection.Name] = connection.Transport.URL
		}
	}
	if urls[fixtureMCPName] != "https://sproozi-mcp.example/mcp/docs" || urls[api.ControlMCPServer] != "https://sproozi-mcp.example/mcp/sproozi" {
		t.Fatalf("stock connections=%s", data)
	}
}

const nativeFixtureMCPCapability api.CapabilityKind = "mcp.alpha"

func TestNativeClientsRendersDeclaredCapabilitiesInStableOrder(t *testing.T) {
	c, err := NativeClients(NativeCodex, []api.CapabilityKind{"mcp.zeta", api.CapabilityKubernetesRead, nativeFixtureMCPCapability}, "https://gateway.test")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || !c.Kubernetes || c.MCPGateway != "https://gateway.test" || strings.Join(c.MCPServers, ",") != "alpha,zeta" {
		t.Fatalf("clients=%+v", c)
	}
	if _, err = NativeClients(NativeClaude, []api.CapabilityKind{nativeFixtureMCPCapability}, "https://gateway.test"); err == nil {
		t.Fatal("unsupported Claude MCP admitted")
	}
	if _, err = NativeClients(NativeCodex, []api.CapabilityKind{nativeFixtureMCPCapability, nativeFixtureMCPCapability}, "https://gateway.test"); err == nil {
		t.Fatal("duplicate MCP admitted")
	}
	if _, err = NativeClients(NativeCodex, nil, ""); err == nil {
		t.Fatal("native clients rendered without the gateway control connection")
	}
}

// Every managed native launch carries the Sproozi control connection, with or
// without granted capabilities, and the trusted entry follows administrator options.
func TestNativeClientsAlwaysCarryTheControlConnection(t *testing.T) {
	for _, kind := range []NativeKind{NativeCodex, NativeClaude} {
		c, err := NativeClients(kind, nil, "https://gateway.test")
		if err != nil || c == nil || c.ControlURL() != "https://gateway.test/mcp/sproozi" {
			t.Fatalf("%s clients=%+v err=%v", kind, c, err)
		}
	}
	if (*NativeClientConfig)(nil).Validate(NativeCodex) == nil {
		t.Fatal("native launch without the control connection accepted")
	}
	c := &NativeClientConfig{MCPGateway: "https://gateway.test", MCPServers: []string{fixtureMCPName}}
	if got := strings.Join(c.CodexOptions(), " "); got != `mcp_servers.docs.url="https://gateway.test/mcp/docs" mcp_servers.docs.required=true `+
		`mcp_servers.sproozi.url="https://gateway.test/mcp/sproozi" mcp_servers.sproozi.required=true` {
		t.Fatalf("codex options=%s", got)
	}
	var claude struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte((&NativeClientConfig{MCPGateway: "https://gateway.test"}).ClaudeMCPConfig()), &claude); err != nil ||
		len(claude.MCPServers) != 1 || claude.MCPServers["sproozi"].Type != "http" || claude.MCPServers["sproozi"].URL != "https://gateway.test/mcp/sproozi" {
		t.Fatalf("claude MCP config=%+v err=%v", claude, err)
	}
}
