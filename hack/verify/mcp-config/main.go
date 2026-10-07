// Print public, run-local connection configuration for the offline CLI check.
package main

import (
	"fmt"
	"os"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func main() {
	config, err := harness.CodexMCPConfig([]string{"mcp.docs", "mcp.inventory"},
		"https://sproozi-gateway.sproozi-system.svc:8443")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(config)
}
