//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
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
			Phase: "pulled", Result: "untrusted final answer omitted from discovery",
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
	// The platform's control address in an empty directory: no service listens.
	q := &options{socket: home.SocketPath(dir), project: "sproozi-dev"}
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

// fakeCluster serves a remote work catalogue holding ids and replaces the
// remote seam with it for the test.
func fakeCluster(t *testing.T, ids ...string) {
	t.Helper()
	views := make([]work.View, 0, len(ids))
	for _, id := range ids {
		views = append(views, work.View{ID: id, ExecutionID: id, AllocationID: uuid.NewString(), Phase: "retained"})
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/work/list" {
			_ = json.NewEncoder(w).Encode(views)
			return
		}
		var reference struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reference)
		for _, v := range views {
			if v.ID == reference.ID {
				_ = json.NewEncoder(w).Encode(v)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	previous := resolveRemote
	resolveRemote = func(context.Context, string, string) (remoteTarget, error) {
		client, err := profile.Connect(profile.Connection{Origin: server.URL, Token: strings.Repeat("t", 32),
			Roots: roots, MaxCheckpointBytes: 1 << 20})
		if err != nil {
			return remoteTarget{}, err
		}
		return remoteTarget{Name: testExecutor, Client: work.ClientFor(client)}, nil
	}
	t.Cleanup(func() { resolveRemote = previous })
}

// pull completes a unique prefix from the cluster's sessions.
func TestPullCompletesAPrefixFromTheCluster(t *testing.T) {
	id, other := "4215d7d1-68ff-47ed-9dad-dbf1c8317d38", "4299a0b0-0000-4000-8000-000000000001"
	fakeCluster(t, id, other)
	socket, requests := fakeService(t, func(q home.Request) home.Response {
		return home.Response{Status: &home.Status{ID: q.ID, Phase: "retained"}}
	})
	if code, _, errOut := runCLI("pull", "4215", testSocketFlag, socket); code != 0 {
		t.Fatalf("pull exited %d: %s", code, errOut)
	}
	last := (*requests)[len(*requests)-1]
	if last.Action != home.ActionPull || last.ID != id {
		t.Fatalf("pull request %+v", last)
	}
	code, _, errOut := runCLI("pull", "42", testSocketFlag, socket)
	if code == 0 || !strings.Contains(errOut, "matches 2 sessions") {
		t.Fatalf("ambiguous prefix: %d %s", code, errOut)
	}
}

// status --remote reads the default cluster.
func TestStatusRemoteReadsTheCluster(t *testing.T) {
	id := "4215d7d1-68ff-47ed-9dad-dbf1c8317d38"
	fakeCluster(t, id)
	if code, out, errOut := runCLI("status", "4215", "--remote", "--json"); code != 0 || !strings.Contains(out, id) {
		t.Fatalf("status --remote exited %d: %s %s", code, out, errOut)
	}
}

// An unreachable default cluster is named by its context, not as an executor.
func TestUnreachableClusterIsNamedByItsContext(t *testing.T) {
	c := catalogue{Executors: []executorAvailability{
		{Executor: localName, Availability: availabilityAvailable},
		{Context: "sproozi", Availability: availabilityUnavailable},
	}, problems: map[string]error{"context sproozi": errors.New("connection refused")}}
	q := &options{}
	stderr := captureStderr(t, func() {
		if err := c.failure(q); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "Not listed: cluster (context sproozi) is unreachable: connection refused") {
		t.Fatalf("stderr %q", stderr)
	}
}

// captureStderr returns what fn writes to the process's standard error.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous }()
	fn()
	_ = writer.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
