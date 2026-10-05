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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func graphqlRequest(t *testing.T, query string, variables map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{graphqlQuery: query, variablesField: variables, "operationName": operationNameFromQuery(query)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer projected-run-token")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func operationNameFromQuery(query string) string {
	parts := strings.Fields(query)
	if len(parts) >= 2 {
		if index := strings.IndexByte(parts[1], '('); index >= 0 {
			return parts[1][:index]
		}
		return parts[1]
	}
	return ""
}

func graphqlResponse(body string) *http.Response {
	return &http.Response{Status: fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK)), StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGraphQLRepositoryInfoForwardsWithProviderCredential(t *testing.T) {
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Path != "/graphql" || request.Header.Get("Authorization") != fixtureProviderAuthorization {
			return nil, fmt.Errorf("unexpected upstream request: %s %s", request.Method, request.URL)
		}
		body, _ := io.ReadAll(request.Body)
		if bytes.Contains(body, []byte("projected-run-token")) {
			return nil, fmt.Errorf("sandbox credential reached upstream")
		}
		return graphqlResponse(`{"data":{"repository":{"id":"R_fixture","name":"home-ops","owner":{"login":"andrewmccall"},"defaultBranchRef":{"name":"main"}}}}`), nil
	})}
	events := &protocolAuditWriter{}
	handler := protocolHandler(client, events)
	query := `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id name owner { login } defaultBranchRef { name } } }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"R_fixture"`) {
		t.Fatalf("unexpected GraphQL response: %d %s", response.Code, response.Body.String())
	}
	if requests != 1 || len(events.events) != 1 || !events.events[0].Allowed {
		t.Fatalf("expected one forwarded request and one audit event: requests=%d events=%#v", requests, events.events)
	}
	encoded, _ := json.Marshal(events.events[0])
	if bytes.Contains(encoded, []byte("projected-run-token")) || bytes.Contains(encoded, []byte("ghs_fixture_provider_token")) || bytes.Contains(encoded, []byte(query)) {
		t.Fatalf("audit event contains request secret or body data: %s", encoded)
	}
	if events.events[0].Resource != "andrewmccall/home-ops" || events.events[0].Capability != "github.pull_request" {
		t.Fatalf("audit must record the authorized repository: %+v", events.events[0])
	}
}

func TestGraphQLCreateResolvesAndReadsBack(t *testing.T) {
	events := &protocolAuditWriter{}
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != fixtureProviderAuthorization {
			return nil, fmt.Errorf("provider credential was not injected")
		}
		body, _ := io.ReadAll(request.Body)
		if bytes.Contains(body, []byte("projected-run-token")) {
			return nil, fmt.Errorf("sandbox credential reached upstream")
		}
		var envelope graphQLEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, err
		}
		if strings.Contains(envelope.Query, "query RepositoryByID") {
			requests = append(requests, "RepositoryByID")
			return graphqlResponse(`{"data":{"node":{"nameWithOwner":"andrewmccall/home-ops"}}}`), nil
		}
		operation, err := classifyGraphQL(envelope.Query)
		if err != nil {
			return nil, err
		}
		requests = append(requests, operation.Name)
		switch operation.Name {
		case pullRequestCreateOperation:
			return graphqlResponse(`{"data":{"createPullRequest":{"pullRequest":{"id":"PR_fixture","url":"https://github.com/andrewmccall/home-ops/pull/42"}}}}`), nil
		case pullRequestByNumberOperation:
			return graphqlResponse(`{"data":{"repository":{"pullRequest":{"number":42,"url":"https://github.com/andrewmccall/home-ops/pull/42","headRefName":"sproozi/run-abc-uid/fix-image","headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","baseRefName":"main","repository":{"nameWithOwner":"andrewmccall/home-ops"}}}}}`), nil
		default:
			return nil, fmt.Errorf("unexpected operation %s", operation.Name)
		}
	})}
	handler := protocolHandler(client, events)
	query := pullRequestCreateDocument
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{inputVariable: map[string]any{
		baseRefNameField: fixtureBaseBranch, "body": "fixture body", "draft": false,
		headRefNameField: fixtureRunBranch, "maintainerCanModify": true,
		repositoryIDField: fixtureRepositoryID, titleField: "fixture change",
	}}))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"PR_fixture"`) {
		t.Fatalf("unexpected mutation response: %d %s", response.Code, response.Body.String())
	}
	if got, want := strings.Join(requests, ","), "RepositoryByID,PullRequestCreate,PullRequestByNumber"; got != want {
		t.Fatalf("unexpected trusted request sequence: got %s want %s", got, want)
	}
	if len(events.events) != 1 || !events.events[0].Allowed {
		t.Fatalf("expected one allow audit event: %#v", events.events)
	}
	if events.events[0].ArtifactURL != "https://github.com/andrewmccall/home-ops/pull/42" {
		t.Fatal("verified PR URL is absent from audit")
	}
}

func TestGraphQLCreateForeignRepositoryDoesNotMutate(t *testing.T) {
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return graphqlResponse(`{"data":{"node":{"nameWithOwner":"foreign/repository"}}}`), nil
	})}
	events := &protocolAuditWriter{}
	handler := protocolHandler(client, events)
	query := pullRequestCreateDocument
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{inputVariable: map[string]any{
		baseRefNameField: fixtureBaseBranch, headRefNameField: fixtureRunBranch, repositoryIDField: "R_foreign", titleField: fixtureText,
	}}))
	if response.Code != http.StatusForbidden || requests != 1 {
		t.Fatalf("foreign repository must be denied before mutation: status=%d requests=%d", response.Code, requests)
	}
	if len(events.events) != 1 || events.events[0].Allowed {
		t.Fatalf("expected redacted deny audit event: %#v", events.events)
	}
}

func TestGraphQLCreateRejectsLiteralMutationArgumentsBeforeUpstream(t *testing.T) {
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return graphqlResponse(`{}`), nil
	})}
	handler := protocolHandler(client, &protocolAuditWriter{})
	query := `mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: {repositoryId: "R_foreign", headRefName: "sproozi/run-abc-uid/other", baseRefName: "main", title: "bad"}) { pullRequest { id url } } }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{inputVariable: map[string]any{
		baseRefNameField: fixtureBaseBranch, headRefNameField: fixtureRunBranch, repositoryIDField: fixtureRepositoryID, titleField: fixtureText,
	}}))
	if response.Code != http.StatusForbidden || requests != 0 {
		t.Fatalf("literal mutation arguments must be denied before upstream: status=%d requests=%d", response.Code, requests)
	}
}

