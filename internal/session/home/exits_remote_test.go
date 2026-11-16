package home

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

const wireToken = "home-exit-watcher-principal-token-32-bytes"

type wireAuthorizer struct{}

func (wireAuthorizer) Authorize(_ context.Context, token string) error {
	if token != wireToken {
		return profile.ErrUnauthenticated
	}
	return nil
}

// wireService is testService whose remote placement is reached through the
// placement protocol over TLS, as home reaches a remote executor; the
// executor's placement is the remote fixture.
func wireService(t *testing.T) (*Service, *placementFixture, *placementFixture, string) {
	t.Helper()
	base, local, remote, repo := testService(t)
	config := base.config
	h, err := profile.NewHandler(profile.Config{Harness: config.Local.Harness, Principal: "developer", Authorizer: wireAuthorizer{},
		Placement: testRemote, InstructionDigest: config.Local.InstructionDigest, Allowances: config.Allowances,
		MaxCheckpointBytes: testLimit, Backend: remote})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(h)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client, err := profile.Connect(profile.Connection{Origin: server.URL, Token: wireToken, Roots: roots, MaxCheckpointBytes: testLimit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	dir := filepath.Join(testutil.PrivateDir(t), "home")
	config.Directory = dir
	placements := map[string]session.Placement{testLocal: local, testRemote: client}
	coordinator, err := session.Open(filepath.Join(dir, "journal"), placements, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{config: config, duration: time.Hour, stacks: []*localStack{{config: config.Local}}, placements: placements, coordinator: coordinator}
	t.Cleanup(func() { _ = s.Close() })
	return s, local, remote, repo
}

// Home's exit watcher notices, over the placement protocol, that the agent
// of a session it moved to a remote executor exited, and stops it through the
// coordinator and the protocol's stop: the session shows the exit and keeps
// the checkpoint it had (ADR 0062).
func TestExitWatcherStopsAnExitedRemoteVisitOverTheProtocol(t *testing.T) {
	t.Parallel()
	s, _, remote, repo := wireService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.sweepExits(t.Context()):
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not finish")
	}
	if status, err := s.Status(id); err != nil || status.Phase == stageFailed {
		t.Fatalf("a running remote agent was settled: %+v %v", status, err)
	}
	stopsBefore := remote.stops
	remote.exited = &session.Exit{Code: 2, Cause: "exited", Output: "remote agent failed"}
	select {
	case <-s.sweepExits(t.Context()):
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not finish")
	}
	status, err := s.Status(id)
	if err != nil || status.Phase != stageFailed || status.Exit == nil || status.Exit.Code != 2 || remote.stops != stopsBefore+1 {
		t.Fatalf("exited remote session: %+v %v, %d stops", status, err, remote.stops-stopsBefore)
	}
}
