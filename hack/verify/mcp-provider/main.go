// Command mcp-provider supplies two deterministic HTTPS fixture providers for
// the isolated configured-MCP acceptance run. It is not a production adapter.
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type searchArgs struct {
	Collection string `json:"collection"`
	Query      string `json:"query"`
}
type inventoryArgs struct {
	Tenant string `json:"tenant"`
	Kind   string `json:"kind"`
}

func main() {
	provider := os.Getenv("PROVIDER")
	token, err := os.ReadFile("/credentials/token")
	if err != nil {
		log.Fatal("Missing fixture credential")
	}
	expected := "Bearer " + strings.TrimSpace(string(token))
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture-" + provider, Version: "1"}, nil)
	switch provider {
	case "docs":
		tool := &mcp.Tool{Name: "search",
			Description: "Search operations documentation. Supply collection operations and your query."}
		mcp.AddTool(server, tool, func(
			_ context.Context, _ *mcp.CallToolRequest, args searchArgs,
		) (*mcp.CallToolResult, any, error) {
			if args.Collection != "operations" {
				return nil, nil, fmt.Errorf("fixture scope violation")
			}
			log.Print("APPROVED_TOOL_CALL docs search")
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: `{"document":"operations-runbook","collection":"operations"}`},
			}}, nil, nil
		})
	case "inventory":
		tool := &mcp.Tool{Name: "list_assets", Description: "List home-ops assets. Supply tenant home-ops and kind router."}
		mcp.AddTool(server, tool, func(
			_ context.Context, _ *mcp.CallToolRequest, args inventoryArgs,
		) (*mcp.CallToolResult, any, error) {
			if args.Tenant != "home-ops" || args.Kind != "router" {
				return nil, nil, fmt.Errorf("fixture scope violation")
			}
			log.Print("APPROVED_TOOL_CALL inventory list_assets")
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: `{"assets":["home-router"],"tenant":"home-ops"}`},
			}}, nil, nil
		})
	default:
		log.Fatal("Unknown fixture provider")
	}
	mcp.AddTool(server, &mcp.Tool{Name: "delete_all", Description: "Unapproved fixture tool"}, func(
		_ context.Context, _ *mcp.CallToolRequest, _ map[string]any,
	) (*mcp.CallToolResult, any, error) {
		log.Print("UNAPPROVED_TOOL_CALL")
		return nil, nil, fmt.Errorf("unapproved tool reached provider")
	})
	handler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true},
	)
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
	log.Print("Fixture provider ready")
	httpServer := &http.Server{Addr: ":8443", Handler: router, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(httpServer.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key"))
}
