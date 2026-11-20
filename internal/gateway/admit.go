package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

// Refusal is the gateway's answer to a request it does not admit: a status
// and the JSON body that both model SDK families parse (ADR 0016).
type Refusal struct {
	Status              int
	Type, Code, Message string
	// recorded says whether Refuse writes an audit record.
	recorded bool
}

// Admit checks that the run and its live policy grant capability, then
// selects the capability's meter from the route's budget tracker. It returns
// the request context carrying the meter, or the refusal to give instead.
func Admit(ctx context.Context, capability api.CapabilityKind) (context.Context, *Refusal) {
	identity, _ := IdentityFromContext(ctx)
	route, _ := ctx.Value(routeContextKey{}).(Route)
	if !identity.Permits(capability) {
		reason := "the run does not request capability " + string(capability) +
			"; add it to the run's capabilities"
		if slices.Contains(identity.Capabilities, capability) {
			reason = "policy " + identity.Policy.Name + " does not allow capability " + string(capability) +
				"; add it to the policy's allowedCapabilities"
		}
		return ctx, &Refusal{Status: http.StatusForbidden, Type: "permission_error", Code: "capability_denied",
			Message: reason, recorded: true}
	}
	meter, err := identity.Meter(ctx, route.Budgets, capability)
	if errors.Is(err, ErrBudgetUnavailable) {
		return ctx, &Refusal{Status: http.StatusServiceUnavailable, Type: "api_error", Code: "budget_unavailable",
			Message: "budget accounting for " + string(capability) + " is unavailable; retry later"}
	}
	if err != nil {
		return ctx, &Refusal{Status: http.StatusForbidden, Type: "permission_error", Code: "allocation_denied",
			Message: "the " + string(capability) + " allocation for this run is missing or exhausted"}
	}
	return budget.WithMeter(ctx, meter), nil
}

// Refuse records r through the route in ctx and writes the JSON body.
func Refuse(ctx context.Context, w http.ResponseWriter, r *Refusal) {
	route, _ := ctx.Value(routeContextKey{}).(Route)
	if r.recorded && route.AuditLogger != nil {
		event := AuditEvent(ctx, string(route.Capability))
		event.DenyReason = r.Message
		route.AuditLogger.Log(event)
	}
	writeDenial(w, r.Status, r.Type, r.Code, r.Message)
}
