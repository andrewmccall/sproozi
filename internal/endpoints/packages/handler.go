// Package packagesgateway enforces the packages.install semantic capability.
package packages

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/andrewmccall/sproozi/internal/budget"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
	packagepolicy "github.com/andrewmccall/sproozi/internal/packages"
)

const (
	pythonFilesHost = "files.pythonhosted.org"
)

type HandlerConfig struct {
	AuditLogger audit.Writer
	HTTPClient  *http.Client
}
type Handler struct{ Config HandlerConfig }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := gateway.IdentityFromContext(r.Context())
	if !ok {
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return
	}
	e := id.AuditEvent("packages.install")
	e.Service, e.SemanticLevel, e.Capability = "pypi", audit.TierSemantic, string(api.CapabilityPackagesInstall)
	if !slices.Contains(id.Run.Spec.Capabilities, api.CapabilityPackagesInstall) || !slices.Contains(id.Policy.Spec.AllowedCapabilities, api.CapabilityPackagesInstall) {
		h.deny(w, e, http.StatusForbidden, "packages.install capability not granted")
		return
	}
	h.handlePackage(w, r, e, id)
}
func (h *Handler) handlePackage(w http.ResponseWriter, r *http.Request, e audit.Event, identity *gateway.RunIdentity) {
	decision := packagepolicy.AuthorizePath(identity.Policy.Spec.PackagesInstall, packagepolicy.Request{Method: r.Method, Host: r.URL.Hostname(), Path: r.URL.Path})
	if !decision.Allowed {
		h.deny(w, e, http.StatusForbidden, "package request denied: "+decision.Reason)
		return
	}
	if h.Config.HTTPClient == nil {
		h.deny(w, e, http.StatusServiceUnavailable, "package gateway unavailable")
		return
	}
	upstreamURL, err := packageUpstreamURL("https://pypi.org", r.URL.Hostname(), r.URL.Path)
	if err != nil || r.URL.RawQuery != "" {
		h.deny(w, e, http.StatusForbidden, "package URL is not canonical")
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build package request")
		return
	}
	// Never let the generic client follow a registry redirect. Each redirect
	// target must be parsed and authorized against the package lock before a new
	// request is made; this handler intentionally has no such implicit path.
	packageClient := *h.Config.HTTPClient
	packageClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if err := budget.FromContext(r.Context()).SpendRequest(); err != nil {
		h.deny(w, e, http.StatusTooManyRequests, err.Error())
		return
	}
	resp, err := packageClient.Do(upstreamReq)
	if err != nil {
		h.deny(w, e, http.StatusBadGateway, "package upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		h.deny(w, e, http.StatusBadGateway, "package redirect requires explicit revalidation")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.copyResponse(w, resp, nil)
		h.auditAllowed(e, resp.StatusCode)
		return
	}
	if decision.Artifact != nil {
		body, verifyErr := packagepolicy.VerifyArtifact(identity.Policy.Spec.PackagesInstall, decision.Artifact, resp.Body)
		if verifyErr != nil {
			h.deny(w, e, http.StatusBadGateway, "package artifact verification failed")
			return
		}
		h.copyResponse(w, resp, body)
		h.auditAllowed(e, resp.StatusCode)
		return
	}
	// Metadata is bounded and every advertised wheel link must be in the
	// administrator's lock. pip may request the resulting artifact separately,
	// where VerifyArtifact performs the byte-level digest check.
	body, verifyErr := packagepolicy.VerifyMetadata(identity.Policy.Spec.PackagesInstall, resp.Body)
	if verifyErr != nil {
		h.deny(w, e, http.StatusBadGateway, "package metadata is invalid")
		return
	}
	h.copyResponse(w, resp, body)
	h.auditAllowed(e, resp.StatusCode)
}

// packageUpstreamURL preserves the registry host selected by the client. A
// PyPI simple response commonly points at files.pythonhosted.org, so routing
// every request through PackageBaseURL (normally pypi.org) silently changes
// the destination identity and can make the package lock meaningless.
// PackageBaseURL supplies only the administrator-owned scheme and deployment
// endpoint; the host is always taken from the already-authorized request.
func packageUpstreamURL(base, requestHost, path string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme != "https" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return "", fmt.Errorf("package base URL is not canonical")
	}
	if requestHost != "pypi.org" && requestHost != pythonFilesHost {
		return "", fmt.Errorf("package host is not approved")
	}
	baseURL.Host = requestHost
	baseURL.Path = ""
	return url.JoinPath(baseURL.String(), path)
}

func (h *Handler) copyResponse(w http.ResponseWriter, response *http.Response, body []byte) {
	for k, vv := range response.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}
func (h *Handler) auditAllowed(e audit.Event, status int) {
	e.Allowed = true
	e.UpstreamStatus = status
	h.Config.AuditLogger.Log(e)
}

// deny writes a plain-text HTTP error and logs a denied audit event.
func (h *Handler) deny(w http.ResponseWriter, e audit.Event, status int, reason string) {
	e.Allowed = false
	e.DenyReason = reason
	h.Config.AuditLogger.Log(e)
	http.Error(w, reason, status)
}
