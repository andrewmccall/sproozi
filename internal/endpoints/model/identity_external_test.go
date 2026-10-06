package model_test

import (
	"net/http"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"

	"github.com/andrewmccall/sproozi/internal/audit"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

type identityConfig struct {
	// Identity is a pre-authenticated Run identity for in-process callers. The
	// workload-facing shared gateway installs identity in request context instead.
	Identity *gateway.RunIdentity
	// budget.Tracker tracks and enforces per-run token budgets.
	BudgetTracker *budget.Tracker

	// Pricing is administrator-owned and is required whenever a cost cap is set.
	Pricing *modelgateway.PricingTable

	// AuditLogger writes structured audit events for each gateway decision.
	AuditLogger audit.Writer

	// UpstreamURL is the base URL of the upstream model provider.
	UpstreamURL string

	// OpenAIKey is the API key used to authenticate with the upstream provider.
	OpenAIKey string

	// ProviderAuth is administrator-selected upstream authentication. OpenAIKey
	// remains the API-key fallback for existing in-process integrations.
	ProviderAuth modelauth.Authenticator

	// HTTPClient is the HTTP client used for upstream requests.
	HTTPClient *http.Client

	// Diagnostics is an administrator-enabled provider-response observer. It
	// receives bounded, redacted errors, never prompts or model output deltas.
	// This does not bypass usage verification or change budget settlement.
	Diagnostics func(modelgateway.ResponseDiagnostic)
}
type identityHandler struct{ Config identityConfig }

func (h *identityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Config.Identity != nil {
		r = r.WithContext(gateway.WithIdentity(r.Context(), h.Config.Identity))
		if limits, ok := h.Config.Identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]; ok {
			r = r.WithContext(budget.WithMeter(r.Context(), h.Config.BudgetTracker.Meter(string(h.Config.Identity.Run.UID), limits)))
		}
	}
	if h.Config.ProviderAuth == nil {
		h.Config.ProviderAuth = modelauth.APIKey{Key: h.Config.OpenAIKey}
	}
	real := &modelgateway.Handler{Config: modelgateway.HandlerConfig{Pricing: h.Config.Pricing, AuditLogger: h.Config.AuditLogger, UpstreamURL: h.Config.UpstreamURL, ProviderAuth: h.Config.ProviderAuth, HTTPClient: h.Config.HTTPClient, Diagnostics: h.Config.Diagnostics}}
	real.ServeHTTP(w, r)
}
