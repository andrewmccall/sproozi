package main

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints/destination"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	packagesgateway "github.com/andrewmccall/sproozi/internal/endpoints/packages"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const (
	anthropicFixtureKey = "anthropic-fixture-key"
	withoutEgress       = "kubernetes.read,model.inference,github.pull_request,packages.install"
)

var updateRecords = flag.Bool("update-records", false,
	"rewrite testdata/routes.txt from the current code")

// compareRecorded compares lines with the recorded file, or rewrites it under
// -update-records.
func compareRecorded(t *testing.T, file string, lines []string) {
	t.Helper()
	got := strings.Join(lines, "\n") + "\n"
	if *updateRecords {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d lines, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("route changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

// routeLines describes every route and path route of a composition, by
// authority: capability, tier, handler type, whether it forwards the
// workload's provider credential and whether budgets and audit are attached.
func routeLines(prefix string, routes gateway.Dispatcher) []string {
	describe := func(name string, r gateway.Route) string {
		handler := "none"
		if r.Handler != nil {
			handler = fmt.Sprintf("%T", r.Handler)
		}
		capability := string(r.Capability)
		if capability == "" {
			capability = "-"
		}
		return strings.Join([]string{prefix, name, capability, string(r.Tier), handler,
			fmt.Sprintf("providerCredential=%t", r.ProviderCredential),
			fmt.Sprintf("budgets=%t", r.Budgets != nil), fmt.Sprintf("audit=%t", r.AuditLogger != nil)}, " | ")
	}
	var lines []string
	for _, authority := range slices.Sorted(maps.Keys(routes)) {
		route := routes[authority]
		lines = append(lines, describe(authority, route))
		for _, p := range route.Paths {
			lines = append(lines, describe(authority+p.Prefix, p.Route))
		}
	}
	return lines
}

// composedRoutes builds the dispatcher as main does, from the environment.
func composedRoutes(t *testing.T) gateway.Dispatcher {
	t.Helper()
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatal(err)
	}
	logger := audit.NewLogger(io.Discard)
	journal := diagnose.NewJournal(logger, diagnose.Limits{})
	handlers := map[api.CapabilityKind]http.Handler{
		api.CapabilityKubernetesRead: kubernetesgateway.NewHandler(kubernetesgateway.HandlerConfig{AuditLogger: journal})}
	openAI, anthropic, signedIn := modelHandlers(cfg, journal, http.DefaultClient, nil, nil)
	if openAI != nil {
		handlers[api.CapabilityModelInference] = openAI
	}
	if cfg.enabled[api.CapabilityGitHubPullRequest] {
		handlers[api.CapabilityGitHubPullRequest] = &githubgateway.Handler{}
	}
	handlers[api.CapabilityPackagesInstall] = &packagesgateway.Handler{}
	handlers[api.CapabilityNetworkEgress] = &destination.Handler{}
	named := mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: cfg.mcpRegistry, AuditLogger: journal})
	dispatcher := composeDispatcher(cfg, handlers, diagnose.NewControl(journal, named), anthropic, signedIn)
	attachAccounting(dispatcher, budget.NewTracker(), journal)
	if err := dispatcher.Validate(); err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

// TestComposedRoutesAreRecorded records the Kubernetes composition's route
// table for each credential mode and module selection.
func TestComposedRoutesAreRecorded(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "servers.json")
	servers := []byte(`{"servers":{"docs":{"url":"https://docs.mcp.example/mcp"}}}`)
	if err := os.WriteFile(registry, servers, 0o600); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(profiles, []byte(`{"registry":[{"scheme":"https","host":"registry.example","port":443},
 {"scheme":"https","host":"pypi.org","port":443}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"openai-key", nil},
		{"openai-and-anthropic-keys", map[string]string{anthropicKeyEnv: anthropicFixtureKey}},
		{"anthropic-key-only", map[string]string{openAIKeyEnv: "", anthropicKeyEnv: anthropicFixtureKey}},
		{"copilot-token", map[string]string{copilotTokenEnv: "gho_fixture"}},
		{modelauth.WorkloadMode, map[string]string{modelAuthModeEnv: modelauth.WorkloadMode, openAIKeyEnv: ""}},
		{"workload-with-openai-key", map[string]string{modelAuthModeEnv: modelauth.WorkloadMode}},
		{"mcp-registry", map[string]string{"MCP_SERVERS_PATH": registry}},
		{"egress-profile", map[string]string{"EGRESS_PROFILES_PATH": profiles}},
		{"kubernetes-only", map[string]string{"SPROOZI_ENABLED_CAPABILITIES": "kubernetes.read", openAIKeyEnv: "",
			"GITHUB_APP_ID": "", "GITHUB_APP_INSTALLATION_ID": "", "GITHUB_APP_PRIVATE_KEY_PATH": ""}},
		{"no-egress", map[string]string{"SPROOZI_ENABLED_CAPABILITIES": withoutEgress}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			t.Setenv(copilotTokenEnv, "")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			lines = append(lines, routeLines(tc.name, composedRoutes(t))...)
		})
	}
	compareRecorded(t, filepath.Join("testdata", "routes.txt"), lines)
}
