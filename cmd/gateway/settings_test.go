package main

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
)

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
