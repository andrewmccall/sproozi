package work

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/andrewmccall/sproozi/internal/session/profile"
)

func decodeInput(b []byte, v any) error {
	if err := decode(b, v); err != nil {
		return &profile.Rejection{Code: profile.CodeInvalid}
	}
	return nil
}

func outcomeCode(err error) profile.Code {
	var rejection *profile.Rejection
	if errors.As(err, &rejection) {
		return rejection.Code
	}
	var remote *profile.RemoteError
	if errors.As(err, &remote) {
		return remote.Code
	}
	// Filesystem, transport and backend errors carry private details and do not
	// establish a semantic rejection, absent execution or known usage.
	return profile.CodeUnavailable
}

func outcomeStatus(code profile.Code) int {
	switch code {
	case profile.CodeInvalid:
		return http.StatusBadRequest
	case profile.CodeDenied:
		return http.StatusForbidden
	case profile.CodeConflict:
		return http.StatusConflict
	case profile.CodeExpired:
		return http.StatusGone
	case profile.CodeUnsupported:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusServiceUnavailable
	}
}

func outcomeMessage(err error) string {
	code := outcomeCode(err)
	return fmt.Sprintf("%s: %s", code, profile.OwnerOutcomeMessage(code))
}
