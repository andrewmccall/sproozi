package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const (
	allModules          = "kubernetes.read,model.inference,github.pull_request,packages.install,network.egress"
	copilotFixtureToken = "gho_fixture"
	sweepAnthropic      = "anthropic"
	sweepAll            = "all"
	sweepBoth           = "both"
	sweepOpenAI         = "openai"
	registryJSON        = `{"servers":{"` + routetest.MCPServer + `":{"url":"https://docs.mcp.example/mcp"}}}`
)

// composeServing builds the dispatcher with main's composeGateway, every
// handler reaching routetest.Client's upstreams and writing to events.
func composeServing(t *testing.T, cfg *startupConfig,
	events *routetest.Recorder) (gateway.Dispatcher, *budget.Tracker) {
	t.Helper()
	client := routetest.Client(t)
	kubeURL, _ := url.Parse("https://kubernetes.default.svc")
	budgets := budget.NewTracker()
	routes, err := composeGateway(cfg, gatewayDeps{journal: diagnose.NewJournal(events, diagnose.Limits{}),
		budgets: budgets, upstream: client, model: client, kubeURL: kubeURL, kube: client,
		githubTokens: routetest.TokenProvider{}, chatGPT: routetest.ChatGPTStore{}, mcp: client})
	if err != nil {
		t.Fatal(err)
	}
	return routes, budgets
}

// credentialModes are the gateway's model credential configurations.
var credentialModes = []struct {
	name string
	env  map[string]string
}{
	{"openai-key", nil},
	{"openai-and-anthropic-keys", map[string]string{anthropicKeyEnv: anthropicFixtureKey}},
	{"anthropic-key", map[string]string{openAIKeyEnv: "", anthropicKeyEnv: anthropicFixtureKey}},
	{"sign-in-tokens", map[string]string{openAIKeyEnv: "", "OPENAI_SIGN_IN_TOKEN": "openai-token-fixture",
		"ANTHROPIC_SIGN_IN_TOKEN": "anthropic-token-fixture", copilotTokenEnv: copilotFixtureToken}},
	{"copilot-token", map[string]string{copilotTokenEnv: copilotFixtureToken}},
	{modelauth.WorkloadMode, map[string]string{modelAuthModeEnv: modelauth.WorkloadMode, openAIKeyEnv: ""}},
	{"workload-with-openai-key", map[string]string{modelAuthModeEnv: modelauth.WorkloadMode}},
	{modelauth.ChatGPTMode, map[string]string{modelAuthModeEnv: modelauth.ChatGPTMode, openAIKeyEnv: ""}},
}

// moduleSelections are installed module sets.
var moduleSelections = []string{allModules, "kubernetes.read", "model.inference,network.egress",
	"kubernetes.read,model.inference,packages.install",
	"kubernetes.read,github.pull_request,packages.install,network.egress"}

