package modelauth

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestChatGPTUsesAStillValidTokenWhileARefreshIsHeld: in the last minute
// before expiry a transient refresh failure does not fail requests the
// stored access token can still serve; a refusal does, and so does a hold
// once the token has expired. Guards: served.
func TestChatGPTUsesAStillValidTokenWhileARefreshIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		// status of the first refresh; later ones succeed.
		status  int
		refused bool
	}{
		{name: string(refreshHeld), status: http.StatusBadGateway},
		{name: string(refreshRefused), status: http.StatusBadRequest, body: invalidGrantBody, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				_, _ = w.Write([]byte(renewedBody))
			}))
			defer server.Close()
			store := &memoryStore{session: fixtureSession()}
			auth := NewChatGPT(store, server.Client())
			auth.tokenEndpoint = server.URL
			clock := withClock(auth)
			// 45 s before expiry: inside the refresh window, outside the margin.
			store.session.SavedAt = clock.Now().Add(-time.Duration(store.session.ExpiresIn)*time.Second + 45*time.Second)
			for i := range 4 {
				header, err := authorizeOnce(auth)
				if tc.refused {
					if err == nil {
						t.Fatalf("request %d was served after a refusal", i)
					}
					continue
				}
				if err != nil || header != "Bearer "+fixtureAccess {
					t.Fatalf("request %d = %q, %v: the still-valid token was not used", i, header, err)
				}
				clock.Advance(time.Second)
			}
			if calls.Load() != 1 {
				t.Fatalf("refresh requests = %d, want 1", calls.Load())
			}
			if tc.refused {
				return
			}
			// After the hold the refresh is sent again, and succeeds.
			clock.Advance(RefreshHold)
			if header, err := authorizeOnce(auth); err != nil || header != renewedBearer || calls.Load() != 2 {
				t.Fatalf("after the hold = %q, %v, refresh requests %d", header, err, calls.Load())
			}
		})
	}
}

// TestChatGPTStopsServingAHeldTokenAtTheExpiryMargin: a held refresh serves
// the stored access token until expiryMargin before its computed expiry, and
// fails requests from then on, so a token the provider may already consider
// expired is not sent. Guards: served.
func TestChatGPTStopsServingAHeldTokenAtTheExpiryMargin(t *testing.T) {
	server := httptest.NewServer(failingEndpoint(http.StatusBadGateway, ""))
	defer server.Close()
	store := &memoryStore{session: fixtureSession()}
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	clock := withClock(auth)
	// The computed expiry is 31 s away: 1 s outside a 30 s margin.
	store.session.SavedAt = clock.Now().Add(-time.Duration(store.session.ExpiresIn)*time.Second + 31*time.Second)
	if header, err := authorizeOnce(auth); err != nil || header != "Bearer "+fixtureAccess {
		t.Fatalf("1 s outside the margin = %q, %v", header, err)
	}
	clock.Advance(time.Second - time.Nanosecond)
	if header, err := authorizeOnce(auth); err != nil || header != "Bearer "+fixtureAccess {
		t.Fatalf("1 ns outside the margin = %q, %v", header, err)
	}
	clock.Advance(time.Nanosecond)
	if header, err := authorizeOnce(auth); err == nil || header != "" {
		t.Fatalf("at the margin within the hold = %q, %v", header, err)
	}
}
