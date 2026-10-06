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

package github

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	fixtureRunBranch = "sproozi/run-abc-uid/fix-image"
)

const protocolFixtureEnvironment = "SPROOZI_PROTOCOL_FIXTURE"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type protocolAuditWriter struct{ events []audit.Event }

func (w *protocolAuditWriter) Log(event audit.Event) { w.events = append(w.events, event) }

func protocolIdentity() *gateway.RunIdentity {
	run := &sprooziv1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "run-abc", Namespace: "default", UID: types.UID("run-abc-uid"),
	}}
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}
	policy := &sprooziv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "fixture-policy", Generation: 7}}
	policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}
	policy.Spec.GitHubPullRequest = sprooziv1alpha1.GitHubPullRequestScope{
		Repositories:        []string{"andrewmccall/home-ops"},
		AllowedBaseBranches: []string{fixtureBaseBranch},
	}
	return &gateway.RunIdentity{Run: run, Policy: policy, Template: &sprooziv1alpha1.AgentTemplate{}, SAName: "run-abc-sa"}
}

func protocolHandler(client *http.Client, events audit.Writer) *identityHandler {
	return &identityHandler{Config: identityConfig{
		Identity:      protocolIdentity(),
		TokenProvider: &FakeTokenProvider{GitHubToken: "ghs_fixture_provider_token"},
		AuditLogger:   events,
		GitBaseURL:    "https://github.fixture.invalid",
		APIBaseURL:    "https://api.fixture.invalid",
		HTTPClient:    client,
	}}
}

func requireProtocolFixture(t *testing.T, tool string) string {
	t.Helper()
	if os.Getenv(protocolFixtureEnvironment) != "1" {
		t.Skip("pinned executable fixture is enabled by make verify-protocol")
	}
	lock, err := os.ReadFile(filepath.Join("..", "..", "..", "hack", "demo", "codex.Dockerfile"))
	if err != nil {
		t.Fatalf("read pinned client versions: %v", err)
	}
	pattern := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(strings.ToUpper(tool)) + `_VERSION=([^\s]+)$`)
	match := pattern.FindSubmatch(lock)
	if len(match) != 2 {
		t.Fatalf("%s is not pinned in hack/demo/codex.Dockerfile", tool)
	}
	return string(match[1])
}

func requireToolVersion(t *testing.T, tool, want string) {
	t.Helper()
	output, err := exec.Command(tool, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("run pinned %s client: %v: %s", tool, err, output)
	}
	if !regexp.MustCompile(`(^|[^0-9])` + regexp.QuoteMeta(want) + `([^0-9]|$)`).Match(output) {
		t.Fatalf("%s version mismatch: want %s, got %q", tool, want, strings.TrimSpace(string(output)))
	}
}

// decodeStatelessRPC unwraps the pkt-lines used between git-send-pack and its
// remote helper. Their concatenated payload is the byte-for-byte smart HTTP
// git-receive-pack request body.
func decodeStatelessRPC(data []byte) ([]byte, error) {
	var decoded bytes.Buffer
	for len(data) != 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("truncated stateless-rpc pkt-line")
		}
		var length int
		if _, err := fmt.Sscanf(string(data[:4]), "%04x", &length); err != nil {
			return nil, fmt.Errorf("invalid stateless-rpc pkt-line: %w", err)
		}
		data = data[4:]
		if length == 0 {
			if len(data) != 0 {
				return nil, fmt.Errorf("bytes follow stateless-rpc flush")
			}
			break
		}
		if length < 4 || length-4 > len(data) {
			return nil, fmt.Errorf("invalid stateless-rpc pkt-line length %d", length)
		}
		_, _ = decoded.Write(data[:length-4])
		data = data[length-4:]
	}
	return decoded.Bytes(), nil
}

// TestPinnedGitReceivePackProtocolFixture uses a real git executable, not a
// hand-built command line, to produce a receive-pack command and PACK body.
// The exact demo version is pinned in hack/demo/codex.Dockerfile, where that binary
// is built and used; this portable fixture verifies protocol semantics.
func TestPinnedGitReceivePackProtocolFixture(t *testing.T) {
	_ = requireProtocolFixture(t, "git")

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	remote := filepath.Join(dir, "remote.git")
	commands := [][]string{
		{"init", "-q", work},
		{"-C", work, "config", "user.email", "fixture@example.invalid"},
		{"-C", work, "config", "user.name", "Sproozi Fixture"},
		{"-C", work, "commit", "--allow-empty", "-q", "-m", fixtureText},
		{"-C", work, "branch", "-M", fixtureRunBranch},
		{"init", "--bare", "-q", remote},
	}
	for _, arguments := range commands {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}

	advertise := exec.Command("git", "receive-pack", "--stateless-rpc", "--advertise-refs", remote)
	advertisement, err := advertise.Output()
	if err != nil {
		t.Fatalf("advertise empty receive-pack: %v", err)
	}
	send := exec.Command("git", "-C", work, "send-pack", "--stateless-rpc", remote,
		"refs/heads/sproozi/run-abc-uid/fix-image:refs/heads/sproozi/run-abc-uid/fix-image")
	send.Stdin = bytes.NewReader(advertisement)
	var framed, stderr bytes.Buffer
	send.Stdout, send.Stderr = &framed, &stderr
	// No server response is connected: send-pack is expected to report a remote
	// disconnect after it has emitted the complete request. The fixture validates
	// the emitted protocol rather than treating that expected exit as success.
	_ = send.Run()
	body, err := decodeStatelessRPC(framed.Bytes())
	if err != nil {
		t.Fatalf("decode pinned git request: %v; stderr=%s", err, stderr.String())
	}
	if !bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("pinned git request omitted PACK payload (%d bytes)", len(body))
	}

	var forwarded []byte
	var forwardedAuthorization string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		forwardedAuthorization = request.Header.Get("Authorization")
		forwarded, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("0000")),
			Request:    request,
		}, nil
	})}
	events := &protocolAuditWriter{}
	handler := protocolHandler(client, events)
	request := httptest.NewRequest(http.MethodPost,
		"/andrewmccall/home-ops.git/git-receive-pack", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer projected-run-token")
	request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("gateway rejected pinned git receive-pack: %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(forwarded, body) {
		t.Fatal("gateway did not forward the pinned git request byte-for-byte")
	}
	decodedAuth, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(forwardedAuthorization, "Basic "))
	if err != nil || string(decodedAuth) != "x-access-token:ghs_fixture_provider_token" {
		t.Fatalf("gateway did not replace run identity with provider authority: %q", forwardedAuthorization)
	}
	if len(events.events) != 1 || !events.events[0].Allowed {
		t.Fatalf("expected one redacted allow audit event, got %#v", events.events)
	}
}

