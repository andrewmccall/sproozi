package tasks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	fixtureSubmitTool = "submit"
)

const testToken = "fixture-task-authentication-token-123456789"

type authorizedTransport struct{ base http.RoundTripper }

func (a authorizedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+testToken)
	return a.base.RoundTrip(copy)
}

func TestMCPUsesFixedWorkflowAndLiteralLifecycle(t *testing.T) {
	service, _ := fixtureService(t)
	handler, err := NewHandler(service, testToken)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	httpClient := server.Client()
	httpClient.Transport = authorizedTransport{base: httpClient.Transport}
	ctx := context.Background()
	consumer := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, err := consumer.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: fixtureSubmitTool, Arguments: fixtureRequest()})
	if err != nil || result.IsError {
		t.Fatalf("MCP submission failed: %#v %v", result, err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var observation View
	if err = json.Unmarshal(data, &observation); err != nil {
		t.Fatal(err)
	}
	if observation.ID != "task-7131d2b16aa0d3cbf705b2170ca8720542728094" || observation.Phase != "Queued" || observation.Result != nil || observation.CancellationRequested {
		t.Fatalf("unexpected native MCP observation: %s", data)
	}
	request := fixtureRequest()
	request.Workflow = "arbitrary-runtime"
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: fixtureSubmitTool, Arguments: request})
	if err != nil || !result.IsError {
		t.Fatalf("unknown workflow accepted: %#v %v", result, err)
	}
}

func TestMCPAuthenticatesEveryRequestAndRejectsCrossOrigin(t *testing.T) {
	service, _ := fixtureService(t)
	handler, err := NewHandler(service, testToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		authorization, origin string
		want                  int
	}{
		{"", "", http.StatusUnauthorized},
		{"Bearer wrong", "", http.StatusUnauthorized},
		{"Bearer " + testToken, "https://attacker.example", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "https://tasks.example/mcp", strings.NewReader(`{}`))
		request.Header.Set("Authorization", test.authorization)
		request.Header.Set("Origin", test.origin)
		request.Header.Set("Mcp-Session-Id", "a-session-is-not-authority")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("authorization/origin yielded %d, want %d", response.Code, test.want)
		}
	}
}
