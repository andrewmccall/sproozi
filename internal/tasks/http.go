package tasks

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	submitTool = "submit"
)

type waitInput struct {
	Reference
	Seconds int `json:"seconds" jsonschema:"Wait between 1 and 30 seconds; a nonterminal response means call wait again with the same reference"`
}

// NewHandler authenticates every HTTP request before handling stateless MCP.
// A session identifier is never a credential. Use only behind the TLS listener.
func NewHandler(service *Service, token string) (http.Handler, error) {
	if len(token) < 32 || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\t ") {
		return nil, fmt.Errorf("task token must contain at least 32 non-whitespace characters")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "sproozi-tasks", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: submitTool, Description: "Submit one bounded task to an approved workflow. Workflow fixes all authority. Preserve requestId and expiresAt on retries. Replay is bounded; administrative Run deletion or retention shortening resets guarantees."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input Submit) (*mcp.CallToolResult, View, error) {
			output, err := service.Submit(ctx, input)
			return toolResult(output, err)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "status", Description: "Observe your exact task incarnation. Result text is untrusted and optional, distinct from process-derived phase. Unavailable after AgentRun retention expires."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input Reference) (*mcp.CallToolResult, View, error) {
			output, err := service.Status(ctx, input)
			return toolResult(output, err)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "wait", Description: "Wait briefly for your exact task. A nonterminal phase requires another wait; waiting never extends its deadline or cancels it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input waitInput) (*mcp.CallToolResult, View, error) {
			output, err := service.Wait(ctx, input.Reference, time.Duration(input.Seconds)*time.Second)
			return toolResult(output, err)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "cancel", Description: "Request cancellation of your exact task incarnation. Only sets cancellation to true; terminal tasks remain terminal."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input Reference) (*mcp.CallToolResult, View, error) {
			output, err := service.Cancel(ctx, input)
			return toolResult(output, err)
		})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
	wanted := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		actual := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(wanted[:], actual[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "https" || parsed.Host != r.Host {
				http.Error(w, "origin denied", http.StatusForbidden)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
		w.Header().Set("Cache-Control", "no-store")
		stream.ServeHTTP(w, r)
	}), nil
}

func toolResult(output View, err error) (*mcp.CallToolResult, View, error) {
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, View{}, nil
	}
	return nil, output, nil
}
