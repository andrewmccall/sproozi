package gateway

import (
	"errors"
	"net/http"
	"strings"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// Route declares the guarantee and capability of an exact upstream authority.
// Authentication belongs to proxytransport; handlers authorize the operation.
type Route struct {
	Capability string
	Tier       audit.EnforcementTier
	Handler    http.Handler
	Budgets    *budget.Tracker
}

type Dispatcher map[string]Route

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
	ctx := r.Context()
	// Named MCP routes select their capability after parsing the server name.
	if route.Capability != "" {
		meter, err := identity.Meter(ctx, route.Budgets, sprooziv1alpha1.CapabilityKind(route.Capability))
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
	clean := r.Clone(ctx)
	clean.Header.Del("Authorization")
	clean.Header.Del("Proxy-Authorization")
	clean.Header.Del("Cookie")
	route.Handler.ServeHTTP(w, clean)
}

func normalizeAuthority(authority string) string {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if authority != "" && !strings.Contains(authority, ":") {
		return authority + ":443"
	}
	return authority
}
