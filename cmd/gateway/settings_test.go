package main

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
)

// The settings' variables, and the values the tests below compare against.
// They repeat the defaults on purpose: a changed default must change a test.
const (
	agentNamespaceEnv      = "AGENT_NAMESPACE"
	credentialNamespaceEnv = "CREDENTIAL_NAMESPACE"
	budgetNamespaceEnv     = "BUDGET_NAMESPACE"
	gitBaseURLEnv          = "GIT_BASE_URL"
	apiBaseURLEnv          = "API_BASE_URL"
	modelDiagnosticsEnv    = "MODEL_DIAGNOSTICS"

	systemNamespaceValue = "sproozi-system"
	publicGitOrigin      = "https://github.com"
	publicAPIOrigin      = "https://api.github.com"
	customGitOrigin      = "https://git.example"
	customAPIOrigin      = "https://api.example"
)

// The namespaces, GitHub origins and diagnostics switch are read once by the
// loader. An empty value counts as unset, and none is validated.
func TestLoaderReadsNamespacesOriginsAndDiagnostics(t *testing.T) {
	type settings struct {
		agent, credential, budget, git, api string
		diagnostics                         bool
	}
	defaults := settings{"sproozi-agents", systemNamespaceValue, systemNamespaceValue,
		publicGitOrigin, publicAPIOrigin, false}
	names := []string{agentNamespaceEnv, credentialNamespaceEnv, budgetNamespaceEnv,
		gitBaseURLEnv, apiBaseURLEnv, modelDiagnosticsEnv}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want settings
	}{
		{"no variable set", nil, defaults},
		{"all empty", map[string]string{agentNamespaceEnv: "", credentialNamespaceEnv: "", budgetNamespaceEnv: "",
			gitBaseURLEnv: "", apiBaseURLEnv: "", modelDiagnosticsEnv: ""}, defaults},
		{"all set", map[string]string{agentNamespaceEnv: "a", credentialNamespaceEnv: "c", budgetNamespaceEnv: "b",
			gitBaseURLEnv: customGitOrigin, apiBaseURLEnv: customAPIOrigin, modelDiagnosticsEnv: "true"},
			settings{"a", "c", "b", customGitOrigin, customAPIOrigin, true}},
		{"values are kept as written", map[string]string{agentNamespaceEnv: "Not A Namespace", gitBaseURLEnv: "::"},
			settings{"Not A Namespace", systemNamespaceValue, systemNamespaceValue, "::", publicAPIOrigin, false}},
		{"diagnostics only for true", map[string]string{modelDiagnosticsEnv: "1"}, defaults},
		{"diagnostics is case-sensitive", map[string]string{modelDiagnosticsEnv: "True"}, defaults},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			for _, name := range names {
				t.Setenv(name, "")
			}
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			cfg, err := loadStartupConfig()
			if err != nil {
				t.Fatal(err)
			}
			got := settings{cfg.agentNamespace, cfg.credentialNamespace, cfg.budgetNamespace,
				cfg.gitBaseURL, cfg.apiBaseURL, cfg.modelDiagnostics}
			if got != tc.want {
				t.Fatalf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The GitHub upstream origins default to github.com and api.github.com, an
// empty value counts as unset, and a set value is kept as written.
func TestGitHubUpstreamOriginsComeFromTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, git, api, wantGit, wantAPI string
	}{
		{"neither set", "", "", publicGitOrigin, publicAPIOrigin},
		{"both set", customGitOrigin, customAPIOrigin, customGitOrigin, customAPIOrigin},
		{"only git set", customGitOrigin, "", customGitOrigin, publicAPIOrigin},
		{"set to anything", "not a url", "x", "not a url", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			t.Setenv("SPROOZI_ENABLED_CAPABILITIES", "github.pull_request")
			t.Setenv(gitBaseURLEnv, tc.git)
			t.Setenv(apiBaseURLEnv, tc.api)
			cfg, err := loadStartupConfig()
			if err != nil {
				t.Fatal(err)
			}
			routes, _ := composeServing(t, cfg, &routetest.Recorder{})
			for _, authority := range []string{githubgateway.GitAuthority, githubgateway.APIAuthority} {
				if routes[authority].Capability != api.CapabilityGitHubPullRequest {
					t.Fatalf("%s is not served by github.pull_request", authority)
				}
			}
			handler, ok := routes[githubgateway.APIAuthority].Handler.(*githubgateway.Handler)
			if !ok {
				t.Fatalf("GitHub handler is %T", routes[githubgateway.APIAuthority].Handler)
			}
			if handler.Config.GitBaseURL != tc.wantGit || handler.Config.APIBaseURL != tc.wantAPI {
				t.Fatalf("origins = %q, %q; want %q, %q",
					handler.Config.GitBaseURL, handler.Config.APIBaseURL, tc.wantGit, tc.wantAPI)
			}
		})
	}
}
