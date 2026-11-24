package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
)

// Refusal is the gateway's answer to a request it does not admit: a status
// and the JSON body that both model SDK families parse (ADR 0016).
type Refusal struct {
	Outcome             audit.Outcome
	Status              int
	Type, Code, Message string
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
		return ctx, &Refusal{Outcome: audit.RefusedPolicy, Status: http.StatusForbidden, Type: "permission_error",
			Code: "capability_denied", Message: reason}
	}
	meter, err := identity.Meter(ctx, route.Budgets, capability)
	if errors.Is(err, ErrBudgetUnavailable) {
		return ctx, &Refusal{Outcome: audit.Unavailable, Status: http.StatusServiceUnavailable, Type: "api_error",
			Code: "budget_unavailable", Message: "budget accounting for " + string(capability) + " is unavailable; retry later"}
	}
	if err != nil {
		// A missing or exhausted allocation refuses the request; a managed
		// account that cannot be read is answered the same way.
		outcome := audit.Unavailable
		if _, _, limitErr := identity.BudgetLimits(capability); limitErr != nil {
			outcome = audit.RefusedBudget
		}
		return ctx, &Refusal{Outcome: outcome, Status: http.StatusForbidden, Type: "permission_error",
			Code: "allocation_denied", Message: "the " + string(capability) + " allocation for this run is missing or exhausted"}
	}
	return budget.WithMeter(ctx, meter), nil
}

// Refuse records r through the route in ctx and writes the JSON body.
func Refuse(ctx context.Context, w http.ResponseWriter, r *Refusal) {
	route, _ := ctx.Value(routeContextKey{}).(Route)
	event := AuditEvent(ctx, string(route.Capability))
	event.DenyReason, event.Outcome = r.Message, r.Outcome
	Record(ctx, route.AuditLogger, event)
	writeDenial(w, r.Status, r.Type, r.Code, r.Message)
}
