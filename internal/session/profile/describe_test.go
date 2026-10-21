package profile

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// authorizerFunc adapts a function to Authorizer for tests.
type authorizerFunc func(context.Context, string) error

func (f authorizerFunc) Authorize(ctx context.Context, token string) error { return f(ctx, token) }

func describeServer(t *testing.T, home Authorizer) Connection {
	t.Helper()
	backend := &retainedBackend{}
	l := launch(fixtureDigest)
	h, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: home, Placement: l.Placement,
		InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour,
		MaxCheckpointBytes: testMax, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := serve(t, h)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	address := s.Listener.Addr().String()
	// The origin names a host that does not resolve; Dial supplies the route,
	// as a Kubernetes port-forward does.
	return Connection{Origin: "https://example.com:8443", Token: testToken, Roots: roots, MaxCheckpointBytes: 1,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", address)
		}}
}

const fixtureDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestDescribeOverACallerSuppliedRoute(t *testing.T) {
	t.Parallel()
	home, err := StaticToken(testToken)
	if err != nil {
		t.Fatal(err)
	}
	connection := describeServer(t, home)
	c, err := Connect(connection)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d, err := c.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if d.Placement != "remote" || d.Harness != harness.NativeCodex || d.MaxCheckpointBytes != testMax {
		t.Fatalf("description %+v", d)
	}
}

func TestAuthorizerOutcomesReachTheCaller(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result error
		status int
		code   Code
	}{
		{"unauthenticated", ErrUnauthenticated, http.StatusUnauthorized, CodeDenied},
		{"not allowed", &Rejection{Code: CodeDenied}, http.StatusForbidden, CodeDenied},
		{"undecided", errors.New("token review unavailable"), http.StatusServiceUnavailable, CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			connection := describeServer(t, authorizerFunc(func(_ context.Context, token string) error {
				seen = token
				return tc.result
			}))
			c, err := Connect(connection)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, err = c.Describe(t.Context())
			var remote *RemoteError
			if !errors.As(err, &remote) || remote.Status != tc.status || remote.Code != tc.code {
				t.Fatalf("got %v, want HTTP %d %s", err, tc.status, tc.code)
			}
			if seen != testToken {
				t.Fatalf("authorizer saw %q", seen)
			}
		})
	}
}

func TestHandlerTakesOneHomeAuthorization(t *testing.T) {
	t.Parallel()
	home, err := StaticToken(testToken)
	if err != nil {
		t.Fatal(err)
	}
	l := launch(fixtureDigest)
	_, err = NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: home, TokenFile: tokenFile(t),
		Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour,
		MaxCheckpointBytes: testMax, Backend: &retainedBackend{}})
	if err == nil {
		t.Fatal("handler accepted both a token file and an authorizer")
	}
	if _, err = StaticToken("short"); err == nil {
		t.Fatal("static token accepted a short token")
	}
}
