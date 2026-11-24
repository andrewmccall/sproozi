package proxytransport

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

type streamEnd struct {
	cause   error
	elapsed time.Duration
}

// streamingModel writes one byte every 50ms until its extended model call ends.
func streamingModel(limit time.Duration, ended chan<- streamEnd) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		ctx, end := ExtendModelCall(r.Context(), limit)
		defer end()
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := io.WriteString(w, "."); err != nil {
				break
			}
			w.(http.Flusher).Flush()
			select {
			case <-ctx.Done():
				ended <- streamEnd{context.Cause(ctx), time.Since(started)}
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		ended <- streamEnd{context.Cause(ctx), time.Since(started)}
	})
}

func readStream(t *testing.T, s proxySession) chan time.Duration {
	t.Helper()
	started := time.Now()
	lasted := make(chan time.Duration, 1)
	_ = s.client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(s.client, "POST /v1/responses HTTP/1.1\r\nHost: service.test\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	go func() {
		response, err := http.ReadResponse(bufio.NewReader(s.client), nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
		}
		lasted <- time.Since(started)
	}()
	return lasted
}

// A model stream outlives the connection's request deadline (750ms) and
// lifetime (1s), up to the gateway maximum or the policy's lower limit.
//
// Each case runs in a synctest bubble over net.Pipe, so the stream's
// elapsed time is its clock's, not the machine's, and the stream ends at
// exactly its limit. A bubble that stops making progress is reported on
// real time (testutil.WatchBubble).
func TestModelCallOutlivesRequestDeadlineUpToItsMaximum(t *testing.T) {
	certificate := testCertificate(t)
	for _, tc := range []struct {
		name  string
		limit time.Duration
		want  time.Duration
	}{
		{"gateway maximum", 0, 2 * time.Second},
		{"policy limit", 1500 * time.Millisecond, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watch := testutil.WatchBubble(t, "the extended model call and its session did not end")
			defer watch.End()
			synctest.Test(t, func(t *testing.T) {
				logs := &lockedBuffer{}
				ended := make(chan streamEnd, 1)
				identity := testIdentity()
				g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: certificate},
					Authenticate: func(context.Context, string) (*gateway.ExecutionGrant, error) { return identity, nil },
					Revalidate:   func(context.Context, *gateway.ExecutionGrant) error { return nil },
					Handler:      streamingModel(tc.limit, ended), AllowedAuthority: onlyTestService,
					SessionLifetime: time.Second, MaxModelCallDuration: 2 * time.Second,
					RevocationInterval: 20 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil))})
				if err != nil {
					t.Fatal(err)
				}
				s := startProxySession(t, g, testProxyAuthorization)
				defer func() { _ = s.client.Close() }()
				lasted := readStream(t, s)
				end := <-ended
				if !errors.Is(end.cause, ErrModelCallDeadline) || end.elapsed != tc.want {
					t.Fatalf("stream ended after %v with %v, want %v at its deadline", end.elapsed, end.cause, tc.want)
				}
				if got := <-lasted; got != tc.want {
					t.Fatalf("client stream ended after %v, want %v", got, tc.want)
				}
				select {
				case <-s.serveDone:
				case <-time.After(2 * time.Second):
					t.Fatal("connection stayed open after the model call ended")
				}
				if strings.Contains(logs.String(), "Ended inspected request at connection lifetime") {
					t.Fatalf("extended call was logged as cut at the connection lifetime: %s", logs.String())
				}
			})
		})
	}
}

// Revocation still ends a model stream that outlived the connection lifetime.
//
// The stream runs in a synctest bubble over net.Pipe. The revocation comes
// 200ms after the lifetime (1s) and past the request deadline (750ms) on its
// clock, so the stream must have been extended to reach it, and it must end
// by the monitor's next check, one revocation interval later. A bubble that
// stops making progress is reported on real time (testutil.WatchBubble).
func TestModelCallIsRevokedMidStream(t *testing.T) {
	certificate := testCertificate(t)
	watch := testutil.WatchBubble(t, "the revoked model call and its session did not end")
	defer watch.End()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		revoked := false
		ended := make(chan streamEnd, 1)
		identity := testIdentity()
		g, err := New(Config{Certificates: map[string]tls.Certificate{testServiceHost: certificate},
			Authenticate: func(context.Context, string) (*gateway.ExecutionGrant, error) { return identity, nil },
			Revalidate: func(context.Context, *gateway.ExecutionGrant) error {
				mu.Lock()
				defer mu.Unlock()
				if revoked {
					return fmt.Errorf("revoked")
				}
				return nil
			},
			Handler: streamingModel(0, ended), AllowedAuthority: onlyTestService,
			SessionLifetime: time.Second, MaxModelCallDuration: 5 * time.Second, RevocationInterval: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		s := startProxySession(t, g, testProxyAuthorization)
		defer func() { _ = s.client.Close() }()
		lasted := readStream(t, s)
		const revokedAt, interval = 1200 * time.Millisecond, 20 * time.Millisecond
		time.Sleep(revokedAt) // past the connection lifetime
		mu.Lock()
		revoked = true
		mu.Unlock()
		end := <-ended
		if errors.Is(end.cause, ErrModelCallDeadline) || end.elapsed < revokedAt || end.elapsed > revokedAt+interval {
			t.Fatalf("revoked stream ended after %v with %v, want within %v of the revocation at %v",
				end.elapsed, end.cause, interval, revokedAt)
		}
		if got := <-lasted; got < revokedAt || got > revokedAt+interval {
			t.Fatalf("client stream lasted %v, want within %v of the revocation at %v", got, interval, revokedAt)
		}
		select {
		case <-s.serveDone:
		case <-time.After(time.Second):
			t.Fatal("session did not close after revocation")
		}
	})
}

// Outside an inspected connection a model call lasts at most its own limit.
func TestModelCallWithoutConnectionUsesItsLimit(t *testing.T) {
	ctx, end := ExtendModelCall(t.Context(), 20*time.Millisecond)
	defer end()
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), ErrModelCallDeadline) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
}
