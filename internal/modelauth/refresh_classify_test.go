package modelauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// invalidGrantBody is a token endpoint reply refusing the refresh token.
const invalidGrantBody = `{"error":"invalid_grant"}`

// classifyEndpoint answers status and body until recovered, then issues a
// new token.
func classifyEndpoint(status int, body string, recovered *atomic.Bool, calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if recovered.Load() {
			_, _ = w.Write([]byte(renewedBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestChatGPTRefusesOnlyAnInvalidGrant: only a 400 or 401 whose body names
// invalid_grant ends the sign-in. Every other failed reply is held and
// recovers once the endpoint answers again after the hold. Guards: classify
// and Exchange's reading of an error body.
func TestChatGPTRefusesOnlyAnInvalidGrant(t *testing.T) {
	const other = `{"error":"temporarily_unavailable","error_description":"provider-secret fixture-refresh"}`
	type row struct {
		name    string
		status  int
		body    string
		refused bool
	}
	rows := make([]row, 0, 25)
	for _, status := range []int{400, 401, 403, 404, 405, 407, 408, 409, 410, 413, 415, 421, 422, 425, 429, 451} {
		rows = append(rows, row{name: fmt.Sprintf("%d other error", status), status: status, body: other})
	}
	rows = append(rows,
		row{name: "400 invalid_grant", status: 400, body: `{"error":"invalid_grant","error_description":"provider-secret"}`, refused: true},
		row{name: "401 invalid_grant", status: 401, body: invalidGrantBody, refused: true},
		row{name: "403 invalid_grant", status: 403, body: invalidGrantBody},
		row{name: "400 invalid_request", status: 400, body: `{"error":"invalid_request"}`},
		row{name: "400 empty body", status: 400},
		row{name: "400 not JSON", status: 400, body: `invalid_grant`},
		row{name: "400 body over 4 KiB", status: 400,
			body: `{"error":"invalid_grant","pad":"` + strings.Repeat("x", 5<<10) + `"}`},
		row{name: "400 token-like error", status: 400, body: `{"error":"invalid_grant fixture-refresh provider-secret"}`},
		row{name: "400 error not a string", status: 400, body: `{"error":["invalid_grant"]}`},
	)
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var recovered atomic.Bool
			server := httptest.NewServer(classifyEndpoint(tc.status, tc.body, &recovered, &calls))
			defer server.Close()
			auth := NewChatGPT(expiredStore(), server.Client())
			auth.tokenEndpoint = server.URL
			clock := withClock(auth)
			_, err := authorizeOnce(auth)
			if err == nil || leaks(err.Error()) || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("error = %v", err)
			}
			want := refreshHeld
			if tc.refused {
				want = refreshRefused
				if !strings.Contains(err.Error(), fmt.Sprintf("(token endpoint HTTP %d invalid_grant)", tc.status)) {
					t.Fatalf("a refusal does not name its grant error: %q", err)
				}
			}
			if got := auth.refresh.current(); got != want {
				t.Fatalf("state = %q, want %q (%v)", got, want, err)
			}
			recovered.Store(true)
			clock.Advance(RefreshHold)
			header, err := authorizeOnce(auth)
			if tc.refused {
				if err == nil || calls.Load() != 1 {
					t.Fatalf("a refused sign-in was refreshed again: %q, %v, %d", header, err, calls.Load())
				}
				return
			}
			if err != nil || header != renewedBearer || calls.Load() != 2 {
				t.Fatalf("after the hold = %q, %v, refresh requests %d", header, err, calls.Load())
			}
		})
	}
}
