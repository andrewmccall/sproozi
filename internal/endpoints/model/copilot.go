package model

import "net/http"

const (
	// Copilot's model API, one host per plan, that a client signed in to
	// GitHub calls (ADR 0047).
	CopilotIndividualAuthority = "api.individual.githubcopilot.com:443"
	CopilotBusinessAuthority   = "api.business.githubcopilot.com:443"
	CopilotEnterpriseAuthority = "api.enterprise.githubcopilot.com:443"
	// ProviderCopilot names GitHub Copilot's model API.
	ProviderCopilot = "copilot"
)

// copilotRoutes maps Copilot's model call paths to the inspected operations
// that serve them: OpenAI Responses, Anthropic Messages and chat completions.
var copilotRoutes = map[string]string{
	"/responses":        responsesPath,
	"/chat/completions": "/v1/chat/completions",
}

// copilot serves one Copilot model API host. Model calls pass through the
// OpenAI or Anthropic model handler, which inspects and counts them; the model
// list and automatic model choice pass through a Relay; everything else,
// including GitHub's built-in MCP server, agents and telemetry, is refused.
type copilot struct {
	openai, anthropic *Handler
	relay             *Relay
}

// NewCopilotHandler serves a Copilot model API host for a client that signed
// in to GitHub. config.UpstreamURL is that host's origin.
func NewCopilotHandler(config HandlerConfig) http.Handler {
	openai := &Handler{Config: config}
	openai.Config.UpstreamPath = func(path string) string {
		for copilotPath, inspected := range copilotRoutes {
			if path == inspected {
				return copilotPath
			}
		}
		return path
	}
	anthropic := NewAnthropicHandler(config)
	anthropic.Config.KeepClientHeaders = true
	return &copilot{openai: openai, anthropic: anthropic, relay: &Relay{UpstreamURL: config.UpstreamURL,
		HTTPClient: config.HTTPClient, AuditLogger: config.AuditLogger, Service: ProviderCopilot,
		Routes: []RelayRoute{{Method: http.MethodGet, Path: "/models"}, {Method: http.MethodPost, Path: "/auto"}}}}
}

func (c *copilot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == anthropicMessagesPath {
		c.anthropic.ServeHTTP(w, r)
		return
	}
	if inspected, ok := copilotRoutes[r.URL.Path]; ok && r.Method == http.MethodPost {
		call := r.Clone(r.Context())
		call.URL.Path, call.URL.RawPath = inspected, ""
		c.openai.ServeHTTP(w, call)
		return
	}
	c.relay.ServeHTTP(w, r)
}
