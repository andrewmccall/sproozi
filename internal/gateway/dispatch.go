package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// Admission says who admits a route's requests (ADR 0070).
type Admission uint8

const (
	// AdmitCapability, the zero value: the dispatcher checks and meters the
	// route's Capability, which must be set.
	AdmitCapability Admission = iota
	// AdmitSelected: the handler selects a capability from the request and
	// checks it before any other work, as named MCP does. Reason names the
	// handler's check.
	AdmitSelected
	// AdmitOpen: the route needs no capability by decision. Reason names the
	// record that decided it.
	AdmitOpen
)

// Route declares the guarantee and capability of an exact upstream authority.
// It is the single source of both: the Dispatcher enforces the capability and
// AuditEvent copies both into every handler's record. Authentication belongs
// to proxytransport; handlers authorize the operation.
type Route struct {
	Capability sprooziv1alpha1.CapabilityKind
	// Admission says who admits a request; Validate refuses a route with a
	// handler and no valid declaration.
	Admission Admission
	// Reason names the check or record behind AdmitSelected or AdmitOpen.
	Reason      string
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

// Validate refuses a route or path route that has a handler and no valid
// admission: a capability for AdmitCapability, or no capability and a Reason
// for AdmitSelected and AdmitOpen. A route without a handler is a reserved,
// closed authority. Compositions call it before serving.
func (d Dispatcher) Validate() error {
	var problems []string
	for authority, route := range d {
		if err := route.validate(); err != nil {
			problems = append(problems, authority+": "+err.Error())
		}
		for _, p := range route.Paths {
			if err := p.Route.validate(); err != nil {
				problems = append(problems, authority+p.Prefix+": "+err.Error())
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("gateway: routes without a valid admission: %s", strings.Join(problems, "; "))
}

func (r Route) validate() error {
	if r.Handler == nil {
		return nil
	}
	switch r.Admission {
	case AdmitCapability:
		if r.Capability == "" {
			return errors.New("serves without a capability; set Capability, or declare AdmitSelected or AdmitOpen with a Reason")
		}
	case AdmitSelected, AdmitOpen:
		if r.Capability != "" {
			return fmt.Errorf("declares capability %s, which the dispatcher would not check", r.Capability)
		}
		if strings.TrimSpace(r.Reason) == "" {
			return errors.New("needs a Reason naming who admits its requests")
		}
	default:
		return fmt.Errorf("has unknown admission %d", r.Admission)
	}
	return nil
}

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
	if route.Admission == AdmitCapability && route.Capability != "" {
		capability := route.Capability
		if !identity.Permits(capability) {
			reason := "the run does not request capability " + string(capability) +
				"; add it to the run's capabilities"
			if slices.Contains(identity.Capabilities, capability) {
				reason = "policy " + identity.Policy.Name + " does not allow capability " + string(capability) +
					"; add it to the policy's allowedCapabilities"
			}
			if route.AuditLogger != nil {
				event := AuditEvent(ctx, string(capability))
				event.DenyReason = reason
				route.AuditLogger.Log(event)
			}
			writeDenial(w, http.StatusForbidden, "permission_error", "capability_denied", reason)
			return
		}
		meter, err := identity.Meter(ctx, route.Budgets, capability)
		if errors.Is(err, ErrBudgetUnavailable) {
			writeDenial(w, http.StatusServiceUnavailable, "api_error", "budget_unavailable",
				"budget accounting for "+string(capability)+" is unavailable; retry later")
			return
		}
		if err != nil {
			writeDenial(w, http.StatusForbidden, "permission_error", "allocation_denied",
				"the "+string(capability)+" allocation for this run is missing or exhausted")
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
