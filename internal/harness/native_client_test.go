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
	if err = json.Unmarshal(data, &connections); err != nil || len(connections) != 1 || connections[0].Name != fixtureMCPName || !connections[0].Enabled || connections[0].Transport.URL != "https://sproozi-mcp.example/mcp/docs" || strings.Contains(string(data), "token_file") {
		t.Fatalf("stock connections=%s err=%v", data, err)
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
	c, err = NativeClients(NativeCodex, nil, "")
	if err != nil || c != nil {
		t.Fatalf("empty declared clients=%+v error=%v", c, err)
	}
}
