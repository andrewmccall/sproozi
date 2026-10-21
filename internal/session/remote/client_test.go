package remote

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
)

func recorded() Executor {
	return Executor{Selection: Selection{Kubeconfig: []string{"/fixture/kubeconfig"}, Context: DefaultContext, Namespace: testNamespace},
		Name: testExecutor, Harness: "codex", MaxCheckpointBytes: 8 << 20}
}

func TestClientConnectsLazilyAndAgainAfterTransportFailures(t *testing.T) {
	s := startExecutor(t)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(s.caPEM)
	reachable := true
	opens := 0
	c, err := NewClient(recorded())
	if err != nil {
		t.Fatal(err)
	}
	c.open = func(_ context.Context, e Executor) (*profile.Client, error) {
		opens++
		if !reachable {
			return nil, errors.New("cluster unreachable")
		}
		return profile.Connect(profile.Connection{Origin: "https://" + testService + "." + testNamespace + ".svc:8443",
			Token: testToken, Roots: roots, MaxCheckpointBytes: e.MaxCheckpointBytes,
			Dial: func(ctx context.Context) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", s.address)
			}})
	}
	owner := work.ClientFor(c)
	if opens != 0 {
		t.Fatal("client contacted the cluster before use")
	}
	reachable = false
	_, err = owner.Info(t.Context(), "00000000-0000-4000-8000-000000000001")
	var remote *profile.RemoteError
	if !errors.As(err, &remote) || remote.Code != profile.CodeUnavailable || !strings.Contains(remote.Transport.Error(), "cluster unreachable") {
		t.Fatalf("unreachable cluster: %v", err)
	}
	reachable = true
	s.status.Store(http.StatusForbidden)
	_, err = owner.Info(t.Context(), "00000000-0000-4000-8000-000000000001")
	if !errors.As(err, &remote) || remote.Code != profile.CodeDenied || opens != 2 {
		t.Fatalf("denial: %v after %d opens", err, opens)
	}
	_, _ = owner.Info(t.Context(), "00000000-0000-4000-8000-000000000001")
	if opens != 2 {
		t.Fatalf("an executor's own answer dropped the connection: %d opens", opens)
	}
	s.status.Store(http.StatusUnauthorized)
	_, _ = owner.Info(t.Context(), "00000000-0000-4000-8000-000000000001")
	_, _ = owner.Info(t.Context(), "00000000-0000-4000-8000-000000000001")
	if opens != 3 {
		t.Fatalf("a rejected token did not reconnect: %d opens", opens)
	}
}

func TestClientNeedsACompleteRecord(t *testing.T) {
	e := recorded()
	e.MaxCheckpointBytes = 0
	if _, err := NewClient(e); err == nil {
		t.Fatal("client accepted a record without a checkpoint limit")
	}
}
