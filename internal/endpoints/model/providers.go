package model

import (
	"net/http"

	"github.com/andrewmccall/sproozi/internal/modelauth"

	"github.com/andrewmccall/sproozi/internal/audit"
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
	// TokenHeaders are set with a gateway-held sign-in token, such as the beta
	// header Anthropic requires for an OAuth token.
	TokenHeaders map[string]string
	// AccountAuthority, AccountPrefix and AccountRoutes are the account and
	// policy requests a signed-in client makes on a host another capability
	// owns, such as Copilot's on api.github.com. Only these paths are served.
	AccountAuthority, AccountPrefix string
	AccountRoutes                   []RelayRoute
}

// Provider names, as harness model protocols spell them.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

var providers = map[string]Provider{
	ProviderOpenAI: {Name: ProviderOpenAI,
		Models: []ModelRoute{
			{Authority: OpenAIAuthority, UpstreamURL: "https://api.openai.com",
				New: func(c HandlerConfig) http.Handler { return &Handler{Config: c} }},
			{Authority: ChatGPTBackendAuthority, UpstreamURL: "https://chatgpt.com", SignedIn: true,
				New: NewChatGPTBackendHandler},
		},
		SignInAuthority: OpenAISignInAuthority,
		SignInRoutes: []RelayRoute{
			{Method: http.MethodPost, Path: "/oauth/token"},
			{Method: http.MethodPost, Path: "/api/accounts/deviceauth/usercode"},
			{Method: http.MethodPost, Path: "/api/accounts/deviceauth/token"},
		}},
	ProviderAnthropic: {Name: ProviderAnthropic,
		Models: []ModelRoute{
			{Authority: AnthropicAuthority, UpstreamURL: "https://api.anthropic.com",
				New: func(c HandlerConfig) http.Handler { return NewAnthropicHandler(c) }},
		},
		SignInAuthority: AnthropicSignInAuthority,
		SignInRoutes:    []RelayRoute{{Method: http.MethodPost, Path: "/v1/oauth/token"}},
		TokenHeaders:    map[string]string{"Anthropic-Beta": "oauth-2025-04-20"}},
	// Copilot has no API-key host: a client signs in to GitHub, and its
	// sign-in or a gateway-held GitHub token reaches its plan's host.
	ProviderCopilot: {Name: ProviderCopilot,
		Models: []ModelRoute{
			{Authority: CopilotIndividualAuthority, UpstreamURL: "https://api.individual.githubcopilot.com", SignedIn: true,
				New: NewCopilotHandler},
			{Authority: CopilotBusinessAuthority, UpstreamURL: "https://api.business.githubcopilot.com", SignedIn: true,
				New: NewCopilotHandler},
			{Authority: CopilotEnterpriseAuthority, UpstreamURL: "https://api.enterprise.githubcopilot.com", SignedIn: true,
				New: NewCopilotHandler},
		},
		AccountAuthority: "api.github.com:443", AccountPrefix: "/copilot_internal/",
		AccountRoutes: []RelayRoute{
			{Method: http.MethodGet, Path: "/copilot_internal/user"},
			{Method: http.MethodGet, Path: "/copilot_internal/managed_settings"},
			{Method: http.MethodGet, Path: "/copilot_internal/team_customizations"},
		}},
}

// ProviderFor returns a provider by its protocol name, such as "openai".
func ProviderFor(name string) (Provider, bool) {
	p, ok := providers[name]
	return p, ok
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
