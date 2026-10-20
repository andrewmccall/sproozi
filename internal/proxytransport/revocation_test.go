package proxytransport

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Revoking an execution with several open connections logs the first one with
// its execution and reason, then one line counting the rest.
func TestRevokedConnectionsAreLoggedOncePerExecutionWindow(t *testing.T) {
	logs := &lockedBuffer{}
	config := admissionConfig(t)
	config.RevocationInterval = 10 * time.Millisecond
	config.Logger = slog.New(slog.NewTextHandler(logs, nil))
	var revoked atomic.Bool
	config.Revalidate = func(context.Context, *gateway.ExecutionGrant) error {
		if revoked.Load() {
			return errors.New("gateway: execution grant revoked: AgentPolicy changed")
		}
		return nil
	}
	config.Authenticate = func(context.Context, string) (*gateway.ExecutionGrant, error) {
		identity := testIdentity()
		identity.Execution.Namespace, identity.Execution.Name = "team", "run-1"
		return identity, nil
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	g.revocations.window = 100 * time.Millisecond
	sessions := make([]proxySession, 0, 3)
	for range 3 {
		sessions = append(sessions, startProxySession(t, g, testProxyAuthorization))
	}
	revoked.Store(true)
	for _, session := range sessions {
		select {
		case <-session.serveDone:
		case <-time.After(2 * time.Second):
			t.Fatal("revoked session stayed open")
		}
		_ = session.client.Close()
	}
	first := logs.String()
	if strings.Count(first, "Revoked inspected connection") != 1 ||
		!strings.Contains(first, "namespace=team execution=run-1 executionUID=test-execution") ||
		!strings.Contains(first, `reason="gateway: execution grant revoked: AgentPolicy changed"`) ||
		strings.Contains(first, "token") {
		t.Fatalf("first revocation log = %q", first)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "Revoked further inspected connections") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if summary := logs.String(); !strings.Contains(summary, "Revoked further inspected connections") ||
		!strings.Contains(summary, "count=2") {
		t.Fatalf("summary log = %q", summary)
	}
}

// Connections refused at the per-execution limit log the first refusal with
// its execution and limit, then one line counting the rest.
func TestLimitRejectionsAreLoggedOncePerExecutionWindow(t *testing.T) {
	logs := &lockedBuffer{}
	config := admissionConfig(t)
	config.MaxConnections = 4
	config.MaxSessionsPerExecution = 1
	config.Logger = slog.New(slog.NewTextHandler(logs, nil))
	config.Authenticate = func(context.Context, string) (*gateway.ExecutionGrant, error) {
		identity := testIdentity()
		identity.Execution.Namespace, identity.Execution.Name = "team", "run-1"
		return identity, nil
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	g.limitRejected.window = 100 * time.Millisecond
	session := startProxySession(t, g, testProxyAuthorization)
	for range 3 {
		refused := httptest.NewRecorder()
		g.ServeHTTP(refused, connectRequest("token"))
		if refused.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d", refused.Code)
		}
	}
	_ = session.client.Close()
	<-session.serveDone
	first := logs.String()
	if strings.Count(first, "Rejected inspected connection at per-execution limit") != 1 ||
		!strings.Contains(first, "namespace=team execution=run-1 executionUID=test-execution limit=1") ||
		strings.Contains(first, "token") {
		t.Fatalf("first limit log = %q", first)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "Rejected further") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if summary := logs.String(); !strings.Contains(summary,
		"Rejected further inspected connections at per-execution limit") || !strings.Contains(summary, "count=2 limit=1") {
		t.Fatalf("summary log = %q", summary)
	}
}
