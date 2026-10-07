package harness_test

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func TestCodexReceivesOnlyRequestedBrokerConnections(t *testing.T) {
	actual, err := harness.CodexMCPConfig([]string{"kubernetes.read", "mcp.inventory", "model.inference", "mcp.docs"}, "https://gateway.svc:8443")
	const expected = "[mcp_servers.\"docs\"]\nurl = \"https://gateway.svc:8443/mcp/docs\"\nrequired = true\n\n[mcp_servers.\"inventory\"]\nurl = \"https://gateway.svc:8443/mcp/inventory\"\nrequired = true\n\n"
	if err != nil || actual != expected {
		t.Fatalf("configuration=%q error=%v", actual, err)
	}
	if _, err := harness.CodexMCPConfig([]string{"mcp.bad/name"}, "https://gateway.svc:8443"); err == nil {
		t.Fatal("invalid connection name accepted")
	}
}
