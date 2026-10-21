// Command fake-mcp renders a harness's MCP file against a local fake MCP
// server and serves it, so a real client can be pointed at it on a laptop:
//
//	go run ./hack/verify/fake-mcp --harness NAME --dir /tmp/check
//
// It writes /tmp/check/<MCP file> with a github and a sproozi connection,
// prints each request the client makes, and runs until interrupted.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/harness/mcpfake"
)

func main() {
	selected := flag.String("harness", "", "registered harness selector")
	dir := flag.String("dir", "", "directory to write the rendered MCP file into")
	listen := flag.String("listen", "127.0.0.1:0", "address to serve the fake MCP server on")
	flag.Parse()
	if err := run(*selected, *dir, *listen); err != nil {
		fmt.Fprintln(os.Stderr, "fake-mcp:", err)
		os.Exit(1)
	}
}

func run(selected, dir, listen string) error {
	if selected == "" || dir == "" {
		return fmt.Errorf("--harness and --dir are required")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	base := "http://" + listener.Addr().String()
	files, err := harness.MCPConfigAt(selected, base, "github")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err = os.WriteFile(path, []byte(content), 0600); err != nil {
			return err
		}
		fmt.Printf("Wrote %s; point %s at it. Serving %s/mcp/{github,sproozi}; Ctrl-C stops.\n", path, selected, base)
	}
	fmt.Println("Expect the client to list github__list_issues and sproozi__diagnose; calling diagnose returns:",
		mcpfake.DiagnoseResult)
	return http.Serve(listener, mcpfake.Handler(os.Stdout))
}
