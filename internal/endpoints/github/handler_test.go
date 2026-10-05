/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github_test

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	testFixtureBaseBranch = "main"
)

const (
	testRepo    = "andrewmccall/home-ops"
	testGHToken = "ghs_faketoken"
	testSA      = "run-abc-sa"
	testRunID   = "run-abc"
)

// buildHandler returns a Handler wired to in-process upstream handlers.
// gitSrv and apiSrv capture forwarded requests so tests can assert on them.
func buildHandler(t *testing.T, identity *gateway.RunIdentity, gitSrv, apiSrv *inProcessServer) *identityHandler {
	t.Helper()
	auditLog := &discardLogger{}
	return &identityHandler{
		Config: identityConfig{
			Identity:      identity,
			TokenProvider: &githubgateway.FakeTokenProvider{GitHubToken: testGHToken},
			AuditLogger:   auditLog,
			GitBaseURL:    gitSrv.URL,
			APIBaseURL:    apiSrv.URL,
			HTTPClient:    &http.Client{Transport: inProcessTransport{git: gitSrv.handler, api: apiSrv.handler}},
		},
	}
}

// makeIdentity builds a RunIdentity with github.pull_request capability and
// the given repository in the policy.
func makeIdentity(repos []string) *gateway.RunIdentity {
	run := &sprooziv1alpha1.AgentRun{}
	run.Name = testRunID
	run.UID = types.UID("run-abc-uid")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}

	policy := &sprooziv1alpha1.AgentPolicy{}
	policy.Name = "test-policy"
	policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}
	policy.Spec.GitHubPullRequest = sprooziv1alpha1.GitHubPullRequestScope{Repositories: repos, AllowedBaseBranches: []string{testFixtureBaseBranch}}

	return &gateway.RunIdentity{Run: run, Policy: policy, Template: &sprooziv1alpha1.AgentTemplate{}, SAName: testSA}
}

// pktLine builds a single git pkt-line for testing.
func pktLine(s string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(s)+4, s))
}

// flushPacket is the git pkt-line flush marker.
func flushPacket() []byte { return []byte("0000") }

// discardLogger drops audit events.
type discardLogger struct{}

func (d *discardLogger) Log(_ audit.Event) {}

type inProcessServer struct {
	URL     string
	handler http.Handler
}

func (s *inProcessServer) Close() {}

func (s *inProcessServer) Client() *http.Client {
	return &http.Client{Transport: inProcessTransport{git: s.handler, api: s.handler}}
}

type inProcessTransport struct {
	git http.Handler
	api http.Handler
}

func (t inProcessTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	handler := t.git
	if strings.Contains(r.URL.Host, "api.test") {
		handler = t.api
	}
	if handler == nil {
		return nil, fmt.Errorf("no in-process upstream for %s", r.URL.Host)
	}
	rr := httptest.NewRecorder()
	upstreamRequest := httptest.NewRequest(r.Method, r.URL.String(), r.Body)
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Host = r.Host
	handler.ServeHTTP(rr, upstreamRequest)
	response := rr.Result()
	response.Request = r
	return response, nil
}

// echoServer returns an in-process upstream that echoes the Authorization
// header it received. No listener is created.
func echoServer(t *testing.T) *inProcessServer {
	t.Helper()
	return &inProcessServer{URL: "http://git.test", handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Received-Auth", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, r.Body)
	})}
}

func customServer(handler http.Handler, url string) *inProcessServer {
	return &inProcessServer{URL: url, handler: handler}
}

// TestCloneAllowedRepo verifies that git-upload-pack for an allowed repository
// is forwarded with GitHub App Basic auth and the sandbox token is stripped.
func TestCloneAllowedRepo(t *testing.T) {
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	req := httptest.NewRequest(http.MethodGet,
		"/andrewmccall/home-ops.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	auth := rr.Header().Get("X-Received-Auth")
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("expected Basic auth forwarded, got %q", auth)
	}
	decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if string(decoded) != "x-access-token:"+testGHToken {
		t.Fatalf("expected GitHub App credential, got %q", string(decoded))
	}
}

// TestForbiddenRepo verifies that a repository not in the policy is rejected.
func TestForbiddenRepo(t *testing.T) {
	reached := false
	gitSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), "http://git.test")
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	req := httptest.NewRequest(http.MethodGet,
		"/other-owner/other-repo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if reached {
		t.Fatal("forbidden repository request must not reach upstream")
	}
}