func TestGraphQLCreateRejectsUnsupportedDocumentShapesBeforeUpstream(t *testing.T) {
	shapes := []string{
		`mutation PullRequestCreate($input: CreatePullRequestInput!, $unused: String!) { createPullRequest(input: $input) { pullRequest { id url } } }`,
		`mutation PullRequestCreate($input: CreatePullRequestInput! = {}) { createPullRequest(input: $input) { pullRequest { id url } } }`,
		`mutation PullRequestCreate($input: CreatePullRequestInput!) @skip(if: false) { createPullRequest(input: $input) { pullRequest { id url } } }`,
		`mutation PullRequestCreate($input: CreatePullRequestInput!) { alias: createPullRequest(input: $input) { pullRequest { id url } } }`,
		`mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id url title } } }`,
		`mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id url } } } mutation Other($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id url } } }`,
	}
	for _, query := range shapes {
		t.Run(query, func(t *testing.T) {
			called := false
			handler := protocolHandler(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				called = true
				return graphqlResponse(`{}`), nil
			})}, &protocolAuditWriter{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{inputVariable: map[string]any{
				baseRefNameField: fixtureBaseBranch, headRefNameField: fixtureRunBranch, repositoryIDField: fixtureRepositoryID, titleField: fixtureText,
			}}))
			if response.Code != http.StatusForbidden || called {
				t.Fatalf("unsupported mutation shape contacted upstream: status=%d called=%v", response.Code, called)
			}
		})
	}
}

func TestGraphQLRejectsUnsupportedRootAndOutOfScopeHead(t *testing.T) {
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return nil, fmt.Errorf("unsupported operation reached upstream")
	})}
	handler := protocolHandler(client, &protocolAuditWriter{})
	unsupported := `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } viewer { login } }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, unsupported, map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}))
	if response.Code != http.StatusForbidden || requests != 0 {
		t.Fatalf("multiple root fields must be denied: status=%d requests=%d", response.Code, requests)
	}
	query := pullRequestCreateDocument
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{inputVariable: map[string]any{
		baseRefNameField: fixtureBaseBranch, headRefNameField: "refs/heads/main", repositoryIDField: fixtureRepositoryID, titleField: fixtureText,
	}}))
	if response.Code != http.StatusForbidden || requests != 0 {
		t.Fatalf("out-of-scope head must be denied before resolution: status=%d requests=%d", response.Code, requests)
	}
}

func TestGraphQLPRViewForwardsPolicyListedRepository(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != fixtureProviderAuthorization {
			return nil, fmt.Errorf("missing provider credential")
		}
		return graphqlResponse(`{"data":{"repository":{"pullRequest":{"number":42}}}}`), nil
	})}
	events := &protocolAuditWriter{}
	handler := protocolHandler(client, events)
	query := `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number) { number } } }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{ownerVariable: fixtureRepositoryOwner, repoVariable: fixtureRepositoryName, "pr_number": 42}))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"number":42`) || len(events.events) != 1 || !events.events[0].Allowed {
		t.Fatalf("PR view was not forwarded: %d %s events=%#v", response.Code, response.Body.String(), events.events)
	}
}

func TestGraphQLRejectsUnknownOperationWithoutUpstreamCall(t *testing.T) {
	called := false
	handler := protocolHandler(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		return graphqlResponse(`{}`), nil
	})}, &protocolAuditWriter{})
	query := `query ViewerInfo { viewer { login } }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{}))
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("unknown GraphQL operation was not denied atomically: status=%d called=%v", response.Code, called)
	}
}

func TestGraphQLRejectsFragmentsBeforeUpstreamCall(t *testing.T) {
	called := false
	handler := protocolHandler(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		return graphqlResponse(`{}`), nil
	})}, &protocolAuditWriter{})
	query := `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { ...RepoFields } } fragment RepoFields on Repository { id }`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, graphqlRequest(t, query, map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}))
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("fragment GraphQL request was not denied atomically: status=%d called=%v", response.Code, called)
	}
}
