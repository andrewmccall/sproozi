package github

import (
	"net/http"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

type identityConfig struct {
	// Identity is a pre-authenticated Run identity for in-process callers. The
	// workload-facing shared gateway installs identity in request context instead.
	Identity *gateway.RunIdentity
	// TokenProvider provides GitHub App installation tokens.
	TokenProvider TokenProvider

	// AuditLogger writes structured audit events for every decision.
	AuditLogger audit.Writer

	// GitBaseURL is the upstream for git smart HTTP (e.g. "https://github.com").
	GitBaseURL string

	// APIBaseURL is the upstream for GitHub REST API (e.g. "https://api.github.com").
	APIBaseURL string

	// HTTPClient is the HTTP client used to forward requests upstream.
	HTTPClient *http.Client
}
type identityHandler struct{ Config identityConfig }

func (h *identityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Config.Identity != nil {
		r = r.WithContext(gateway.WithIdentity(r.Context(), h.Config.Identity))
	}
	real := &Handler{Config: HandlerConfig{TokenProvider: h.Config.TokenProvider, AuditLogger: h.Config.AuditLogger, GitBaseURL: h.Config.GitBaseURL, APIBaseURL: h.Config.APIBaseURL, HTTPClient: h.Config.HTTPClient}}
	real.ServeHTTP(w, r)
}
