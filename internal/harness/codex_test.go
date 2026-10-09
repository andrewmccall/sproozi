package harness_test

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	testDocsCapability = "mcp.docs"
	testClaudeName     = "claude-code"
	testOpenCodeName   = "opencode"
	testHermesName     = "hermes"
)

func TestNativeMCPConfiguration(t *testing.T) {
	for _, tt := range []struct{ harness, filename, expected string }{
		{"codex", "codex-mcp.toml", "[mcp_servers.\"docs\"]\nurl = \"https://gateway.svc:8443/mcp/docs\"\nrequired = true\n\n[mcp_servers.\"inventory\"]\nurl = \"https://gateway.svc:8443/mcp/inventory\"\nrequired = true\n\n"},
		{testClaudeName, "claude-mcp.json", "{\"mcpServers\":{\"docs\":{\"type\":\"http\",\"url\":\"https://gateway.svc:8443/mcp/docs\"},\"inventory\":{\"type\":\"http\",\"url\":\"https://gateway.svc:8443/mcp/inventory\"}}}\n"},
		{testOpenCodeName, "opencode-mcp.json", "{\"mcp\":{\"docs\":{\"enabled\":true,\"oauth\":false,\"type\":\"remote\",\"url\":\"https://gateway.svc:8443/mcp/docs\"},\"inventory\":{\"enabled\":true,\"oauth\":false,\"type\":\"remote\",\"url\":\"https://gateway.svc:8443/mcp/inventory\"}}}\n"},
		{testHermesName, "hermes-mcp.json", "{\"mcp_servers\":{\"sproozi-mcp-docs\":{\"connect_timeout\":15,\"enabled\":true,\"strict_redirect_headers\":true,\"timeout\":60,\"url\":\"https://gateway.svc:8443/mcp/docs\"},\"sproozi-mcp-inventory\":{\"connect_timeout\":15,\"enabled\":true,\"strict_redirect_headers\":true,\"timeout\":60,\"url\":\"https://gateway.svc:8443/mcp/inventory\"}}}\n"},
	} {
		t.Run(tt.harness, func(t *testing.T) {
			files, err := harness.MCPConfig(tt.harness, []string{"kubernetes.read", "mcp.inventory", "model.inference", testDocsCapability, testDocsCapability}, "https://gateway.svc:8443")
			if err != nil || len(files) != 1 || files[tt.filename] != tt.expected {
				t.Fatalf("files=%v error=%v", files, err)
			}
			empty, err := harness.MCPConfig(tt.harness, nil, "https://gateway.svc:8443")
			if err != nil || len(empty) != 1 {
				t.Fatalf("empty configuration=%v error=%v", empty, err)
			}
		})
	}
}

func TestMCPConfigurationRejectsInvalidSelectionsAndConnections(t *testing.T) {
	for _, tt := range []struct{ selected, endpoint, capability string }{
		{"unsupported", "https://gateway.svc", testDocsCapability},
		{"codex", "http://gateway.svc", testDocsCapability},
		{testClaudeName, "https://user:password@gateway.svc", testDocsCapability},
		{testOpenCodeName, "https://gateway.svc/path", testDocsCapability},
		{testOpenCodeName, "https://gateway.svc?query", testDocsCapability},
		{testClaudeName, "https://gateway.svc", "mcp.bad/name"},
	} {
		if _, err := harness.MCPConfig(tt.selected, []string{tt.capability}, tt.endpoint); err == nil {
			t.Fatalf("accepted invalid configuration %+v", tt)
		}
	}
	files, err := harness.MCPConfig("", nil, "")
	if err != nil || len(files) != 0 {
		t.Fatalf("administrator configuration=%v error=%v", files, err)
	}
}

func TestHermesMCPNamesDoNotSelectBuiltinToolsets(t *testing.T) {
	files, err := harness.MCPConfig(testHermesName, []string{"mcp.all", "mcp.terminal"}, "https://gateway.svc")
	expected := "{\"mcp_servers\":{\"sproozi-mcp-all\":{\"connect_timeout\":15,\"enabled\":true,\"strict_redirect_headers\":true,\"timeout\":60,\"url\":\"https://gateway.svc/mcp/all\"},\"sproozi-mcp-terminal\":{\"connect_timeout\":15,\"enabled\":true,\"strict_redirect_headers\":true,\"timeout\":60,\"url\":\"https://gateway.svc/mcp/terminal\"}}}\n"
	if err != nil || files["hermes-mcp.json"] != expected {
		t.Fatalf("Native toolset selection broadened: %v %v", files, err)
	}
}
