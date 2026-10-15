package main

import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

func TestCatalogueKeepsPartialAvailabilityAndBoundsResults(t *testing.T) {
	id := uuid.NewString()
	rows := make([]work.View, catalogueSessions+1)
	for i := range rows {
		rows[i] = work.View{
			ID: id, ExecutionID: id, AllocationID: uuid.NewString(), Placement: "remote-placement",
			Phase: "home-owned", Result: "untrusted final answer omitted from discovery",
		}
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/work/list" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	dir := t.TempDir()
	ca, token := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(ca, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte(strings.Repeat("t", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	connection := profile.ClientConfig{URL: server.URL, TokenFile: token, CAFile: ca, MaxCheckpointBytes: 1 << 20}
	out := discoverSessions(t.Context(), options{
		socket: filepath.Join(dir, "missing.sock"),
		fixed:  home.Config{Remote: map[string]profile.ClientConfig{testExecutor: connection, "unconfigured": {}}},
	})
	if !out.Truncated || len(out.Sessions) != catalogueSessions || len(out.Executors) != 3 {
		t.Fatalf("unbounded catalogue: %+v", out.Executors)
	}
	if out.Executors[0].Executor != dockerExecutor || out.Executors[0].Availability != testUnavailable ||
		out.Executors[1].Executor != testExecutor || out.Executors[1].Availability != testAvailable ||
		out.Executors[2].Availability != testUnavailable {
		t.Fatalf("partial availability lost: %+v", out.Executors)
	}
	if out.Sessions[0].ID != id || out.Sessions[0].Executor != testExecutor ||
		out.Sessions[0].Placement != "remote-placement" || out.Sessions[0].Ownership != testHomeOwnership {
		t.Fatalf("identity/source lost: %+v", out.Sessions[0])
	}
	b, err := json.Marshal(out)
	if err != nil || strings.Contains(string(b), "untrusted final answer") {
		t.Fatalf("discovery carried native text: %v", err)
	}
}
