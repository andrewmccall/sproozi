package mcp

import (
	"net/http"
	"os"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLiveServerToolsCompile lists a real server's tools and checks that the
// gateway accepts each tool's input schema. It runs only when
// SPROOZI_MCP_LIVE_URL names a server, such as https://mcp.deepwiki.com/mcp.
func TestLiveServerToolsCompile(t *testing.T) {
	url := os.Getenv("SPROOZI_MCP_LIVE_URL")
	if url == "" {
		t.Skip("SPROOZI_MCP_LIVE_URL is not set")
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "sproozi-live-test", Version: "live"}, nil).Connect(t.Context(),
		&sdk.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Timeout: 30 * time.Second},
			DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if _, _, err := compileInput(tool.InputSchema); err != nil {
			t.Errorf("tool %q: %v", tool.Name, err)
		} else {
			t.Logf("tool %q: input schema accepted", tool.Name)
		}
	}
}
