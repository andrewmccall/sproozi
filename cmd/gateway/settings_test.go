package main

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
)

// The namespaces, GitHub origins and diagnostics switch are read once by the
// loader. An empty value counts as unset, and none is validated.
func TestLoaderReadsNamespacesOriginsAndDiagnostics(t *testing.T) {
	type settings struct {
		agent, credential, budget, git, api string
		diagnostics                         bool
	}
	defaults := settings{"sproozi-agents", "sproozi-system", "sproozi-system",
		"https://github.com", "https://api.github.com", false}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want settings
	}{
		{"unset", nil, defaults},
		{"empty", map[string]string{"AGENT_NAMESPACE": "", "CREDENTIAL_NAMESPACE": "", "BUDGET_NAMESPACE": "",
			"GIT_BASE_URL": "", "API_BASE_URL": "", "MODEL_DIAGNOSTICS": ""}, defaults},
		{"set", map[string]string{"AGENT_NAMESPACE": "a", "CREDENTIAL_NAMESPACE": "c", "BUDGET_NAMESPACE": "b",
			"GIT_BASE_URL": "https://git.example", "API_BASE_URL": "https://api.example", "MODEL_DIAGNOSTICS": "true"},
			settings{"a", "c", "b", "https://git.example", "https://api.example", true}},
		{"not a namespace or origin is kept as written", map[string]string{"AGENT_NAMESPACE": "Not A Namespace",
			"GIT_BASE_URL": "::"}, settings{"Not A Namespace", "sproozi-system", "sproozi-system", "::",
			"https://api.github.com", false}},
		{"diagnostics only for true", map[string]string{"MODEL_DIAGNOSTICS": "1"}, defaults},
		{"diagnostics is case-sensitive", map[string]string{"MODEL_DIAGNOSTICS": "True"}, defaults},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			for _, name := range []string{"AGENT_NAMESPACE", "CREDENTIAL_NAMESPACE", "BUDGET_NAMESPACE",
				"GIT_BASE_URL", "API_BASE_URL", "MODEL_DIAGNOSTICS"} {
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
		{"unset", "", "", "https://github.com", "https://api.github.com"},
		{"set", "https://git.example", "https://api.example", "https://git.example", "https://api.example"},
		{"only git set", "https://git.example", "", "https://git.example", "https://api.github.com"},
		{"set to anything", "not a url", "x", "not a url", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			t.Setenv("SPROOZI_ENABLED_CAPABILITIES", "github.pull_request")
			t.Setenv("GIT_BASE_URL", tc.git)
			t.Setenv("API_BASE_URL", tc.api)
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
