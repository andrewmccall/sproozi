package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// strayModelAuthority is a model.inference authority the provider table does
// not name, added to endpoints.SemanticAuthorities by a test.
const strayModelAuthority = "api.newmodel.example:443"

// sweepCopilot selects a gateway-held Copilot token in a record's sweep.
const sweepCopilot = "copilot"

// credentialSources name the environment variable each fixture credential
// of the record is set in.
var credentialSources = routetest.Sources{
	"sk-openai-fixture":       openAIKeyEnv,
	anthropicFixtureKey:       anthropicKeyEnv,
	"openai-token-fixture":    openAITokenEnv,
	"anthropic-token-fixture": anthropicTokenEnv,
	copilotFixtureToken:       copilotTokenEnv,
}

// certifiedModelHosts are the names among every provider's hosts and every
// model.inference semantic authority's host that cfg's certificate must
// cover.
func certifiedModelHosts(cfg *startupConfig) string {
	hosts := map[string]bool{}
	for _, provider := range modelgateway.Providers() {
		for _, host := range provider.Hosts(true) {
			hosts[host] = true
		}
	}
	for authority, capability := range endpoints.SemanticAuthorities {
		if capability == api.CapabilityModelInference {
			host, _, _ := net.SplitHostPort(authority)
			hosts[host] = true
		}
	}
	var certified []string
	for host := range hosts {
		if _, ok := cfg.certificates[host]; ok {
			certified = append(certified, host)
		}
	}
	slices.Sort(certified)
	return "certificates: " + strings.Join(certified, " ")
}

// TestModelRoutesAreRecorded records, for every combination of auth mode,
// gateway keys, sign-in tokens and module selection, what the Kubernetes
// composition serves on each model.inference authority and which model hosts
// its certificate must cover, or the start-up error. It also records private
// upstreams, provider diagnostics, and a model authority outside the provider
// table.
func TestModelRoutesAreRecorded(t *testing.T) {
	validGatewayEnvironment(t)
	set := func(want bool, value string) string {
		if want {
			return value
		}
		return ""
	}
	record := &routetest.Grouped{}
	compose := func(name string, before, after func()) {
		before()
		cfg, err := loadStartupConfig()
		if err != nil {
			record.Add(name, []string{"refused: " + err.Error()})
			return
		}
		after()
		journal := diagnose.NewJournal(audit.NewLogger(io.Discard), diagnose.Limits{})
		routes, err := composeGateway(cfg, gatewayDeps{journal: journal, budgets: budget.NewTracker(),
			drain: modelgateway.NewDrain(), model: http.DefaultClient, chatGPT: routetest.ChatGPTStore{}})
		if err != nil {
			record.Add(name, []string{"refused: " + err.Error()})
			return
		}
		record.Add(name, append([]string{certifiedModelHosts(cfg)},
			routetest.ModelRouteLines(routes, credentialSources)...))
	}
	nothing := func() {}
	for _, mode := range []string{modelauth.APIKeyMode, modelauth.ChatGPTMode, modelauth.WorkloadMode} {
		for _, keys := range []string{"", sweepOpenAI, sweepAnthropic, sweepBoth} {
			for _, tokens := range []string{"", sweepOpenAI, sweepAnthropic, sweepCopilot, sweepAll} {
				for _, modules := range append(moduleSelections, "network.egress", "kubernetes.read,model.inference") {
					name := fmt.Sprintf("%s/keys=%s/tokens=%s/%s", mode, keys, tokens, modules)
					compose(name, func() {
						t.Setenv(modelAuthModeEnv, mode)
						t.Setenv(openAIKeyEnv, set(keys == sweepOpenAI || keys == sweepBoth, "sk-openai-fixture"))
						t.Setenv(anthropicKeyEnv, set(keys == sweepAnthropic || keys == sweepBoth, anthropicFixtureKey))
						t.Setenv("OPENAI_SIGN_IN_TOKEN", set(tokens == sweepOpenAI || tokens == sweepAll, "openai-token-fixture"))
						t.Setenv("ANTHROPIC_SIGN_IN_TOKEN",
							set(tokens == sweepAnthropic || tokens == sweepAll, "anthropic-token-fixture"))
						t.Setenv(copilotTokenEnv, set(tokens == sweepCopilot || tokens == sweepAll, copilotFixtureToken))
						t.Setenv("SPROOZI_ENABLED_CAPABILITIES", modules)
					}, nothing)
				}
			}
		}
	}
	reset := func(mode string) {
		t.Setenv(modelAuthModeEnv, mode)
		t.Setenv(openAIKeyEnv, "")
		t.Setenv(anthropicKeyEnv, "")
		t.Setenv("OPENAI_SIGN_IN_TOKEN", "")
		t.Setenv("ANTHROPIC_SIGN_IN_TOKEN", "")
		t.Setenv(copilotTokenEnv, "")
		t.Setenv("SPROOZI_ENABLED_CAPABILITIES", allModules)
	}
	for _, tc := range []struct {
		name   string
		before func()
	}{
		{"private-upstreams/workload", func() { reset(modelauth.WorkloadMode) }},
		{"private-upstreams/keys", func() {
			reset(modelauth.APIKeyMode)
			t.Setenv(openAIKeyEnv, "sk-openai-fixture")
			t.Setenv(anthropicKeyEnv, anthropicFixtureKey)
		}},
		{"private-upstreams/tokens", func() {
			reset(modelauth.APIKeyMode)
			t.Setenv("OPENAI_SIGN_IN_TOKEN", "openai-token-fixture")
			t.Setenv("ANTHROPIC_SIGN_IN_TOKEN", "anthropic-token-fixture")
			t.Setenv(copilotTokenEnv, copilotFixtureToken)
		}},
	} {
		compose(tc.name, func() {
			tc.before()
			t.Setenv("OPENAI_UPSTREAM_URL", "https://openai.private.example")
			t.Setenv("ANTHROPIC_UPSTREAM_URL", "https://anthropic.private.example")
		}, nothing)
	}
	t.Setenv("OPENAI_UPSTREAM_URL", "")
	t.Setenv("ANTHROPIC_UPSTREAM_URL", "")
	compose("diagnostics/workload", func() {
		reset(modelauth.WorkloadMode)
		t.Setenv("MODEL_DIAGNOSTICS", "true")
	}, nothing)
	t.Setenv("MODEL_DIAGNOSTICS", "")
	addStray := func() { endpoints.SemanticAuthorities[strayModelAuthority] = api.CapabilityModelInference }
	t.Cleanup(func() { delete(endpoints.SemanticAuthorities, strayModelAuthority) })
	for _, mode := range []string{modelauth.APIKeyMode, modelauth.WorkloadMode} {
		credential := func() {
			reset(mode)
			if mode == modelauth.APIKeyMode {
				t.Setenv(openAIKeyEnv, "sk-openai-fixture")
			}
		}
		// The stray authority is added after loading, as the fixture
		// certificate does not name it, and then before loading.
		compose("stray-after-loading/"+mode, credential, addStray)
		compose("stray-before-loading/"+mode, func() { credential(); addStray() }, nothing)
		delete(endpoints.SemanticAuthorities, strayModelAuthority)
	}
	compareRecorded(t, filepath.Join("testdata", "model_routes.txt"), record.Lines())
}