func egressProfiles(t *testing.T) string {
	t.Helper()
	profiles := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(profiles, []byte(`{"`+routetest.EgressProfile+`":[{"scheme":"https","host":"`+
		routetest.EgressHost+`","port":443}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return profiles
}

// TestEveryComposedRouteKeepsTheRouteContract serves every route that main
// composes, through main's own construction, for each credential mode and
// module selection, against ADR 0070's contract.
func TestEveryComposedRouteKeepsTheRouteContract(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv("EGRESS_PROFILES_PATH", egressProfiles(t))
	registry := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(registry, []byte(registryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range credentialModes {
		for _, modules := range moduleSelections {
			t.Run(mode.name+"/"+modules, func(t *testing.T) {
				t.Setenv(copilotTokenEnv, "")
				t.Setenv("SPROOZI_ENABLED_CAPABILITIES", modules)
				for key, value := range mode.env {
					t.Setenv(key, value)
				}
				cfg, err := loadStartupConfig()
				if err != nil {
					t.Skipf("the gateway refuses this configuration at start-up: %v", err)
				}
				cfg.mcpRegistry = routetest.MCPRegistry(t)
				events := &routetest.Recorder{}
				routes, budgets := composeServing(t, cfg, events)
				routetest.Check(t, routes, events, budgets)
				routetest.Diagnoses(t, routes)
				declared := routetest.Edges(t, routes, events)
				if modules == allModules {
					for _, reason := range gateway.UnrecordedReasons() {
						if !declared[reason] {
							t.Errorf("no request declared %q unrecorded", reason)
						}
					}
				}
				if strays := routetest.ModelAuthorities(routes); len(strays) != 0 {
					t.Errorf("model.inference routes outside the provider table: %v", strays)
				}
			})
		}
	}
}

// TestEveryStartupConfigurationDeclaresItsRoutes composes main's dispatcher
// for every combination of auth mode, gateway keys, sign-in tokens, modules
// and MCP registry the loader accepts, and checks each route's declaration
// and the provider table, without serving.
func TestEveryStartupConfigurationDeclaresItsRoutes(t *testing.T) {
	validGatewayEnvironment(t)
	dir := t.TempDir()
	registry := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(registry, []byte(registryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EGRESS_PROFILES_PATH", egressProfiles(t))
	set := func(want bool, value string) string {
		if want {
			return value
		}
		return ""
	}
	composed := 0
	for _, mode := range []string{modelauth.APIKeyMode, modelauth.ChatGPTMode, modelauth.WorkloadMode} {
		for _, keys := range []string{"", sweepOpenAI, sweepAnthropic, sweepBoth} {
			for _, tokens := range []string{"", sweepOpenAI, sweepAnthropic, "copilot", sweepAll} {
				for _, modules := range append(moduleSelections, "network.egress", "kubernetes.read,model.inference") {
					for _, mcp := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/%s/%s/mcp=%t", mode, keys, tokens, modules, mcp), func(t *testing.T) {
							t.Setenv(modelAuthModeEnv, mode)
							t.Setenv(openAIKeyEnv, set(keys == sweepOpenAI || keys == sweepBoth, "sk-openai-fixture"))
							t.Setenv(anthropicKeyEnv, set(keys == sweepAnthropic || keys == sweepBoth, anthropicFixtureKey))
							t.Setenv("OPENAI_SIGN_IN_TOKEN", set(tokens == sweepOpenAI || tokens == sweepAll, "openai-token-fixture"))
							t.Setenv("ANTHROPIC_SIGN_IN_TOKEN",
								set(tokens == sweepAnthropic || tokens == sweepAll, "anthropic-token-fixture"))
							t.Setenv(copilotTokenEnv, set(tokens == "copilot" || tokens == sweepAll, copilotFixtureToken))
							t.Setenv("SPROOZI_ENABLED_CAPABILITIES", modules)
							t.Setenv("MCP_SERVERS_PATH", set(mcp, registry))
							cfg, err := loadStartupConfig()
							if err != nil {
								return // refused before composition
							}
							composed++
							routes, err := composeGateway(cfg, gatewayDeps{journal: diagnose.NewJournal(nil, diagnose.Limits{}),
								budgets: budget.NewTracker(), chatGPT: routetest.ChatGPTStore{}})
							if err != nil {
								t.Fatal(err)
							}
							if strays := routetest.ModelAuthorities(routes); len(strays) != 0 {
								t.Errorf("model.inference routes outside the provider table: %v", strays)
							}
						})
					}
				}
			}
		}
	}
	if composed == 0 {
		t.Fatal("no configuration composed")
	}
	t.Logf("composed and validated %d configurations", composed)
}

// A model.inference authority added to endpoints.SemanticAuthorities without
// a provider-table entry is reserved and closed in every credential mode, and
// the gateway's certificate need not name it (ADR 0070).
func TestAModelAuthorityOutsideTheProviderTableIsNotServed(t *testing.T) {
	validGatewayEnvironment(t)
	endpoints.SemanticAuthorities[strayModelAuthority] = api.CapabilityModelInference
	t.Cleanup(func() { delete(endpoints.SemanticAuthorities, strayModelAuthority) })
	host, _, _ := net.SplitHostPort(strayModelAuthority)
	for _, mode := range credentialModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(copilotTokenEnv, "")
			for key, value := range mode.env {
				t.Setenv(key, value)
			}
			cfg, err := loadStartupConfig()
			if err != nil {
				t.Fatalf("the certificate does not name %s, which is not served: %v", host, err)
			}
			if _, certified := cfg.certificates[host]; certified {
				t.Errorf("%s needs a certificate name, but is not served", host)
			}
			cfg.mcpRegistry = routetest.MCPRegistry(t)
			routes, _ := composeServing(t, cfg, &routetest.Recorder{})
			route, reserved := routes[strayModelAuthority]
			if !reserved || route.Handler != nil || routes.Allows(strayModelAuthority) {
				t.Errorf("%s is not reserved and closed: %+v", strayModelAuthority, route)
			}
			if strays := routetest.ModelAuthorities(routes); len(strays) != 0 {
				t.Errorf("model.inference routes outside the provider table: %v", strays)
			}
		})
	}
}
