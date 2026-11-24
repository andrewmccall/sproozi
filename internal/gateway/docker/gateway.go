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
	// Every native execution reaches the Sproozi control connection on the MCP
	// authority; the journal forwards each audit record unchanged.
	journal := diagnose.NewJournal(config.AuditLogger, diagnose.Limits{})
	dispatch, err := composeRoutes(owner, config, budget.NewTrackerWithLedger(owner.ledger), journal)
	if err != nil {
		return nil, err
	}
	guard := gateway.NewLeakGuard(dispatch, nil)
	return proxytransport.New(proxytransport.Config{Certificates: config.Certificates, Serving: config.Serving, Authenticate: owner.Authenticate, Revalidate: owner.Revalidate, Handler: guard, AllowedAuthority: guard.Allows, ProviderCredential: guard.ForwardsCredential, AuditLogger: journal, MaxModelCallDuration: config.MaxModelCall})
}

// composeRoutes builds the routes NewGateway serves, each writing to journal
// and charging tracker, and refuses any route without a valid admission.
func composeRoutes(owner *Owner, config GatewayConfig, tracker *budget.Tracker, journal *diagnose.Journal) (gateway.Dispatcher, error) {
	config.AuditLogger = journal
	modelConfig := config.Model
	modelConfig.AuditLogger = config.AuditLogger
	dispatch, err := modelRoutes(config, modelConfig, tracker)
	if err != nil {
		return nil, err
	}
	if err := addNativeRoutes(owner, config, dispatch, tracker); err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(config.MCPAuthority)
	_, reserved := endpoints.SemanticAuthorities[config.MCPAuthority]
	if _, covered := config.Certificates[host]; err != nil || host == "" || port != "443" || reserved || !covered {
		return nil, errors.New("docker gateway: exact certified MCP authority on port 443 required")
	}
	// Without a registry every other path on the MCP authority is refused
	// with the standard 404, and the refusal is recorded without the path.
	var named http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := gateway.AuditEvent(r.Context(), "mcp.request")
		e.Service, e.DenyReason, e.Outcome = "mcp", "no MCP server is registered at this gateway", audit.RefusedPolicy
		gateway.Record(r.Context(), journal, e)
		http.NotFound(w, r)
	})
	if config.MCPRegistry != nil {
		named = mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: config.MCPRegistry, HTTPClient: config.MCPClient, Budgets: tracker, AuditLogger: journal})
	}
	dispatch[config.MCPAuthority] = gateway.Route{Admission: gateway.AdmitSelected,
		Tier: audit.TierProtocol, Handler: diagnose.NewControl(journal, named), AuditLogger: journal}
	if err := dispatch.Validate(); err != nil {
		return nil, err
	}
	return dispatch, nil
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
			// Keep a provider's path routes on the host, such as Copilot's.
			dispatch[host] = gateway.Route{Capability: api.CapabilityGitHubPullRequest, Tier: audit.TierSemantic, Handler: githubHandler, Budgets: tracker, AuditLogger: config.AuditLogger,
				Paths: dispatch[host].Paths}
		}
	}
	if config.Kubernetes != nil {
		if config.Kubernetes.Upstream == nil || config.Kubernetes.Client == nil {
			return errors.New("docker gateway: fixed Kubernetes provider required")
		}
		kubeConfig := *config.Kubernetes
		kubeConfig.AuditLogger = config.AuditLogger
		dispatch[kubernetesgateway.Authority] = gateway.Route{Capability: api.CapabilityKubernetesRead, Tier: audit.TierSemantic, Handler: kubernetesgateway.NewHandler(kubeConfig), Budgets: tracker, AuditLogger: config.AuditLogger}
	}
	for _, capability := range owner.nativeCapabilities {
		if !slices.Contains(owner.policy.Spec.AllowedCapabilities, capability) || capability == api.CapabilityGitHubPullRequest && config.GitHub == nil || capability == api.CapabilityKubernetesRead && config.Kubernetes == nil {
			return errors.New("docker gateway: configured native allocation lacks handler or policy")
		}
	}
	return nil
}

// modelRoutes serves the harness's provider, the one its mechanism reaches,
// such as OpenAI for Copilot with a key and GitHub's Copilot service when it
// signs in, with the provider table's routes for that credential (ADR 0047).
func modelRoutes(config GatewayConfig, modelConfig modelgateway.HandlerConfig, tracker *budget.Tracker) (gateway.Dispatcher, error) {
	h, _ := harness.Managed(config.Harness)
	mode := modelConfig.ProviderAuth.Mode()
	provider, ok := modelgateway.ProviderFor(h.Provider(mode == modelauth.WorkloadMode || mode == modelauth.TokenMode))
	if !ok {
		return nil, errors.New("docker gateway: unknown model provider")
	}
	if mode != modelauth.WorkloadMode && config.SignIn != nil {
		return nil, errors.New("docker gateway: a sign-in relay needs a harness that signs in itself")
	}
	// A configured private endpoint or fixture serves every route; the
	// public API default gives each route its own origin.
	var upstreams map[string]string
	if modelConfig.UpstreamURL != provider.Models[0].UpstreamURL {
		upstreams = map[string]string{}
		for _, m := range provider.Models {
			upstreams[m.Authority] = modelConfig.UpstreamURL
		}
	}
	dispatch := gateway.Dispatcher{}
	for _, served := range provider.Routes(modelConfig.ProviderAuth,
		modelgateway.RouteConfig{Handler: modelConfig, Upstreams: upstreams, SignIn: config.SignIn}) {
		route := served.Route
		route.Budgets, route.AuditLogger = tracker, config.AuditLogger
		if served.Prefix != "" {
			addPath(dispatch, served.Authority, served.Prefix, route)
		} else {
			dispatch[served.Authority] = route
		}
	}
	return dispatch, nil
}

// addPath serves prefix on authority with r, beside the authority's own
// route, which another capability may add later.
func addPath(dispatch gateway.Dispatcher, authority, prefix string, r gateway.Route) {
	owner := dispatch[authority]
	owner.Paths = append(owner.Paths, gateway.PathRoute{Prefix: prefix, Route: r})
	// The authority's other paths are refused through it until another
	// capability serves them, and that refusal is recorded.
	if owner.AuditLogger == nil {
		owner.AuditLogger = r.AuditLogger
	}
	dispatch[authority] = owner
}
