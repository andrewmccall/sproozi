// Print public, run-local connection configuration for the offline CLI check.
// With --managed, print the strict MCP configuration a managed Claude Code
// session receives instead of an AgentRun's MCP file.
package main

import (
	"flag"
	"fmt"
	"os"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
)

const gateway = "https://sproozi-gateway.sproozi-system.svc:8443"

var capabilities = []api.CapabilityKind{"mcp.docs", "mcp.inventory"}

func main() {
	selected := flag.String("harness", "codex", "Native CLI configuration format")
	managed := flag.Bool("managed", false, "print a managed session's launch configuration (claude-code only)")
	flag.Parse()
	if err := run(*selected, *managed); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(selected string, managed bool) error {
	if managed {
		if harness.NativeKind(selected) != harness.NativeClaude {
			return fmt.Errorf("--managed renders claude-code only; Codex and Copilot managed configuration is checked elsewhere")
		}
		clients, err := harness.NativeClients(harness.NativeClaude, capabilities, gateway)
		if err != nil {
			return err
		}
		fmt.Println(clients.ClaudeMCPConfig())
		return nil
	}
	config, err := harness.MCPConfig(selected, []string{string(capabilities[0]), string(capabilities[1])}, gateway)
	if err != nil {
		return err
	}
	for _, document := range config {
		fmt.Print(document)
	}
	return nil
}
