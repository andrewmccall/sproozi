package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// Route declares the guarantee and capability of an exact upstream authority.
// It is the single source of both: the Dispatcher enforces the capability and
// AuditEvent copies both into every handler's record. Authentication belongs
// to proxytransport; handlers authorize the operation.
type Route struct {
	Capability  string
	Tier        audit.EnforcementTier
	Handler     http.Handler
	Budgets     *budget.Tracker
	AuditLogger audit.Writer
	// ProviderCredential marks a model route that forwards the workload's own
	// provider credential. LeakGuard refuses that credential everywhere else.
	ProviderCredential bool
	// Paths serve path prefixes of the authority with their own capability
	// and handler, such as Copilot's account requests on api.github.com,
	// which the GitHub capability otherwise serves.
	Paths []PathRoute
}

// PathRoute serves the requests under Prefix, a clean path ending in "/".
type PathRoute struct {
	Prefix string
	Route  Route
}

// Select returns the route serving path on authority, and a key that names
// it for LeakGuard. A path that isn't clean never selects a path route.
func (d Dispatcher) Select(authority, requestPath string) (Route, string, bool) {
	key := normalizeAuthority(authority)
	route, ok := d[key]
	if !ok {
		return Route{}, "", false
	}
	if requestPath == path.Clean(requestPath) {
		for _, p := range route.Paths {
			if strings.HasPrefix(requestPath, p.Prefix) {
				return p.Route, key + p.Prefix, true
			}
		}
	}
	return route, key, true
}

type Dispatcher map[string]Route

type routeContextKey struct{}

func (d Dispatcher) Allows(authority string) bool {
	route, ok := d[normalizeAuthority(authority)]
	return ok && (route.Handler != nil || slices.ContainsFunc(route.Paths, func(p PathRoute) bool { return p.Route.Handler != nil }))
}

// ServeHTTP preserves streaming, flushing and response lifetime. Buffering, when
// required for validation, belongs to the individual capability implementation.
func (d Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeDenial(w, http.StatusUnauthorized, "authentication_error", "identity_required",
			"gateway identity required; connect through the gateway proxy with the run's token")
		return
	}
	route, _, ok := d.Select(r.Host, r.URL.Path)
	if !ok || route.Handler == nil {
		writeDenial(w, http.StatusForbidden, "permission_error", "destination_denied",
			"destination "+r.Host+" is not enabled at this gateway")
		return
	}
	ctx := context.WithValue(r.Context(), routeContextKey{}, route)
	// Named MCP routes select their capability after parsing the server name
	// and check it with the same Permits.
	if route.Capability != "" {
		capability := sprooziv1alpha1.CapabilityKind(route.Capability)
		if !identity.Permits(capability) {
			reason := "the run does not request capability " + route.Capability +
				"; add it to the run's capabilities"
			if slices.Contains(identity.Capabilities, capability) {
				reason = "policy " + identity.Policy.Name + " does not allow capability " + route.Capability +
					"; add it to the policy's allowedCapabilities"
			}
			if route.AuditLogger != nil {
				event := AuditEvent(ctx, route.Capability)
				event.DenyReason = reason
				route.AuditLogger.Log(event)
			}
			writeDenial(w, http.StatusForbidden, "permission_error", "capability_denied", reason)
			return
		}
		meter, err := identity.Meter(ctx, route.Budgets, capability)
		if errors.Is(err, ErrBudgetUnavailable) {
			writeDenial(w, http.StatusServiceUnavailable, "api_error", "budget_unavailable",
				"budget accounting for "+route.Capability+" is unavailable; retry later")
			return
		}
		if err != nil {
			writeDenial(w, http.StatusForbidden, "permission_error", "allocation_denied",
				"the "+route.Capability+" allocation for this run is missing or exhausted")
			return
		}
		ctx = budget.WithMeter(ctx, meter)
	}
	route.Handler.ServeHTTP(w, r.WithContext(ctx))
}

// writeDenial writes the one error body that both the Anthropic and OpenAI
// SDK families parse, so a gateway denial reaches the agent as a readable error.
func writeDenial(w http.ResponseWriter, status int, errorType, code, message string) {
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body.Type, body.Error.Type, body.Error.Message, body.Error.Code = "error", errorType, message, code
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func normalizeAuthority(authority string) string {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if authority != "" && !strings.Contains(authority, ":") {
		return authority + ":443"
	}
	return authority
}
