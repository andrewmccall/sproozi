package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	fixtureRepositoryName = "home-ops"
)

const (
	fixtureProviderAuthorization = "Bearer ghs_fixture_provider_token"
	fixtureRepositoryOwner       = "andrewmccall"
	fixtureRepositoryID          = "R_fixture"
	fixtureText                  = "fixture"
	fixtureBaseBranch            = "main"
)

func TestGitPathParserDoesNotAuthorizeLookalikeRepositoryPaths(t *testing.T) {
	for _, path := range []string{
		"/owner/repo.git.evil/info/refs",
		"/owner/repo.git/administration",
		"/owner/repo.git/info/refs/extra",
		"/owner/repo.git//git-upload-pack",
	} {
		if isGitPath(path) {
			t.Errorf("isGitPath(%q) = true, want false", path)
		}
	}
	if got, ok := parseGitRepo("/owner/repo.git/info/refs"); !ok || got != "owner/repo" {
		t.Fatalf("parseGitRepo(valid) = %q, %v", got, ok)
	}
}

func TestGraphQLUpstreamRedirectIsNotFollowed(t *testing.T) {
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://evil.invalid/graphql"}},
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})}
	handler := protocolHandler(client, &protocolAuditWriter{})
	query := `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}))
	if response.Code != http.StatusFound || requests != 1 {
		t.Fatalf("redirect must be returned without a second request: status=%d requests=%d", response.Code, requests)
	}
}

func TestGraphQLDoesNotForwardSandboxCredentialHeaders(t *testing.T) {
	var received http.Header
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		received = request.Header.Clone()
		if request.Header.Get("Authorization") != fixtureProviderAuthorization || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" {
			return nil, fmt.Errorf("credential header leaked upstream: %#v", request.Header)
		}
		return graphqlResponse(`{"data":{"repository":{"id":"R_fixture"}}}`), nil
	})}
	handler := protocolHandler(client, &protocolAuditWriter{})
	query := `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }`
	request := graphqlRequest(t, query, map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName})
	request.Header.Set("Proxy-Authorization", "Bearer sandbox-token")
	request.Header.Set("Cookie", "credential=sandbox-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: %d %s received=%#v", response.Code, response.Body.String(), received)
	}
}

func TestGraphQLRejectsAmbiguousRunBranch(t *testing.T) {
	called := false
	handler := protocolHandler(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		return nil, fmt.Errorf("ambiguous branch reached upstream")
	})}, &protocolAuditWriter{})
	query := pullRequestCreateDocument
	request := graphqlRequest(t, query, map[string]any{inputVariable: map[string]any{
		baseRefNameField: fixtureBaseBranch, headRefNameField: "sproozi/run-abc-uid/fix..image", repositoryIDField: fixtureRepositoryID, titleField: fixtureText,
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("ambiguous run branch was not denied before upstream: status=%d called=%v", response.Code, called)
	}
}
