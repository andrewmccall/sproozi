package profile

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

const testRefusal = "AgentRun session-0 failed before its sandbox started: RuntimeInvalid: image is required"

// refusedAnswer is a well-formed refused answer.
const refusedAnswer = `{"version":1,"error":"refused","reason":"Failed"}`

// A preparation the executor can never make crosses the protocol as
// refused (409) with its reason, bounded and printable, and reads as
// session.StartRefused, so the home's coordinator ends the start (ADR 0067).
func TestRefusedPreparationCrossesTheProtocolWithItsReason(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ reason, want string }{
		"plain":   {testRefusal, testRefusal},
		"control": {"RuntimeInvalid:\x1b[31m red\nline", "RuntimeInvalid: [31m red line"},
		"long":    {strings.Repeat("a", 600), strings.Repeat("a", 509) + "…"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, b, _, l, data := fixture(t)
			b.failure = &session.StartRefused{Reason: test.reason}
			_, err := c.Prepare(t.Context(), l, data)
			remoteCode(t, err, CodeRefused, http.StatusConflict)
			var refused *session.StartRefused
			if !errors.Is(err, session.ErrStartRefused) || !errors.As(err, &refused) || refused.Reason != test.want ||
				errors.Is(err, session.ErrStartEnded) {
				t.Fatalf("refused prepare: %v, reason %q", err, test.want)
			}
			if !strings.Contains(err.Error(), "refused (HTTP 409)") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refused prepare says %q", err)
			}
		})
	}
}

// Only a prepare may be refused, only with a 409, and only a refusal carries
// a reason, which must already be bounded plain text: anything else is an
// invalid response, never a refusal the home would end a start for.
func TestRefusedOutcomeIsCheckedLikeAnyOther(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		action, body string
		status       int
		refused      bool
	}{
		"prepare":          {actionPrepare, refusedAnswer, http.StatusConflict, true},
		"no reason":        {actionPrepare, `{"version":1,"error":"refused"}`, http.StatusConflict, true},
		"inspect":          {actionInspect, refusedAnswer, http.StatusConflict, false},
		"wrong status":     {actionPrepare, refusedAnswer, http.StatusServiceUnavailable, false},
		"reason elsewhere": {actionPrepare, `{"version":1,"error":"conflict","reason":"Failed"}`, http.StatusConflict, false},
		"control reason":   {actionPrepare, `{"version":1,"error":"refused","reason":"a\nb"}`, http.StatusConflict, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answer := func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/"+test.action {
						h.ServeHTTP(w, r)
						return
					}
					w.Header().Set(headerContentType, jsonMedia)
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
				})
			}
			c, _, _, l, data := wrappedFixture(t, answer)
			var err error
			if test.action == actionPrepare {
				_, err = c.Prepare(t.Context(), l, data)
			} else {
				_, err = c.Inspect(t.Context(), l)
			}
			if got := errors.Is(err, session.ErrStartRefused); got != test.refused || err == nil {
				t.Fatalf("%s answered %s: %v, refused %t", test.action, test.body, err, got)
			}
		})
	}
}
