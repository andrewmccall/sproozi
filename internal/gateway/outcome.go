package gateway

import (
	"errors"
	"net/http"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// RefusalOutcome is the outcome a handler records when it answers with an
// error status of its own choosing. A handler whose status does not say why,
// such as a budget refusal answered 502, sets the outcome itself.
func RefusalOutcome(status int) audit.Outcome {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return audit.RefusedPolicy
	case http.StatusTooManyRequests:
		return audit.RefusedBudget
	case http.StatusRequestTimeout:
		return audit.Cancelled
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return audit.UpstreamFailed
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		return audit.Unavailable
	default:
		return audit.RefusedRequest
	}
}

// SpendOutcome is the outcome of a request Spend refused with err.
func SpendOutcome(err error) audit.Outcome {
	if errors.Is(err, ErrRefusedBudget) {
		return audit.RefusedBudget
	}
	return audit.Unavailable
}

// FailureOutcome is the outcome of an upstream request that failed with err,
// which may be Spend's refusal before the request was sent.
func FailureOutcome(err error) audit.Outcome {
	switch {
	case errors.Is(err, ErrRefusedBudget):
		return audit.RefusedBudget
	case errors.Is(err, ErrBudgetUnavailable):
		return audit.Unavailable
	default:
		return audit.UpstreamFailed
	}
}
