package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	"github.com/andrewmccall/sproozi/internal/endpoints/destination"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	packagesgateway "github.com/andrewmccall/sproozi/internal/endpoints/packages"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// servingRoutes composes the dispatcher as main does, from the environment,
// with every handler reaching routetest.Client's upstreams and writing to
// events.
func servingRoutes(t *testing.T, events *routetest.Recorder) (gateway.Dispatcher, *budget.Tracker) {
	t.Helper()
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatal(err)
	}
	return composeServing(t, cfg, events)
}

func composeServing(t *testing.T, cfg *startupConfig,
	events *routetest.Recorder) (gateway.Dispatcher, *budget.Tracker) {
	t.Helper()
	cfg.mcpRegistry = routetest.MCPRegistry(t)
	client := routetest.Client(t)
	journal := diagnose.NewJournal(events, diagnose.Limits{})
	upstream, _ := url.Parse("https://kubernetes.default.svc")
	handlers := map[api.CapabilityKind]http.Handler{api.CapabilityKubernetesRead: kubernetesgateway.NewHandler(
		kubernetesgateway.HandlerConfig{Upstream: upstream, Client: client, AuditLogger: journal})}
	openAI, anthropic, signedIn := modelHandlers(cfg, journal, client, nil, nil)
	if openAI != nil {
		handlers[api.CapabilityModelInference] = openAI
	}
	if cfg.enabled[api.CapabilityGitHubPullRequest] {
		handlers[api.CapabilityGitHubPullRequest] = &githubgateway.Handler{Config: githubgateway.HandlerConfig{
			TokenProvider: routetest.TokenProvider{}, AuditLogger: journal, GitBaseURL: "https://github.com",
			APIBaseURL: "https://api.github.com", HTTPClient: client}}
	}
	handlers[api.CapabilityPackagesInstall] = &packagesgateway.Handler{Config: packagesgateway.HandlerConfig{
		HTTPClient: client, AuditLogger: journal}}
	handlers[api.CapabilityNetworkEgress] = &destination.Handler{Config: destination.HandlerConfig{
		Profiles: cfg.profiles, AuditLogger: journal, HTTPClient: client}}
	budgets := budget.NewTracker()
	named := mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: cfg.mcpRegistry, HTTPClient: client,
		Budgets: budgets, AuditLogger: journal})
	dispatcher := composeDispatcher(cfg, handlers, diagnose.NewControl(journal, named), anthropic, signedIn)
	attachAccounting(dispatcher, budgets, journal)
	return dispatcher, budgets
}

// TestEveryComposedRouteKeepsTheRouteContract checks every route of the
// Kubernetes composition, for each credential mode, against ADR 0070's
// contract.
func TestEveryComposedRouteKeepsTheRouteContract(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(profiles, []byte(`{"`+routetest.EgressProfile+`":[{"scheme":"https","host":"`+
		routetest.EgressHost+`","port":443}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"openai-key", nil},
		{"openai-and-anthropic-keys", map[string]string{anthropicKeyEnv: anthropicFixtureKey}},
		{"copilot-token", map[string]string{copilotTokenEnv: "gho_fixture"}},
		{modelauth.WorkloadMode, map[string]string{modelAuthModeEnv: modelauth.WorkloadMode, openAIKeyEnv: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			t.Setenv(copilotTokenEnv, "")
			t.Setenv("EGRESS_PROFILES_PATH", profiles)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			events := &routetest.Recorder{}
			routes, budgets := servingRoutes(t, events)
			routetest.Check(t, routes, events, budgets)
			if strays := routetest.ModelAuthorities(routes); len(strays) != 0 {
				t.Errorf("model.inference routes outside the provider table: %v", strays)
			}
		})
	}
}

// A model.inference authority added to endpoints.SemanticAuthorities without
// a provider-table entry is served by the OpenAI handler with the OpenAI
// credential. This is a known exception, kept until the Kubernetes
// composition builds its model routes from the provider table as the Docker
// composition does (ADR 0070); this test fails once that is fixed, so the
// exception is removed with it.
func TestKnownExceptionAModelAuthorityOutsideTheProviderTableGetsTheOpenAIHandler(t *testing.T) {
	const stray = "api.newmodel.example:443"
	validGatewayEnvironment(t)
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatal(err)
	}
	endpoints.SemanticAuthorities[stray] = api.CapabilityModelInference
	t.Cleanup(func() { delete(endpoints.SemanticAuthorities, stray) })
	routes, _ := composeServing(t, cfg, &routetest.Recorder{})
	strays := routetest.ModelAuthorities(routes)
	if len(strays) != 1 || strays[stray] != "*model.Handler" {
		t.Fatalf("model.inference routes outside the provider table: %v; if the stray authority is now closed, "+
			"remove this known exception", strays)
	}
}
