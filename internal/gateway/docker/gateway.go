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
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
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
	// SignIn relays the harness's own OAuth sign-in and refresh on its
	// SignInHost when the model route forwards the harness's credential.
	SignIn http.Handler
}

func NewGateway(owner *Owner, config GatewayConfig) (*proxytransport.Gateway, error) {
	if config.Harness.Validate() != nil || owner == nil || config.Model.ProviderAuth == nil || config.Model.UpstreamURL == "" {
		return nil, errors.New("docker gateway: fixed model provider required")
	}
	if owner.policy.Spec.Budgets[api.CapabilityModelInference].MaxCostMicros > 0 && config.Model.Pricing == nil {
		return nil, errors.New("docker gateway: trusted model pricing required")
	}
	tracker := budget.NewTrackerWithLedger(owner.ledger)
	// Every native execution reaches the Sproozi control connection on the MCP
	// authority; the journal forwards each audit record unchanged.
	journal := diagnose.NewJournal(config.AuditLogger, diagnose.Limits{})
	config.AuditLogger = journal
	modelConfig := config.Model
	modelConfig.AuditLogger = config.AuditLogger
	authority := modelgateway.OpenAIAuthority
	handler := &modelgateway.Handler{Config: modelConfig}
	if h, _ := harness.Managed(config.Harness); h.Protocol == harness.ProtocolAnthropic {
		authority = modelgateway.AnthropicAuthority
		handler = modelgateway.NewAnthropicHandler(modelConfig)
	}
	workload := modelConfig.ProviderAuth != nil && modelConfig.ProviderAuth.Mode() == modelauth.WorkloadMode
	dispatch := gateway.Dispatcher{authority: {Capability: string(api.CapabilityModelInference), Tier: audit.TierSemantic, Handler: handler, Budgets: tracker, AuditLogger: config.AuditLogger, ProviderCredential: workload}}
	if config.SignIn != nil {
		h, _ := harness.Managed(config.Harness)
		if !workload || h.SignInHost == "" {
			return nil, errors.New("docker gateway: a sign-in route needs a harness that signs in itself")
		}
		dispatch[net.JoinHostPort(h.SignInHost, "443")] = gateway.Route{Capability: string(api.CapabilityModelInference), Tier: audit.TierSemantic, Handler: config.SignIn, Budgets: tracker, AuditLogger: config.AuditLogger}
	}
	if err := addNativeRoutes(owner, config, dispatch, tracker); err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(config.MCPAuthority)
	_, reserved := endpoints.SemanticAuthorities[config.MCPAuthority]
	if _, covered := config.Certificates[host]; err != nil || host == "" || port != "443" || reserved || !covered {
		return nil, errors.New("docker gateway: exact certified MCP authority on port 443 required")
	}
	var named = http.NotFoundHandler()
	if config.MCPRegistry != nil {
		named = mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: config.MCPRegistry, HTTPClient: config.MCPClient, Budgets: tracker, AuditLogger: journal})
	}
	dispatch[config.MCPAuthority] = gateway.Route{Tier: audit.TierProtocol, Handler: diagnose.NewControl(journal, named)}
	guard := gateway.NewLeakGuard(dispatch, nil)
	return proxytransport.New(proxytransport.Config{Certificates: config.Certificates, Serving: config.Serving, Authenticate: owner.Authenticate, Revalidate: owner.Revalidate, Handler: guard, AllowedAuthority: guard.Allows, ProviderCredential: guard.ForwardsCredential, AuditLogger: journal, MaxModelCallDuration: config.MaxModelCall})
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
