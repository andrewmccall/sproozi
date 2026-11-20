package docker

import (
	"crypto/tls"
	"net/url"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/gateway/routetest"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// TestEveryComposedRouteKeepsTheRouteContract checks every route the Docker
// composition serves, for every managed harness and credential mechanism,
// against ADR 0070's contract. The ChatGPT mechanism needs a stored sign-in
// to serve a request; its routes are those of a key (routes.txt).
func TestEveryComposedRouteKeepsTheRouteContract(t *testing.T) {
	for _, kind := range harness.ManagedKinds() {
		for _, mechanism := range []struct {
			name string
			auth modelauth.Authenticator
		}{
			{"key", modelauth.APIKey{Key: "sk-gateway-fixture"}},
			{"token", modelauth.Token{Value: "gateway-token-fixture"}},
			{"sign-in", modelauth.Workload{}},
		} {
			t.Run(string(kind)+"/"+mechanism.name, func(t *testing.T) {
				client := routetest.Client(t)
				upstream, _ := url.Parse("https://kubernetes.default.svc")
				owner := &Owner{policy: gateway.PolicySnapshot{Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{
					api.CapabilityModelInference, api.CapabilityGitHubPullRequest, api.CapabilityKubernetesRead}}},
					nativeCapabilities: []api.CapabilityKind{api.CapabilityGitHubPullRequest, api.CapabilityKubernetesRead}}
				events := &routetest.Recorder{}
				config := GatewayConfig{Harness: kind, AuditLogger: events,
					Certificates: map[string]tls.Certificate{"gateway.fixture": {}}, MCPAuthority: "gateway.fixture:443",
					MCPRegistry: routetest.MCPRegistry(t), MCPClient: client,
					GitHub: &githubgateway.HandlerConfig{TokenProvider: routetest.TokenProvider{}, HTTPClient: client,
						GitBaseURL: "https://github.com", APIBaseURL: "https://api.github.com"},
					Kubernetes: &kubernetesgateway.HandlerConfig{Upstream: upstream, Client: client},
					Model: modelgateway.HandlerConfig{UpstreamURL: "https://api.openai.com", ProviderAuth: mechanism.auth,
						HTTPClient: client}}
				if h, _ := harness.Managed(kind); h.Protocol == harness.ProtocolAnthropic {
					config.Model.UpstreamURL = "https://api.anthropic.com"
				}
				tracker := budget.NewTracker()
				routes, err := composeRoutes(owner, config, tracker, diagnose.NewJournal(events, diagnose.Limits{}))
				if err != nil {
					t.Fatal(err)
				}
				routetest.Check(t, routes, events, tracker)
				if strays := routetest.ModelAuthorities(routes); len(strays) != 0 {
					t.Errorf("model.inference routes outside the provider table: %v", strays)
				}
			})
		}
	}
}
