package model

import (
	"net/http"
)

const (
	// ChatGPTBackendAuthority serves a ChatGPT-signed-in Codex (ADR 0047).
	ChatGPTBackendAuthority  = "chatgpt.com:443"
	chatGPTBackendResponses  = "/backend-api/codex/responses"
	chatGPTBackendModels     = "/backend-api/codex/models"
	chatGPTBackendAccounts   = "/backend-api/wham/accounts/check"
	chatGPTBackendUserConfig = "/backend-api/wham/settings/user"
)

// chatGPTBackend serves the ChatGPT backend a Codex signed in with ChatGPT
// calls. Model calls pass through the model handler, which inspects and counts
// them; the account and model metadata Codex needs before a session pass
// through a Relay; everything else, including ChatGPT's own plugins and
// connector MCP, analytics and usage, is refused.
type chatGPTBackend struct {
	model *Handler
	relay *Relay
}

// NewChatGPTBackendHandler serves chatgpt.com for a Codex that signed in
// itself. config.UpstreamURL is the backend's origin, such as
// https://chatgpt.com.
func NewChatGPTBackendHandler(config HandlerConfig) http.Handler {
	model := &Handler{Config: config}
	model.Config.UpstreamPath = func(path string) string {
		if path == responsesPath {
			return chatGPTBackendResponses
		}
		return path
	}
	var logger = config.AuditLogger
	relay := &Relay{UpstreamURL: config.UpstreamURL, HTTPClient: config.HTTPClient, AuditLogger: logger, Service: "chatgpt",
		Routes: []RelayRoute{
			{Method: http.MethodGet, Path: chatGPTBackendModels, Query: true},
			{Method: http.MethodGet, Path: chatGPTBackendAccounts, Query: true},
			{Method: http.MethodGet, Path: chatGPTBackendUserConfig, Query: true},
		}}
	return &chatGPTBackend{model: model, relay: relay}
}

func (b *chatGPTBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == chatGPTBackendResponses {
		call := r.Clone(r.Context())
		call.URL.Path, call.URL.RawPath = responsesPath, ""
		b.model.ServeHTTP(w, call)
		return
	}
	b.relay.ServeHTTP(w, r)
}
