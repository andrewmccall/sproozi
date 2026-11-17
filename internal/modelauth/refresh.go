package modelauth

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RefreshHold is how long after a transient refresh failure the gateway sends
// no further refresh for the same stored sign-in. It equals
// SessionReloadInterval, the documented bound on how old the gateway's view of
// the credential may be, so a remembered failure is no older than a remembered
// session. The first model request after it tries again; nothing waits for it.
const RefreshHold = SessionReloadInterval

// refreshState is what the last refresh of the stored sign-in left. It is held
// in memory beside the session it describes and is not shared between
// processes: a gateway restart forgets it.
type refreshState string

const (
	// refreshReady: no failure is remembered; a due refresh is sent.
	refreshReady refreshState = "ready"
	// refreshHeld: the last refresh failed transiently (no reply, a timeout,
	// 408, 429, 5xx, or an unusable 200). No refresh is sent before until.
	refreshHeld refreshState = "held"
	// refreshRefused: the provider refused the refresh token (any other 4xx,
	// such as 400 or 401 invalid_grant). No refresh is sent again for these
	// credentials; an administrator must sign in again.
	refreshRefused refreshState = "refused"
)

type refreshEvent string

const (
	// eventRefreshed: the provider issued a usable credential.
	eventRefreshed refreshEvent = "refreshed"
	// eventFailed: a refresh failed transiently.
	eventFailed refreshEvent = "failed"
	// eventRefused: the provider refused the refresh token.
	eventRefused refreshEvent = "refused"
	// eventSignedIn: the store holds credentials other than those the
	// remembered failure was for, such as an administrator's new sign-in.
	eventSignedIn refreshEvent = "signed-in"
)

// refreshTransitions is the (from, event) → to table. A refresh is sent only
// from ready, or from held once its hold has ended (admit), so refused leaves
// only by a new sign-in.
var refreshTransitions = map[refreshEvent]map[refreshState]refreshState{
	eventRefreshed: {refreshReady: refreshReady, refreshHeld: refreshReady},
	eventFailed:    {refreshReady: refreshHeld, refreshHeld: refreshHeld},
	eventRefused:   {refreshReady: refreshRefused, refreshHeld: refreshRefused},
	eventSignedIn:  {refreshHeld: refreshReady, refreshRefused: refreshReady},
}

// refreshRecord is the remembered outcome of the last refresh. The zero value
// is ready.
type refreshRecord struct {
	state refreshState
	// of is the stored session the failure is for; its credentials, not its
	// expiry, identify it (Session.SameCredentials).
	of Session
	// until ends a hold; zero in every other state.
	until time.Time
	// err is what model requests see while the failure is remembered.
	err *RefreshError
}

func (r refreshRecord) current() refreshState {
	if r.state == "" {
		return refreshReady
	}
	return r.state
}

// check validates the record's structure against its state.
func (r refreshRecord) check() error {
	switch r.current() {
	case refreshReady:
		if !r.until.IsZero() || r.err != nil {
			return errors.New("modelauth: a ready refresh record remembers a failure")
		}
	case refreshHeld:
		if r.until.IsZero() || r.err == nil || r.err.Refused {
			return errors.New("modelauth: a held refresh record has no hold or transient failure")
		}
	case refreshRefused:
		if !r.until.IsZero() || r.err == nil || !r.err.Refused {
			return errors.New("modelauth: a refused refresh record has a hold or no refusal")
		}
	default:
		return fmt.Errorf("modelauth: unknown refresh state %q", r.state)
	}
	return nil
}

// apply moves the record by the table, or refuses an undeclared move and
// leaves the record unchanged.
func (r *refreshRecord) apply(event refreshEvent, of Session, failure *RefreshError, now time.Time) error {
	to, ok := refreshTransitions[event][r.current()]
	if !ok {
		return fmt.Errorf("modelauth: refresh event %q is not declared from %q", event, r.current())
	}
	next := refreshRecord{state: to}
	switch to {
	case refreshHeld:
		next.of, next.until, next.err = of, now.Add(RefreshHold), failure
	case refreshRefused:
		next.of, next.err = of, failure
	}
	if err := next.check(); err != nil {
		return err
	}
	*r = next
	return nil
}

// admit returns the remembered failure when no refresh may be sent now.
func (r refreshRecord) admit(now time.Time) *RefreshError {
	switch r.current() {
	case refreshHeld:
		if now.Before(r.until) {
			return r.err
		}
	case refreshRefused:
		return r.err
	}
	return nil
}

// RefreshError is a failed refresh of the gateway's ChatGPT sign-in. Its text
// is built from fixed phrases and an HTTP status only, never from the
// provider's body or any token, so it may be shown to a model request.
type RefreshError struct {
	// Refused is true when the provider refused the refresh token and an
	// administrator must sign in again; false when the gateway tries again
	// after RefreshHold.
	Refused bool
	cause   string
}

func (e *RefreshError) Error() string {
	if e.Refused {
		return "the provider refused to renew the ChatGPT sign-in (" + e.cause +
			"); an administrator must sign in again"
	}
	return fmt.Sprintf("the ChatGPT sign-in could not be renewed (%s); the gateway tries again %s after the failure",
		e.cause, RefreshHold)
}

// classify turns a failed refresh into its outcome. A token endpoint's 4xx
// other than 408 and 429 refuses the grant (OAuth 2.0 token errors such as
// invalid_grant are 400 or 401); anything else may succeed if sent again.
func classify(err error) *RefreshError {
	var endpoint *tokenEndpointError
	if !errors.As(err, &endpoint) {
		return &RefreshError{cause: "the renewed credential was incomplete"}
	}
	switch status := endpoint.status; {
	case status == 0:
		return &RefreshError{cause: "the token endpoint was unreachable or did not answer in time"}
	case status == http.StatusOK:
		return &RefreshError{cause: "the token endpoint returned an unreadable reply"}
	case status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests:
		return &RefreshError{Refused: true, cause: fmt.Sprintf("token endpoint HTTP %d", status)}
	default:
		return &RefreshError{cause: fmt.Sprintf("token endpoint HTTP %d", status)}
	}
}

// tokenEndpointError is a failed token exchange. status is 0 when the
// endpoint gave no reply, and 200 when its reply could not be read.
type tokenEndpointError struct {
	status  int
	message string
}

func (e *tokenEndpointError) Error() string { return e.message }
