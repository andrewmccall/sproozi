//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPDiscoveryAndSessionListingUseObjectSchemas(t *testing.T) {
	c, _ := fixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	token := strings.Repeat("t", 32)
	h, err := NewHandler(o, token, strings.Repeat("h", 32))
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/work/mcp", strings.NewReader(body))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var reply struct {
			Result map[string]any `json:"result"`
			Error  any            `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || w.Code != http.StatusOK || reply.Error != nil {
			t.Fatalf("MCP request failed: %d %s %v", w.Code, w.Body.String(), err)
		}
		return reply.Result
	}
	tools := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["tools"].([]any)
	for _, tool := range tools {
		definition := tool.(map[string]any)
		if definition["outputSchema"].(map[string]any)["type"] != "object" {
			t.Fatalf("MCP output schema rejects stock client discovery: %#v", definition)
		}
	}
	result := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sproozi_sessions","arguments":{}}}`)
	content := result["structuredContent"].(map[string]any)
	sessions := content["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("Missing retained native job: %#v", result)
	}
	view := sessions[0].(map[string]any)
	if view["id"] != v.ID || view["phase"] != "retained" || view["repository"] != "sproozi" || view["result"] != "Verified the repository change" {
		t.Fatalf("Unexpected native catalogue response: %#v", view)
	}
}
