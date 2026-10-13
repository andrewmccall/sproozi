// Print public, run-local connection configuration for the offline CLI check.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func main() {
	selected := flag.String("harness", "codex", "Native CLI configuration format")
	flag.Parse()
	config, err := harness.MCPConfig(*selected, []string{"mcp.docs", "mcp.inventory"},
		"https://sproozi-gateway.sproozi-system.svc:8443")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, document := range config {
		fmt.Print(document)
	}
}
