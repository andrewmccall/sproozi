package modelauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestChatGPTRefreshNeverFollowsARedirect: a token endpoint that redirects
// must not carry the refresh token to the redirect's target, whichever client
// the authenticator was given. A redirect is a held failure. Guards:
// NewChatGPT and NewRefreshClient.
func TestChatGPTRefreshNeverFollowsARedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		for name, client := range map[string]*http.Client{"plain client": {}, "refresh client": NewRefreshClient()} {
			t.Run(http.StatusText(status)+"/"+name, func(t *testing.T) {
				var stolen atomic.Int32
				elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					stolen.Add(1)
					_, _ = w.Write([]byte(renewedBody))
				}))
				defer elsewhere.Close()
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, elsewhere.URL+"/oauth/token", status)
				}))
				defer endpoint.Close()
				auth := NewChatGPT(expiredStore(), client)
				auth.tokenEndpoint = endpoint.URL
				withClock(auth)
				_, err := authorizeOnce(auth)
				if stolen.Load() != 0 {
					t.Fatal("the refresh followed the redirect to another host")
				}
				if err == nil || auth.refresh.current() != refreshHeld || strings.Contains(err.Error(), "127.0.0.1") {
					t.Fatalf("state = %q, err = %v", auth.refresh.current(), err)
				}
			})
		}
	}
	client := NewRefreshClient()
	if client.Timeout != refreshTimeout {
		t.Fatalf("refresh client timeout = %s, want %s", client.Timeout, refreshTimeout)
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("the refresh client uses an ambient proxy")
	}
}
