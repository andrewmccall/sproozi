package proxytransport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

// A refused identity gets the same generic 401 whatever the cause, but the
// gateway logs the cause once per window, then one line counting the rest,
// and never logs the token.
func TestRefusedIdentitiesLogTheirCauseOncePerWindow(t *testing.T) {
	const secret = "secret-token-value"
	logs := &lockedBuffer{}
	config := admissionConfig(t)
	config.Logger = slog.New(slog.NewTextHandler(logs, nil))
	causes := []error{
		errors.New(`agentruns.sproozi.com "run-1" is forbidden`),
		errors.New("gateway: token is bound to a Pod other than the AgentRun's sandbox"),
	}
	next := 0
	config.Authenticate = func(context.Context, string) (*gateway.ExecutionGrant, error) {
		return nil, causes[next]
	}
	g, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	g.authRejected.window = 100 * time.Millisecond
	for i := range 4 {
		next = i / 3 // three of the first cause, then one of the second
		refused := httptest.NewRecorder()
		request := connectRequest(secret)
		request.RemoteAddr = "10.0.0.7:4242"
		g.ServeHTTP(refused, request)
		if refused.Code != http.StatusUnauthorized || strings.TrimSpace(refused.Body.String()) != "invalid gateway identity" {
			t.Fatalf("response = %d %q, want the generic 401", refused.Code, refused.Body.String())
		}
	}
	first := logs.String()
	if strings.Count(first, "Rejected inspected connection with invalid gateway identity") != 2 ||
		!strings.Contains(first, `reason="agentruns.sproozi.com \"run-1\" is forbidden" remote=10.0.0.7:4242`) ||
		!strings.Contains(first, "sandbox") || strings.Contains(first, secret) {
		t.Fatalf("first refusal logs = %q", first)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "Rejected further") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if summary := logs.String(); !strings.Contains(summary,
		`Rejected further inspected connections with invalid gateway identity" count=2 reason="agentruns.sproozi.com`) ||
		strings.Contains(summary, secret) {
		t.Fatalf("summary log = %q", summary)
	}
}

// Distinct causes beyond the bound are counted, not logged one by one.
func TestRefusedIdentityCausesAreBounded(t *testing.T) {
	logs := &lockedBuffer{}
	l := newAuthFailureLog(slog.New(slog.NewTextHandler(logs, nil)))
	l.window = 50 * time.Millisecond
	for i := range authFailureMaxCauses + 5 {
		l.record(errors.New("cause "+strings.Repeat("x", i)), "remote")
	}
	if got := strings.Count(logs.String(), "Rejected inspected connection"); got != authFailureMaxCauses {
		t.Fatalf("logged %d first lines, want %d", got, authFailureMaxCauses)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "other causes") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), `count=5 reason="other causes"`) {
		t.Fatalf("overflow summary missing: %q", logs.String())
	}
}
