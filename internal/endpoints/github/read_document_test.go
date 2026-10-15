package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGraphQLReadDocumentBoundary(t *testing.T) {
	repository := `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id name owner { login } defaultBranchRef { name } } } # supported metadata
`
	pr := `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number) { number url repository { nameWithOwner } } } }`
	tests := []struct {
		name, query string
		variables   map[string]any
		status      int
	}{
		{"repository metadata", repository, map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 200},
		{"PR metadata", pr, map[string]any{ownerVariable: fixtureRepositoryOwner, repoVariable: fixtureRepositoryName, prNumberVariable: 42}, 200},
		{"repository argument must use approved variable", strings.Replace(repository, "owner: $owner", `owner: "other"`, 1), map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
		{"PR number must use approved variable", strings.Replace(pr, "number: $pr_number", "number: 7", 1), map[string]any{ownerVariable: fixtureRepositoryOwner, repoVariable: fixtureRepositoryName, prNumberVariable: 42}, 403},
		{"nested fields are bounded", strings.Replace(repository, "owner { login }", "owner { login repositories { totalCount } }", 1), map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
		{"directives are unsupported", strings.Replace(repository, "id name", "id @skip(if: true) name", 1), map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
		{"aliases are unsupported", strings.Replace(repository, "id name", "node: id name", 1), map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
		{"trailing document is unsupported", repository + " query Other { viewer { login } }", map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
		{"extra literal cannot disappear during lexing", strings.Replace(repository, "id name", `id "ignored" name`, 1), map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
		{"empty selection is unsupported", strings.Replace(repository, "owner { login }", "owner { }", 1), map[string]any{ownerVariable: fixtureRepositoryOwner, nameVariable: fixtureRepositoryName}, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			handler := protocolHandler(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				var got graphQLEnvelope
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				wantTokens, _ := lexGraphQL(tt.query)
				if got.Query != strings.Join(wantTokens, " ") {
					t.Fatalf("forwarded document = %s", got.Query)
				}
				for key, want := range tt.variables {
					encoded, _ := json.Marshal(want)
					if string(got.Variables[key]) != string(encoded) {
						t.Fatalf("forwarded %s = %s", key, got.Variables[key])
					}
				}
				return graphqlResponse(`{"data":{"repository":{"id":"R_fixture"}}}`), nil
			})}, &protocolAuditWriter{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, graphqlRequest(t, tt.query, tt.variables))
			if response.Code != tt.status {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body.String(), tt.status)
			}
			wantCalls := 0
			if tt.status == 200 {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("upstream calls=%d want %d", calls, wantCalls)
			}
		})
	}
}
