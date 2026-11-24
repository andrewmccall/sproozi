package docker

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// TestDockerModelRoutesAreRecorded records what the Docker composition serves
// on each model.inference authority, with what each handler was built with,
// for every managed harness and credential mechanism, each provider's public
// origin and a private one, with and without the GitHub and Kubernetes
// capabilities and with and without a replacement sign-in relay; and for a
// model authority outside the provider table.
func TestDockerModelRoutesAreRecorded(t *testing.T) {
	record := &routetest.Grouped{}
	compose := func(name string, kind harness.NativeKind, auth modelauth.Authenticator, upstream string, services,
		signIn bool) {
		owner := &Owner{policy: gateway.PolicySnapshot{Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{
			api.CapabilityModelInference}}}}
		config := GatewayConfig{Harness: kind, AuditLogger: audit.NewLogger(io.Discard),
			Certificates: map[string]tls.Certificate{gatewayFixtureHost: {}}, MCPAuthority: gatewayFixtureHost + ":443",
			Model: modelgateway.HandlerConfig{UpstreamURL: upstream, ProviderAuth: auth, HTTPClient: http.DefaultClient,
				Drain: modelgateway.NewDrain()}}
		if services {
			kube, _ := url.Parse("https://kubernetes.fixture")
			owner.policy.Spec.AllowedCapabilities = append(owner.policy.Spec.AllowedCapabilities,
				api.CapabilityGitHubPullRequest, api.CapabilityKubernetesRead)
			owner.nativeCapabilities = []api.CapabilityKind{api.CapabilityGitHubPullRequest, api.CapabilityKubernetesRead}
			config.GitHub = &githubgateway.HandlerConfig{TokenProvider: fixtureTokens{}, HTTPClient: http.DefaultClient,
				GitBaseURL: "https://github.com", APIBaseURL: "https://api.github.com"}
			config.Kubernetes = &kubernetesgateway.HandlerConfig{Upstream: kube, Client: http.DefaultClient}
		}
		if signIn {
			config.SignIn = modelgateway.NewAnthropicSignInHandler("https://sign-in.fixture", http.DefaultClient,
				config.AuditLogger)
		}
		routes, err := composeRoutes(owner, config, budget.NewTracker(), diagnose.NewJournal(config.AuditLogger,
			diagnose.Limits{}))
		if err != nil {
			record.Add(name, []string{"refused: " + err.Error()})
			return
		}
		record.Add(name, routetest.ModelRouteLines(routes))
	}
	upstreams := []string{openAIOrigin, "https://api.anthropic.com", "https://api.individual.githubcopilot.com",
		"https://models.fixture"}
	for _, kind := range harness.ManagedKinds() {
		h, _ := harness.Managed(kind)
		provider, _ := modelgateway.ProviderFor(h.Provider(true))
		for _, mechanism := range []struct {
			name string
			auth modelauth.Authenticator
		}{
			{mechanismKey, modelauth.APIKey{Key: "sk-fixture"}},
			{"anthropic-key", modelauth.AnthropicAPIKey{Key: "sk-ant-fixture"}},
			{mechanismToken, modelauth.Token{Value: "fixture-token", Headers: provider.TokenHeaders}},
			{"chatgpt", &modelauth.ChatGPT{}},
			{mechanismSignIn, modelauth.Workload{}},
		} {
			for _, upstream := range upstreams {
				for _, services := range []bool{true, false} {
					for _, signIn := range []bool{false, true} {
						name := string(kind) + "/" + mechanism.name + "/" + upstream
						if !services {
							name += "/no-services"
						}
						if signIn {
							name += "/sign-in-relay"
						}
						compose(name, kind, mechanism.auth, upstream, services, signIn)
					}
				}
			}
		}
	}
	const stray = "api.newmodel.example:443"
	endpoints.SemanticAuthorities[stray] = api.CapabilityModelInference
	t.Cleanup(func() { delete(endpoints.SemanticAuthorities, stray) })
	for _, kind := range harness.ManagedKinds() {
		compose("stray/"+string(kind)+"/"+mechanismKey, kind, modelauth.APIKey{Key: "sk-fixture"}, openAIOrigin, true, false)
		compose("stray/"+string(kind)+"/"+mechanismSignIn, kind, modelauth.Workload{}, openAIOrigin, true, false)
	}
	compareRecorded(t, filepath.Join("testdata", "model_routes.txt"), record.Lines())
}
