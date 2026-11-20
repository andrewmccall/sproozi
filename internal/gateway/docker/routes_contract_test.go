package docker

import (
	"crypto/tls"
	"net/http"
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
// against ADR 0070's contract.
func TestEveryComposedRouteKeepsTheRouteContract(t *testing.T) {
	for _, kind := range harness.ManagedKinds() {
		for _, mechanism := range []struct {
			name string
			auth modelauth.Authenticator
		}{
			{mechanismKey, modelauth.APIKey{Key: "sk-gateway-fixture"}},
			{mechanismToken, modelauth.Token{Value: "gateway-token-fixture"}},
			{mechanismSignIn, modelauth.Workload{}},
			{modelauth.ChatGPTMode, modelauth.NewChatGPT(routetest.ChatGPTStore{}, &http.Client{})},
		} {
			t.Run(string(kind)+"/"+mechanism.name, func(t *testing.T) {
				if h, _ := harness.Managed(kind); mechanism.name == modelauth.ChatGPTMode && h.Protocol == harness.ProtocolAnthropic {
					t.Skip("a ChatGPT sign-in serves only OpenAI Responses; an Anthropic harness cannot use it")
				}
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
					Model: modelgateway.HandlerConfig{UpstreamURL: openAIOrigin, ProviderAuth: mechanism.auth,
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
				routetest.Edges(t, routes, events)
				if strays := routetest.ModelAuthorities(routes); len(strays) != 0 {
					t.Errorf("model.inference routes outside the provider table: %v", strays)
				}
			})
		}
	}
}

// A Docker gateway without an MCP registry, GitHub or Kubernetes leaves no
// request unrecorded by omission either.
func TestMinimalDockerRoutesRecordEveryEdgeRequest(t *testing.T) {
	for _, kind := range harness.ManagedKinds() {
		t.Run(string(kind), func(t *testing.T) {
			events := &routetest.Recorder{}
			owner := &Owner{policy: gateway.PolicySnapshot{Spec: api.AgentPolicySpec{
				AllowedCapabilities: []api.CapabilityKind{api.CapabilityModelInference}}}}
			config := GatewayConfig{Harness: kind, AuditLogger: events,
				Certificates: map[string]tls.Certificate{"gateway.fixture": {}}, MCPAuthority: "gateway.fixture:443",
				Model: modelgateway.HandlerConfig{UpstreamURL: openAIOrigin, ProviderAuth: modelauth.Workload{},
					HTTPClient: routetest.Client(t)}}
			routes, err := composeRoutes(owner, config, budget.NewTracker(), diagnose.NewJournal(events, diagnose.Limits{}))
			if err != nil {
				t.Fatal(err)
			}
			routetest.Edges(t, routes, events)
		})
	}
}
