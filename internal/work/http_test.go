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
	t.Parallel()
	c, _ := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	standalone := submit(t, o)
	waitPhase(t, o, standalone.ID, phaseRetained)
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
	result := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sproozi_sessions","arguments":{"project":"sproozi-dev"}}}`)
	content := result["structuredContent"].(map[string]any)
	sessions := content["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("Missing retained native job: %#v", result)
	}
	view := sessions[0].(map[string]any)
	if view["id"] != v.ID || view["executionId"] != v.ID || view["allocationId"] != v.AllocationID || view["placement"] != "kubernetes" || view["project"] != "sproozi-dev" || view["phase"] != "retained" || view["repository"] != "sproozi" || view["result"] != "Verified the repository change" {
		t.Fatalf("Unexpected native catalogue response: %#v", view)
	}
	for _, tc := range []struct {
		body   string
		want   string
		status int
	}{
		{`{"project":"sproozi-dev"}`, v.ID, http.StatusOK},
		{`{"project":"home-ops"}`, "[]", http.StatusOK},
		{`{"project":"private"}`, "[]", http.StatusOK},
		{`{"project":"bad/project"}`, `{"error":"invalid_request"}`, http.StatusBadRequest},
		{`{"project":1}`, `{"error":"invalid_request"}`, http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/work/list", strings.NewReader(tc.body))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) || strings.Contains(w.Body.String(), standalone.ID) {
			t.Fatalf("Filtered catalogue %s returned %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	invalid := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sproozi_sessions","arguments":{"project":"bad/project"}}}`)
	if invalid["isError"] != true || !strings.Contains(invalid["content"].([]any)[0].(map[string]any)["text"].(string), "invalid_request:") {
		t.Fatalf("Invalid project filter lost its safe outcome: %#v", invalid)
	}
}