// TestPushToRunOwnedBranch verifies that creating a run-owned branch is forwarded.
func TestPushToRunOwnedBranch(t *testing.T) {
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	// Build a git-receive-pack body for the immutable run-owned branch namespace.
	old := strings.Repeat("0", 40)
	new := strings.Repeat("a", 40)
	refLine := fmt.Sprintf("%s %s refs/heads/sproozi/run-abc-uid/fix-image-tag\x00 side-band-64k", old, new)
	body := append(pktLine(refLine+"\n"), flushPacket()...)
	body = append(body, []byte("PACKFILE-DATA")...)

	req := httptest.NewRequest(http.MethodPost,
		"/andrewmccall/home-ops.git/git-receive-pack",
		strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sandbox-token")
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPushToDefaultBranchMain verifies that a push targeting refs/heads/main is rejected.
func TestPushToDefaultBranchMain(t *testing.T) {
	reached := false
	gitSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), "http://git.test")
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	old := strings.Repeat("0", 40)
	new := strings.Repeat("b", 40)
	refLine := fmt.Sprintf("%s %s refs/heads/main\x00 side-band-64k", old, new)
	body := append(pktLine(refLine+"\n"), flushPacket()...)

	req := httptest.NewRequest(http.MethodPost,
		"/andrewmccall/home-ops.git/git-receive-pack",
		strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if reached {
		t.Fatal("default-branch push must not reach upstream")
	}
}

// TestPushToDefaultBranchMaster verifies that refs/heads/master is also rejected.
func TestPushToDefaultBranchMaster(t *testing.T) {
	reached := false
	gitSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), "http://git.test")
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	old := strings.Repeat("0", 40)
	new := strings.Repeat("c", 40)
	refLine := fmt.Sprintf("%s %s refs/heads/master", old, new)
	body := append(pktLine(refLine+"\n"), flushPacket()...)

	req := httptest.NewRequest(http.MethodPost,
		"/andrewmccall/home-ops.git/git-receive-pack",
		strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for master push, got %d", rr.Code)
	}
	if reached {
		t.Fatal("default-branch push must not reach upstream")
	}
}

// TestPRCreateAllowed verifies that POST /repos/{owner}/{repo}/pulls is
// proxied with bearer token auth.
func TestPRCreateAllowed(t *testing.T) {
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testGHToken {
			t.Fatal("missing gateway credentials")
		}
		w.Header().Set("X-Received-Auth", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/andrewmccall/home-ops/pull/42","head":{"ref":"sproozi/run-abc-uid/fix-image-tag","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main","repo":{"full_name":"andrewmccall/home-ops"}}}`))
	}), "http://api.test")
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	req := httptest.NewRequest(http.MethodPost,
		"/repos/andrewmccall/home-ops/pulls",
		strings.NewReader(`{"title":"fix image","head":"sproozi/run-abc-uid/fix-image-tag","base":"main"}`))
	req.Header.Set("Authorization", "Bearer sandbox-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	auth := rr.Header().Get("X-Received-Auth")
	if auth != "Bearer "+testGHToken {
		t.Fatalf("expected Bearer token forwarded, got %q", auth)
	}
}

// TestPRCreateForbiddenRepo verifies that PR creation for a foreign repo is rejected.
func TestPRCreateForbiddenRepo(t *testing.T) {
	reached := false
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusCreated)
	}), "http://api.test")
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	req := httptest.NewRequest(http.MethodPost,
		"/repos/foreign-owner/other-repo/pulls",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if reached {
		t.Fatal("forbidden PR create must not reach upstream")
	}
}

// TestMergeAttemptDenied verifies that PUT /repos/{owner}/{repo}/pulls/{n}/merge
// is rejected — the proxy only permits POST .../pulls.
func TestMergeAttemptDenied(t *testing.T) {
	reached := false
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), "http://api.test")
	defer apiSrv.Close()

	h := buildHandler(t, makeIdentity([]string{testRepo}), gitSrv, apiSrv)

	req := httptest.NewRequest(http.MethodPut,
		"/repos/andrewmccall/home-ops/pulls/42/merge", nil)
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for merge attempt, got %d", rr.Code)
	}
	if reached {
		t.Fatal("merge operation must not reach upstream")
	}
}

// TestMissingCapability verifies that a run without github.pull_request is rejected.
func TestMissingCapability(t *testing.T) {
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	identity := makeIdentity([]string{testRepo})
	identity.Run.Spec.Capabilities = nil // no capabilities

	h := buildHandler(t, identity, gitSrv, apiSrv)

	req := httptest.NewRequest(http.MethodGet,
		"/andrewmccall/home-ops.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer sandbox-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing capability, got %d", rr.Code)
	}
}

// TestAuthenticationFailure verifies that an invalid token is rejected.
func TestAuthenticationFailure(t *testing.T) {
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := &identityHandler{
		Config: identityConfig{
			TokenProvider: &githubgateway.FakeTokenProvider{GitHubToken: testGHToken},
			AuditLogger:   &discardLogger{},
			GitBaseURL:    gitSrv.URL,
			APIBaseURL:    apiSrv.URL,
			HTTPClient:    gitSrv.Client(),
		},
	}

	req := httptest.NewRequest(http.MethodGet,
		"/andrewmccall/home-ops.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// TestMissingGatewayIdentity verifies that semantic handlers reject requests
// which have not crossed the shared gateway authentication boundary.
func TestMissingGatewayIdentity(t *testing.T) {
	gitSrv := echoServer(t)
	defer gitSrv.Close()
	apiSrv := echoServer(t)
	defer apiSrv.Close()

	h := &identityHandler{Config: identityConfig{
		TokenProvider: &githubgateway.FakeTokenProvider{GitHubToken: testGHToken},
		AuditLogger:   &discardLogger{},
		GitBaseURL:    gitSrv.URL,
		APIBaseURL:    apiSrv.URL,
		HTTPClient:    gitSrv.Client(),
	}}

	req := httptest.NewRequest(http.MethodGet,
		"/andrewmccall/home-ops.git/info/refs?service=git-upload-pack", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}
