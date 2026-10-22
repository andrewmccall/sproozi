package model

import (
	"bytes"
	"net/http"
	"slices"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	// AnthropicSignInAuthority serves Claude Code's OAuth sign-in and refresh.
	AnthropicSignInAuthority = "platform.claude.com:443"
	anthropicTokenPath       = "/v1/oauth/token"
	maxSignInBytes           = 64 << 10
)

// SignInHandler relays a harness's own OAuth sign-in and token refresh to its
// provider unchanged (ADR 0047). It holds no credential, charges no budget and
// never records a body: both directions carry the harness's tokens.
type SignInHandler struct {
	UpstreamURL string
	Paths       []string
	HTTPClient  *http.Client
	AuditLogger audit.Writer
}

// NewAnthropicSignInHandler relays only Claude Code's OAuth token endpoint.
func NewAnthropicSignInHandler(upstreamURL string, client *http.Client, logger audit.Writer) *SignInHandler {
	return &SignInHandler{UpstreamURL: upstreamURL, Paths: []string{anthropicTokenPath},
		HTTPClient: client, AuditLogger: logger}
}

func (h *SignInHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e := gateway.AuditEvent(r.Context(), r.URL.Path)
	e.Service = "sign-in"
	if r.Method != http.MethodPost || !slices.Contains(h.Paths, r.URL.Path) || r.URL.RawQuery != "" {
		h.refuse(w, e, http.StatusForbidden, "only the provider's sign-in token request is relayed")
		return
	}
	body, err := readBounded(r.Body, maxSignInBytes)
	if err != nil {
		h.refuse(w, e, http.StatusRequestEntityTooLarge, "sign-in request exceeds gateway limit")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.UpstreamURL+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		h.refuse(w, e, http.StatusInternalServerError, "failed to build sign-in request")
		return
	}
	for _, name := range []string{"Content-Type", "Accept", "User-Agent"} {
		if value := r.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	client := *h.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(upstream)
	if err != nil {
		h.refuse(w, e, http.StatusBadGateway, "sign-in request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := readBounded(resp.Body, maxSignInBytes)
	if err != nil {
		h.refuse(w, e, http.StatusBadGateway, "sign-in response exceeds gateway limit")
		return
	}
	e.Allowed, e.UpstreamStatus = true, resp.StatusCode
	h.log(e)
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(reply)
}

func (h *SignInHandler) refuse(w http.ResponseWriter, e audit.Event, status int, reason string) {
	e.DenyReason = reason
	h.log(e)
	http.Error(w, reason, status)
}

func (h *SignInHandler) log(e audit.Event) {
	if h.AuditLogger != nil {
		h.AuditLogger.Log(e)
	}
}
