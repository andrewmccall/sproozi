//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

const testExecutor = "engineering"

func TestCatalogueKeepsPartialAvailabilityAndBoundsResults(t *testing.T) {
	id := uuid.NewString()
	rows := make([]work.View, catalogueSessions+1)
	for i := range rows {
		rows[i] = work.View{
			Association: project.Association{Project: "sproozi-dev", Repository: "sproozi"},
			ID:          id, ExecutionID: id, AllocationID: uuid.NewString(), Placement: "remote-placement",
			Phase: "home-owned", Result: "untrusted final answer omitted from discovery",
		}
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/work/list" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		var query work.SessionQuery
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.Project != "sproozi-dev" {
			t.Errorf("Remote Project filter lost: %+v %v", query, err)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	dir := testutil.PrivateDir(t)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	connect := func(origin string) *work.Client {
		client, err := profile.Connect(profile.Connection{Origin: origin, Token: strings.Repeat("t", 32), Roots: roots,
			MaxCheckpointBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		return work.ClientFor(client)
	}
	q := &options{socket: filepath.Join(dir, "missing.sock"), project: "sproozi-dev"}
	out := discoverSessions(t.Context(), q, true,
		[]remoteTarget{{Name: testExecutor, Client: connect(server.URL)},
			{Name: "unreachable", Client: connect("https://127.0.0.1:1")}})
	if !out.Truncated || len(out.Sessions) != catalogueSessions || len(out.Executors) != 3 {
		t.Fatalf("unbounded catalogue: %+v", out.Executors)
	}
	if out.Executors[0].Executor != testExecutor || out.Executors[0].Availability != "available" ||
		out.Executors[1].Executor != localName || out.Executors[1].Error != errorServiceNotRunning ||
		out.Executors[2].Availability != "unavailable" {
		t.Fatalf("partial availability lost: %+v", out.Executors)
	}
	if out.Sessions[0].ID != id || out.Sessions[0].Executor != testExecutor ||
		out.Sessions[0].Project != "sproozi-dev" || out.Sessions[0].Repository != "sproozi" ||
		out.Sessions[0].Placement != "remote-placement" || out.Sessions[0].Ownership != localName ||
		out.Sessions[0].Phase != "pulled" {
		t.Fatalf("identity/source lost: %+v", out.Sessions[0])
	}
	b, err := json.Marshal(out)
	if err != nil || strings.Contains(string(b), "untrusted final answer") {
		t.Fatalf("discovery carried native text: %v", err)
	}
}
