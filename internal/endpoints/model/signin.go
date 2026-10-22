package model

import (
	"bytes"
	"net/http"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	// AnthropicSignInAuthority serves Claude Code's OAuth sign-in and refresh.
	AnthropicSignInAuthority = "platform.claude.com:443"
	// OpenAISignInAuthority serves Codex's ChatGPT device-code sign-in and refresh.
	OpenAISignInAuthority = "auth.openai.com:443"
	maxRelayRequestBytes  = 64 << 10
	maxRelayResponseBytes = 8 << 20
)

// RelayRoute is one request a Relay passes on: an exact method and path,
// and whether a query string is allowed.
type RelayRoute struct {
	Method, Path string
	Query        bool
}

// Relay passes a harness's own sign-in, token refresh and account metadata
// requests to its provider unchanged (ADR 0047). It holds no credential,
// charges no budget and never records a body, because both directions carry
// the harness's tokens. Every other request is refused.
type Relay struct {
	UpstreamURL string
	Routes      []RelayRoute
	HTTPClient  *http.Client
	AuditLogger audit.Writer
	Service     string
}

// NewAnthropicSignInHandler relays Anthropic's sign-in routes to upstreamURL,
// such as a fixture.
func NewAnthropicSignInHandler(upstreamURL string, client *http.Client, logger audit.Writer) *Relay {
	relay := providers[ProviderAnthropic].SignInRelay(client, logger)
	relay.UpstreamURL = upstreamURL
	return relay
}

func (h *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e := gateway.AuditEvent(r.Context(), r.URL.Path)
	e.Service = h.Service
	allowed := slices.ContainsFunc(h.Routes, func(route RelayRoute) bool {
		return route.Method == r.Method && route.Path == r.URL.Path && (route.Query || r.URL.RawQuery == "")
	})
	if !allowed {
		h.refuse(w, e, http.StatusForbidden, "the gateway relays only the provider's sign-in and account requests")
		return
	}
	body, err := readBounded(r.Body, maxRelayRequestBytes)
	if err != nil {
		h.refuse(w, e, http.StatusRequestEntityTooLarge, "relayed request exceeds gateway limit")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, h.UpstreamURL+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		h.refuse(w, e, http.StatusInternalServerError, "failed to build relayed request")
		return
	}
	for name, values := range r.Header {
		switch strings.ToLower(name) {
		case "cookie", "proxy-authorization", "accept-encoding", "connection", "content-length":
			continue
		}
		upstream.Header[name] = values
	}
	client := *h.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(upstream)
	if err != nil {
		h.refuse(w, e, http.StatusBadGateway, "relayed request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := readBounded(resp.Body, maxRelayResponseBytes)
	if err != nil {
		h.refuse(w, e, http.StatusBadGateway, "relayed response exceeds gateway limit")
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

func (h *Relay) refuse(w http.ResponseWriter, e audit.Event, status int, reason string) {
	e.DenyReason = reason
	h.log(e)
	http.Error(w, reason, status)
}

func (h *Relay) log(e audit.Event) {
	if h.AuditLogger != nil {
		h.AuditLogger.Log(e)
	}
}
