package modelauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const selectedModel = "gpt-6.1-sol"

type modelTransport struct{ target *url.URL }

func (rt modelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.URL.Scheme, copy.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(copy)
}

func TestModelMissingFromCatalogRequiresCompletedExactInference(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		status       int
		valid        bool
	}{
		{"completed", `{"type":"response.completed","response":{"status":"completed","model":"gpt-6.1-sol"}}`, 200, true},
		{"late failure", `{"type":"response.failed","response":{"error":{"message":"private-provider-detail"}}}`, 200, false},
		{"incomplete", `{"type":"response.incomplete"}`, 200, false},
		{"truncated", `{"type":"response.created"}`, 200, false},
		{"wrong model", `{"type":"response.completed","response":{"status":"completed","model":"different-model"}}`, 200, false},
		{"rejected", `{"error":{"message":"private-provider-detail"}}`, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+fixtureAccess {
					t.Error("missing provider authentication")
				}
				if r.URL.Path == "/v1/models" {
					_, _ = fmt.Fprint(w, `{"models":[{"slug":"other","visibility":"list"}]}`)
					return
				}
				calls++
				var body struct {
					Model         string
					Store, Stream bool
					Input         []json.RawMessage
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || json.NewDecoder(r.Body).Decode(&body) != nil ||
					body.Model != selectedModel || body.Store || !body.Stream || len(body.Input) != 1 {
					t.Error("invalid exact-model probe")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.stream)
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			err := checkModel(context.Background(), &http.Client{Transport: modelTransport{target}}, Session{AccessToken: fixtureAccess}, selectedModel)
			if (err == nil) != tc.valid || calls != 1 {
				t.Fatalf("model verification = %v; inference calls = %d", err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "private-provider-detail") {
				t.Fatal("provider details leaked")
			}
		})
	}
}

func TestListedModelNeedsNoInference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Error("listed model incurred inference")
		}
		_, _ = fmt.Fprintf(w, `{"models":[{"slug":%q,"visibility":"list"}]}`, selectedModel)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	if err := checkModel(context.Background(), &http.Client{Transport: modelTransport{target}}, Session{AccessToken: fixtureAccess}, selectedModel); err != nil {
		t.Fatal(err)
	}
}
