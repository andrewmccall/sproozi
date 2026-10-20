package docker

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

// GatewayConfig fixes the model provider and named MCP registrations. The root
// service separately owns outer listener TLS and worker proxy/CA configuration.
// No workload-selectable upstream URL or provider credential is accepted.
type GatewayConfig struct {
	Harness      harness.NativeKind
	Certificates map[string]tls.Certificate
	// Serving, when set, supplies the current certificate for every inspected host.
	Serving      *proxytransport.Certificate
	Model        modelgateway.HandlerConfig
	MCPRegistry  *mcpgateway.Registry
	MCPAuthority string
	MCPClient    *http.Client
	AuditLogger  audit.Writer
	GitHub       *githubgateway.HandlerConfig
	Kubernetes   *kubernetesgateway.HandlerConfig
	// MaxModelCall is the longest one model call may stream. Zero selects
	// proxytransport.DefaultMaxModelCallDuration.
	MaxModelCall time.Duration
}

func NewGateway(owner *Owner, config GatewayConfig) (*proxytransport.Gateway, error) {
	if config.Harness.Validate() != nil || owner == nil || config.Model.ProviderAuth == nil || config.Model.UpstreamURL == "" {
		return nil, errors.New("docker gateway: fixed model provider required")
	}
	if owner.policy.Spec.Budgets[api.CapabilityModelInference].MaxCostMicros > 0 && config.Model.Pricing == nil {
		return nil, errors.New("docker gateway: trusted model pricing required")
	}
	tracker := budget.NewTrackerWithLedger(owner.ledger)
	modelConfig := config.Model
	modelConfig.AuditLogger = config.AuditLogger
	authority := modelgateway.OpenAIAuthority
	handler := &modelgateway.Handler{Config: modelConfig}
	if config.Harness == harness.NativeClaude {
		authority = modelgateway.AnthropicAuthority
		handler = modelgateway.NewAnthropicHandler(modelConfig)
	}
	dispatch := gateway.Dispatcher{authority: {Capability: string(api.CapabilityModelInference), Tier: audit.TierSemantic, Handler: handler, Budgets: tracker, AuditLogger: config.AuditLogger}}
	if err := addNativeRoutes(owner, config, dispatch, tracker); err != nil {
		return nil, err
	}
	if config.MCPRegistry != nil {
		host, port, err := net.SplitHostPort(config.MCPAuthority)
		_, reserved := endpoints.SemanticAuthorities[config.MCPAuthority]
		if err != nil || host == "" || port != "443" || reserved {
			return nil, errors.New("docker gateway: exact MCP authority on port 443 required")
		}
		dispatch[config.MCPAuthority] = gateway.Route{Tier: audit.TierProtocol, Handler: mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: config.MCPRegistry, HTTPClient: config.MCPClient, Budgets: tracker, AuditLogger: config.AuditLogger})}
	}
	return proxytransport.New(proxytransport.Config{Certificates: config.Certificates, Serving: config.Serving, Authenticate: owner.Authenticate, Revalidate: owner.Revalidate, Handler: dispatch, AllowedAuthority: dispatch.Allows, MaxModelCallDuration: config.MaxModelCall})
}

func addNativeRoutes(owner *Owner, config GatewayConfig, dispatch gateway.Dispatcher, tracker *budget.Tracker) error {
	if config.GitHub != nil {
		if config.GitHub.TokenProvider == nil || config.GitHub.HTTPClient == nil || config.GitHub.GitBaseURL == "" || config.GitHub.APIBaseURL == "" {
			return errors.New("docker gateway: fixed GitHub provider required")
		}
		githubConfig := *config.GitHub
		githubConfig.AuditLogger = config.AuditLogger
		githubHandler := &githubgateway.Handler{Config: githubConfig}
		for _, host := range []string{githubgateway.GitAuthority, githubgateway.APIAuthority} {
			dispatch[host] = gateway.Route{Capability: string(api.CapabilityGitHubPullRequest), Tier: audit.TierSemantic, Handler: githubHandler, Budgets: tracker, AuditLogger: config.AuditLogger}
		}
	}
	if config.Kubernetes != nil {
		if config.Kubernetes.Upstream == nil || config.Kubernetes.Client == nil {
			return errors.New("docker gateway: fixed Kubernetes provider required")
		}
		kubeConfig := *config.Kubernetes
		kubeConfig.AuditLogger = config.AuditLogger
		dispatch[kubernetesgateway.Authority] = gateway.Route{Capability: string(api.CapabilityKubernetesRead), Tier: audit.TierSemantic, Handler: kubernetesgateway.NewHandler(kubeConfig), Budgets: tracker, AuditLogger: config.AuditLogger}
	}
	for _, capability := range owner.nativeCapabilities {
		if !slices.Contains(owner.policy.Spec.AllowedCapabilities, capability) || capability == api.CapabilityGitHubPullRequest && config.GitHub == nil || capability == api.CapabilityKubernetesRead && config.Kubernetes == nil {
			return errors.New("docker gateway: configured native allocation lacks handler or policy")
		}
	}
	return nil
}
