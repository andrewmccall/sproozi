/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package egress implements inspected HTTPS destination enforcement
// that enforces template/policy-approved destination profiles. Every decision
// is audited; request bodies and credential-bearing URLs are never logged.
package destination

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// hopByHopHeaders are stripped from forwarded HTTP requests.
// Authorization is included so sandbox tokens never reach registries.
var hopByHopHeaders = []string{
	"Authorization",
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// HandlerConfig holds all dependencies for Handler.
type HandlerConfig struct {
	Profiles    *ProfileStore
	AuditLogger audit.Writer
	HTTPClient  *http.Client
}

// Handler is the HTTP handler for the egress gateway.
type Handler struct {
	Config HandlerConfig
}

// ServeHTTP authorizes the run and forwards inspected HTTPS to approved destinations.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	e := audit.Event{Timestamp: time.Now().UTC(), Operation: r.Method + " " + r.Host}

	identity, ok := gateway.IdentityFromContext(ctx)
	if !ok {
		h.deny(w, e, http.StatusUnauthorized, "gateway identity required")
		return
	}
	e = identity.AuditEvent(r.Method + " " + r.Host)
	e.Capability, e.SemanticLevel, e.Service = string(sprooziv1alpha1.CapabilityNetworkEgress), audit.TierDestination, "network"

	if !slices.Contains(identity.Run.Spec.Capabilities, sprooziv1alpha1.CapabilityNetworkEgress) ||
		!slices.Contains(identity.Policy.Spec.AllowedCapabilities, sprooziv1alpha1.CapabilityNetworkEgress) {
		h.deny(w, e, http.StatusForbidden, "network.egress capability not granted")
		return
	}

	dests := h.Config.Profiles.ActiveDestinations(
		identity.Template.Spec.EgressProfiles,
		identity.Policy.Spec.EgressProfiles,
	)

	if r.Method == http.MethodConnect || r.URL == nil || r.URL.Scheme != httpsScheme || r.URL.Host == "" {
		h.deny(w, e, http.StatusForbidden, "inspected HTTPS request required")
		return
	}
	h.handleHTTP(w, r, e, dests)
}

func (h *Handler) handleHTTP(w http.ResponseWriter, r *http.Request, e audit.Event, dests []Destination) {
	defaultPort := "80"
	if r.URL.Scheme == httpsScheme {
		defaultPort = "443"
	}

	host, port, err := parseAuthority(r.URL.Host, defaultPort)
	if err != nil {
		h.deny(w, e, http.StatusBadRequest, err.Error())
		return
	}

	e.Operation = fmt.Sprintf("egress.http %s://%s:%d", r.URL.Scheme, host, port)

	if !isApproved(r.URL.Scheme, host, port, dests) {
		h.deny(w, e, http.StatusForbidden,
			fmt.Sprintf("destination %s://%s:%d not in approved egress profiles", r.URL.Scheme, host, port))
		return
	}

	targetURL := *r.URL
	targetURL.Host = fmt.Sprintf("%s:%d", host, port)

	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), r.Body)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	copyNonHopHeaders(upstream.Header, r.Header)

	// Redirects are a module invariant, not a property callers may weaken by
	// injecting a default http.Client. A redirect changes the destination tuple
	// and must return to authorization at the proxy boundary.
	client := *h.Config.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if err := budget.FromContext(r.Context()).SpendRequest(); err != nil {
		h.deny(w, e, http.StatusTooManyRequests, err.Error())
		return
	}
	resp, err := client.Do(upstream)
	if err != nil {
		h.deny(w, e, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	e.Allowed = true
	e.UpstreamStatus = resp.StatusCode
	h.Config.AuditLogger.Log(e)
}

func copyNonHopHeaders(dst, src http.Header) {
	drop := make(map[string]bool)
	for _, v := range src["Connection"] {
		drop[strings.ToLower(strings.TrimSpace(v))] = true
	}
	for k, vv := range src {
		if drop[strings.ToLower(k)] {
			continue
		}
		if slices.ContainsFunc(hopByHopHeaders, func(h string) bool {
			return strings.EqualFold(k, h)
		}) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (h *Handler) deny(w http.ResponseWriter, e audit.Event, status int, reason string) {
	e.Allowed = false
	e.DenyReason = reason
	h.Config.AuditLogger.Log(e)
	http.Error(w, reason, status)
}
