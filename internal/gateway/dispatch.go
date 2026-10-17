package gateway

import (
	"context"
	"errors"
	"net/http"
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
}

type Dispatcher map[string]Route

type routeContextKey struct{}

func (d Dispatcher) Allows(authority string) bool {
	route, ok := d[normalizeAuthority(authority)]
	return ok && route.Handler != nil
}

// ServeHTTP preserves streaming, flushing and response lifetime. Buffering, when
// required for validation, belongs to the individual capability implementation.
func (d Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return
	}
	route, ok := d[normalizeAuthority(r.Host)]
	if !ok || route.Handler == nil {
		http.Error(w, "gateway destination denied", http.StatusForbidden)
		return
	}
	ctx := context.WithValue(r.Context(), routeContextKey{}, route)
	// Named MCP routes select their capability after parsing the server name
	// and check it with the same Permits.
	if route.Capability != "" {
		capability := sprooziv1alpha1.CapabilityKind(route.Capability)
		if !identity.Permits(capability) {
			if route.AuditLogger != nil {
				event := AuditEvent(ctx, route.Capability)
				event.DenyReason = "capability not granted"
				route.AuditLogger.Log(event)
			}
			http.Error(w, route.Capability+" capability not granted", http.StatusForbidden)
			return
		}
		meter, err := identity.Meter(route.Budgets, capability)
		if err != nil {
			status := http.StatusForbidden
			if errors.Is(err, ErrBudgetUnavailable) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, "budget allocation denied or unavailable", status)
			return
		}
		ctx = budget.WithMeter(ctx, meter)
	}
	route.Handler.ServeHTTP(w, r.WithContext(ctx))
}

func normalizeAuthority(authority string) string {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if authority != "" && !strings.Contains(authority, ":") {
		return authority + ":443"
	}
	return authority
}
