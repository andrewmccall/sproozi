package model

import (
	"net/http"
	"slices"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// ModelRoute is one authority that serves a provider's model calls.
type ModelRoute struct {
	// Authority is the host:port the client calls.
	Authority string
	// UpstreamURL is the provider origin the gateway forwards to.
	UpstreamURL string
	// SignedIn marks an authority only a client signed in to a subscription
	// calls, such as the ChatGPT backend. A gateway-held API key never
	// serves it.
	SignedIn bool
	// New builds the handler for this authority.
	New func(HandlerConfig) http.Handler
}

// Provider is everything the gateway knows about one model provider (ADR
// 0047). Every harness that uses the provider gets the same routes, whichever
// mechanism authenticates it: a gateway-held key or token, or the client's own
// sign-in forwarded by the gateway.
type Provider struct {
	Name   string
	Models []ModelRoute
	// SignInAuthority and SignInRoutes relay the client's own sign-in and token
	// refresh when it signs in itself.
	SignInAuthority string
	SignInRoutes    []RelayRoute
	// SignInPrefix makes the sign-in relay a path route, for a sign-in host
	// another capability owns, such as Copilot's on github.com.
	SignInPrefix string
	// TokenHeaders are set with a gateway-held sign-in token, such as the beta
	// header Anthropic requires for an OAuth token.
	TokenHeaders map[string]string
	// APIKey attaches a gateway-held API key the way the provider's API
	// expects it. A provider without an API authority, such as Copilot, has
	// none.
	APIKey func(key string) modelauth.Authenticator
	// AccountAuthority, AccountPrefixes and AccountRoutes are the account and
	// policy requests a signed-in client makes on a host another capability
	// owns, such as Copilot's on api.github.com. Only these paths are served.
	AccountAuthority string
	AccountPrefixes  []string
	AccountRoutes    []RelayRoute
}

// Provider names, as harness model protocols spell them.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// The public API origins, which an administrator may replace and which the
// gateway checks against when a mode needs the public one.
const (
	OpenAIUpstreamURL    = "https://api.openai.com"
	AnthropicUpstreamURL = "https://api.anthropic.com"
)

// providers is the table, in the order compositions build their routes.
var providers = []Provider{
	{Name: ProviderOpenAI,
		Models: []ModelRoute{
			{Authority: OpenAIAuthority, UpstreamURL: OpenAIUpstreamURL,
				New: func(c HandlerConfig) http.Handler { return &Handler{Config: c} }},
			{Authority: ChatGPTBackendAuthority, UpstreamURL: "https://chatgpt.com", SignedIn: true,
				New: NewChatGPTBackendHandler},
		},
		SignInAuthority: OpenAISignInAuthority,
		SignInRoutes: []RelayRoute{
			{Method: http.MethodPost, Path: "/oauth/token"},
			{Method: http.MethodPost, Path: "/api/accounts/deviceauth/usercode"},
			{Method: http.MethodPost, Path: "/api/accounts/deviceauth/token"},
		},
		APIKey: func(key string) modelauth.Authenticator { return modelauth.APIKey{Key: key} }},
	{Name: ProviderAnthropic,
		Models: []ModelRoute{
			{Authority: AnthropicAuthority, UpstreamURL: AnthropicUpstreamURL,
				New: func(c HandlerConfig) http.Handler { return NewAnthropicHandler(c) }},
		},
		SignInAuthority: AnthropicSignInAuthority,
		SignInRoutes:    []RelayRoute{{Method: http.MethodPost, Path: "/v1/oauth/token"}},
		TokenHeaders:    map[string]string{"Anthropic-Beta": "oauth-2025-04-20"},
		APIKey:          func(key string) modelauth.Authenticator { return modelauth.AnthropicAPIKey{Key: key} }},
	// Copilot has no API-key host: a client signs in to GitHub, and its
	// sign-in or a gateway-held GitHub token reaches its plan's host.
	{Name: ProviderCopilot,
		Models: []ModelRoute{
			{Authority: CopilotIndividualAuthority, UpstreamURL: "https://api.individual.githubcopilot.com", SignedIn: true,
				New: NewCopilotHandler},
			{Authority: CopilotBusinessAuthority, UpstreamURL: "https://api.business.githubcopilot.com", SignedIn: true,
				New: NewCopilotHandler},
			{Authority: CopilotEnterpriseAuthority, UpstreamURL: "https://api.enterprise.githubcopilot.com", SignedIn: true,
				New: NewCopilotHandler},
		},
		// Copilot's /login runs GitHub's device flow on github.com, which
		// the GitHub capability otherwise serves for Git.
		SignInAuthority: "github.com:443", SignInPrefix: "/login/",
		SignInRoutes: []RelayRoute{
			{Method: http.MethodPost, Path: "/login/device/code"},
			{Method: http.MethodPost, Path: "/login/oauth/access_token"},
		},
		AccountAuthority: "api.github.com:443", AccountPrefixes: []string{"/copilot_internal/", "/copilot/"},
		AccountRoutes: []RelayRoute{
			// GitHub-backed Copilot owns these read-only account namespaces.
			// The provider validates parameters and decides the account's policy.
			{Method: http.MethodGet, Path: "/copilot_internal/", Prefix: true, Query: true},
			{Method: http.MethodGet, Path: "/copilot/", Prefix: true, Query: true},
		}},
}

// ProviderFor returns a provider by its protocol name, such as "openai".
func ProviderFor(name string) (Provider, bool) {
	for _, p := range providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// Providers are every provider in the table, in the order the gateway
// compositions build their routes.
func Providers() []Provider {
	return slices.Clone(providers)
}

// ServedRoute is one route a provider serves: a model.inference route on
// Authority, or, with Prefix, a path route of an authority another
// capability owns, such as Copilot's account requests on api.github.com. The
// composition attaches the budget tracker and audit writer.
type ServedRoute struct {
	Authority string
	Prefix    string
	Route     gateway.Route
}

// RouteConfig is what every route of a provider is built with.
type RouteConfig struct {
	// Handler holds what every model handler shares. Routes sets its
	// UpstreamURL and ProviderAuth per route.
	Handler HandlerConfig
	// Upstreams replaces the table's origin of a model authority, such as an
	// administrator's private API endpoint or a fixture.
	Upstreams map[string]string
	// SignIn, when set, replaces the provider's sign-in relay.
	SignIn http.Handler
}

// HasAPIAuthority reports whether the provider's first model authority is an
// API a gateway-held key reaches, rather than a signed-in client's host.
func (p Provider) HasAPIAuthority() bool {
	return len(p.Models) > 0 && !p.Models[0].SignedIn
}

// Routes are the routes the provider serves with auth, its one credential
// (ADR 0047). A gateway-held key, token or ChatGPT sign-in serves the
// provider's API authority, or every model authority of a provider with no
// API authority, such as Copilot, with its account requests. When auth
// forwards the harness's own sign-in, every model authority and the account
// requests forward it, and the sign-in relay serves its sign-in and refresh.
// No other authority is served: an authority outside the table has no route,
// and a provider without a credential has none.
func (p Provider) Routes(auth modelauth.Authenticator, c RouteConfig) []ServedRoute {
	if auth == nil {
		return nil
	}
	forwards := auth.Mode() == modelauth.WorkloadMode
	apiRoute := p.HasAPIAuthority()
	route := func(handler http.Handler, credential bool) gateway.Route {
		return gateway.Route{Capability: api.CapabilityModelInference, Tier: audit.TierSemantic, Handler: handler,
			ProviderCredential: credential}
	}
	var served []ServedRoute
	for _, m := range p.Models {
		if m.SignedIn && !forwards && apiRoute {
			continue
		}
		config := c.Handler
		config.UpstreamURL, config.ProviderAuth = m.UpstreamURL, auth
		if upstream, ok := c.Upstreams[m.Authority]; ok {
			config.UpstreamURL = upstream
		}
		served = append(served, ServedRoute{Authority: m.Authority, Route: route(m.New(config), forwards)})
	}
	if p.AccountAuthority != "" && (forwards || !apiRoute) {
		relay := p.AccountRelay(auth, c.Handler.HTTPClient, c.Handler.AuditLogger)
		for _, prefix := range p.AccountPrefixes {
			served = append(served, ServedRoute{Authority: p.AccountAuthority, Prefix: prefix, Route: route(relay, forwards)})
		}
	}
	if forwards && p.SignInAuthority != "" {
		relay := c.SignIn
		if relay == nil {
			relay = p.SignInRelay(c.Handler.HTTPClient, c.Handler.AuditLogger)
		}
		served = append(served, ServedRoute{Authority: p.SignInAuthority, Prefix: p.SignInPrefix, Route: route(relay, false)})
	}
	return served
}

// SignInRelay relays the provider's own sign-in and token refresh.
func (p Provider) SignInRelay(client *http.Client, logger audit.Writer) *Relay {
	return &Relay{UpstreamURL: "https://" + hostOf(p.SignInAuthority), Routes: p.SignInRoutes,
		HTTPClient: client, AuditLogger: logger, Service: "sign-in"}
}

// AccountRelay serves the provider's account requests with auth: a
// gateway-held credential, or the harness's own when auth forwards it.
func (p Provider) AccountRelay(auth modelauth.Authenticator, client *http.Client, logger audit.Writer) *Relay {
	return &Relay{UpstreamURL: "https://" + hostOf(p.AccountAuthority), Routes: p.AccountRoutes,
		HTTPClient: client, AuditLogger: logger, Service: p.Name, Auth: auth}
}

// Hosts are the names the gateway's certificate must cover for the provider:
// every model authority, and the sign-in authority when clients sign in.
func (p Provider) Hosts(signIn bool) []string {
	var hosts []string
	for _, m := range p.Models {
		if signIn || !m.SignedIn {
			hosts = append(hosts, hostOf(m.Authority))
		}
	}
	if signIn && p.SignInAuthority != "" {
		hosts = append(hosts, hostOf(p.SignInAuthority))
	}
	if signIn && p.AccountAuthority != "" {
		hosts = append(hosts, hostOf(p.AccountAuthority))
	}
	return hosts
}

func hostOf(authority string) string {
	for i := len(authority) - 1; i >= 0; i-- {
		if authority[i] == ':' {
			return authority[:i]
		}
	}
	return authority
}