// TestPinnedGHPRCreateStartsWithGraphQL is a compatibility fixture. It invokes
// gh with every interactive input supplied and a deliberately unreachable proxy,
// so the client reveals its first request without any network listener or API
// mutation. gh 2.97.0 starts pr create with a fragment-based RepositoryInfo;
// the real document is denied. A reduced, fragment-free query is tested below
// separately and is not proof of compatibility with gh pr create.
func TestPinnedGHPRCreateStartsWithGraphQL(t *testing.T) {
	wantVersion := requireProtocolFixture(t, "gh")
	requireToolVersion(t, "gh", wantVersion)

	command := exec.Command("gh", "pr", "create",
		"--repo", "andrewmccall/home-ops",
		"--head", fixtureRunBranch,
		"--base", fixtureBaseBranch,
		"--title", "fixture change",
		"--body", "fixture body")
	command.Env = append(os.Environ(),
		"GH_TOKEN=sproozi-fixture-token",
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_DEBUG=api",
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"http_proxy=http://127.0.0.1:1",
		"https_proxy=http://127.0.0.1:1",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("gh unexpectedly reached a network service through the unreachable fixture proxy")
	}
	for _, expected := range []string{
		"POST /graphql HTTP/1.1",
		"Host: api.github.com",
		"query RepositoryInfo",
		"fragment repo on Repository",
		`GraphQL variables: {"name":"home-ops","owner":"andrewmccall"}`,
		"User-Agent: GitHub CLI " + wantVersion,
	} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Fatalf("pinned gh request did not contain %q; output:\n%s", expected, output)
		}
	}
	queryMatch := regexp.MustCompile(`(?s)GraphQL query:\n(.*?)\nGraphQL variables:`).FindSubmatch(output)
	if len(queryMatch) != 2 {
		t.Fatal("pinned gh did not expose its complete query")
	}
	if _, err := classifyGraphQL(string(queryMatch[1])); err == nil {
		t.Fatal("fragment-based gh pr create query must remain denied")
	}

	upstreamCalled := false
	var forwardedAuthorization string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamCalled = true
		forwardedAuthorization = request.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"repository":{"id":"R_fixture","name":"home-ops","owner":{"login":"andrewmccall"},"defaultBranchRef":{"name":"main"}}}}`)), Request: request}, nil
	})}
	handler := protocolHandler(client, &protocolAuditWriter{})
	body, _ := json.Marshal(map[string]any{
		graphqlQuery:   `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id name owner { login } defaultBranchRef { name } } }`,
		variablesField: map[string]string{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName},
	})
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer projected-run-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL RepositoryInfo was not forwarded, got %d: %s", response.Code, response.Body.String())
	}
	if !upstreamCalled || forwardedAuthorization != fixtureProviderAuthorization {
		t.Fatalf("GraphQL request did not reach upstream with provider credential: called=%v auth=%q", upstreamCalled, forwardedAuthorization)
	}
}

func TestPinnedGHAPICreatesPRUsingBoundedREST(t *testing.T) {
	wantVersion := requireProtocolFixture(t, "gh")
	requireToolVersion(t, "gh", wantVersion)
	command := exec.Command("gh", "api", "--method", "POST", "repos/andrewmccall/home-ops/pulls",
		"-f", "base=main", "-f", "head=sproozi/run-abc-uid/fix-image",
		"-f", "title=fixture change", "-f", "body=fixture body", "--jq", ".html_url")
	command.Env = append(os.Environ(),
		"GH_TOKEN=sproozi-fixture-token", "GH_DEBUG=api", "GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1", "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1",
		"http_proxy=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("gh unexpectedly reached a service through the unreachable fixture proxy")
	}
	for _, expected := range []string{"POST /repos/andrewmccall/home-ops/pulls HTTP/1.1", "Host: api.github.com"} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Fatalf("pinned gh REST request did not contain %q", expected)
		}
	}
	if bytes.Contains(output, []byte("POST /graphql")) {
		t.Fatal("bounded REST creation unexpectedly performed GraphQL discovery")
	}
}
