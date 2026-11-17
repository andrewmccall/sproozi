package modelauth

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const (
	renewedRefresh = "new-refresh"
	renewedBody    = `{"access_token":"new-access","refresh_token":"` + renewedRefresh + `","token_type":"Bearer","expires_in":3600}`
)

// TestChatGPTNewSignInEndsAHoldOrRefusalAtOnce: an administrator's new
// sign-in reaches the next model request that reads the store, within the
// hold or after a refusal. Guards: the signed-in event in reloadOrRefresh.
func TestChatGPTNewSignInEndsAHoldOrRefusalAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   refreshState
	}{
		{name: "held", status: http.StatusServiceUnavailable, want: refreshHeld},
		{name: "refused", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, want: refreshRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			store := expiredStore()
			auth := NewChatGPT(store, server.Client())
			auth.tokenEndpoint = server.URL
			withClock(auth)
			if _, err := authorizeOnce(auth); err == nil || auth.refresh.current() != tc.want {
				t.Fatalf("state = %q, err = %v", auth.refresh.current(), err)
			}
			// A fresh new sign-in is used, with no refresh.
			store.session = fixtureSession()
			store.session.AccessToken, store.session.RefreshToken = "signed-in-access", "signed-in-refresh"
			if header, err := authorizeOnce(auth); err != nil || header != "Bearer signed-in-access" {
				t.Fatalf("after a new sign-in = %q, %v", header, err)
			}
			if auth.refresh.current() != refreshReady || calls.Load() != 1 {
				t.Fatalf("state = %q, refresh requests = %d", auth.refresh.current(), calls.Load())
			}
		})
	}
}

// TestChatGPTDueNewSignInIsRefreshedWithinAHold: a failure of one sign-in
// does not hold the refresh of the next. Guards: signed-in before admit.
func TestChatGPTDueNewSignInIsRefreshedWithinAHold(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get(refreshTokenField) == fixtureRefresh {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(renewedBody))
	}))
	defer server.Close()
	store := expiredStore()
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	withClock(auth)
	if _, err := authorizeOnce(auth); err == nil || auth.refresh.current() != refreshHeld {
		t.Fatalf("state = %q, err = %v", auth.refresh.current(), err)
	}
	store.session.RefreshToken = "second-sign-in"
	if header, err := authorizeOnce(auth); err != nil || header != renewedBearer {
		t.Fatalf("a due new sign-in = %q, %v", header, err)
	}
	if calls.Load() != 2 || store.session.RefreshToken != renewedRefresh {
		t.Fatalf("refresh requests = %d, stored %q", calls.Load(), store.session.RefreshToken)
	}
}

// TestChatGPTRecoversAfterTheHold: the first request after a hold sends the
// refresh again, and a success clears the failure. Guards: admit and the
// refreshed event.
func TestChatGPTRecoversAfterTheHold(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(renewedBody))
	}))
	defer server.Close()
	auth := NewChatGPT(expiredStore(), server.Client())
	auth.tokenEndpoint = server.URL
	clock := withClock(auth)
	if _, err := authorizeOnce(auth); err == nil {
		t.Fatal("authorized during an outage")
	}
	clock.Advance(RefreshHold - time.Nanosecond)
	if _, err := authorizeOnce(auth); err == nil || calls.Load() != 1 {
		t.Fatalf("a request within the hold refreshed: %v, %d", err, calls.Load())
	}
	clock.Advance(time.Nanosecond)
	if header, err := authorizeOnce(auth); err != nil || header != renewedBearer {
		t.Fatalf("after the hold = %q, %v", header, err)
	}
	if auth.refresh.current() != refreshReady || auth.refresh.check() != nil || calls.Load() != 2 {
		t.Fatalf("state = %q, refresh requests = %d", auth.refresh.current(), calls.Load())
	}
}

// TestRefreshTransitions: every declared move, and an undeclared one leaves
// the record unchanged. Guards: refreshTransitions, apply and check.
func TestRefreshTransitions(t *testing.T) {
	transient, refusal := &RefreshError{cause: "c"}, &RefreshError{Refused: true, cause: "c"}
	held := refreshRecord{state: refreshHeld, of: fixtureSession(), until: time.Now().Add(time.Second), err: transient}
	refused := refreshRecord{state: refreshRefused, of: fixtureSession(), err: refusal}
	for _, tc := range []struct {
		from    refreshRecord
		event   refreshEvent
		failure *RefreshError
		want    refreshState // "" when the move is refused
	}{
		{refreshRecord{}, eventRefreshed, nil, refreshReady},
		{held, eventRefreshed, nil, refreshReady},
		{refused, eventRefreshed, nil, ""},
		{refreshRecord{}, eventFailed, transient, refreshHeld},
		{held, eventFailed, transient, refreshHeld},
		{refused, eventFailed, transient, ""},
		{refreshRecord{}, eventRefused, refusal, refreshRefused},
		{held, eventRefused, refusal, refreshRefused},
		{refused, eventRefused, refusal, ""},
		{refreshRecord{}, eventSignedIn, nil, ""},
		{held, eventSignedIn, nil, refreshReady},
		{refused, eventSignedIn, nil, refreshReady},
		// A failure whose kind contradicts the event fails check.
		{refreshRecord{}, eventFailed, refusal, ""},
		{refreshRecord{}, eventRefused, transient, ""},
	} {
		record := tc.from
		err := record.apply(tc.event, fixtureSession(), tc.failure, time.Now())
		if tc.want == "" {
			if err == nil || record.current() != tc.from.current() || record.until != tc.from.until || record.err != tc.from.err {
				t.Errorf("%s from %s: applied (%v), now %+v", tc.event, tc.from.current(), err, record)
			}
			continue
		}
		if err != nil || record.current() != tc.want || record.check() != nil {
			t.Errorf("%s from %s = %s, %v", tc.event, tc.from.current(), record.current(), err)
		}
	}
	for _, bad := range []refreshRecord{
		{state: "waiting"},
		{state: refreshReady, err: transient},
		{state: refreshReady, until: time.Now()},
		{state: refreshHeld, err: transient},
		{state: refreshHeld, until: time.Now()},
		{state: refreshHeld, until: time.Now(), err: refusal},
		{state: refreshRefused},
		{state: refreshRefused, err: transient},
		{state: refreshRefused, until: time.Now(), err: refusal},
	} {
		if bad.check() == nil {
			t.Errorf("check accepted %+v", bad)
		}
	}
}
