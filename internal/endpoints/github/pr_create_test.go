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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPRCreateForwardsOnlyValidatedFields verifies that GitHub receives the
// head and base the gateway checked. encoding/json matches keys regardless of
// case and keeps the last duplicate, so forwarding the original body would let
// a case variant or duplicate choose a different head or base.
func TestPRCreateForwardsOnlyValidatedFields(t *testing.T) {
	const pulls = "/repos/andrewmccall/home-ops/pulls"
	const created = `{"number":42,"html_url":"https://github.com/andrewmccall/home-ops/pull/42",` +
		`"head":{"ref":"sproozi/run-abc-uid/a","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
		`"base":{"ref":"main","repo":{"full_name":"andrewmccall/home-ops"}}}`
	allowed := `{"title":"t","body":"b","head":"sproozi/run-abc-uid/a","base":"main","draft":true,"maintainer_can_modify":false}`
	for _, tc := range []struct {
		name, target, body, forwarded string
	}{
		{name: "case variant", target: pulls,
			body: `{"head":"other","base":"release","HEAD":"sproozi/run-abc-uid/a","Base":"main"}`},
		{name: "duplicate key", target: pulls,
			body: `{"head":"other","head":"sproozi/run-abc-uid/a","base":"main"}`},
		{name: "unvalidated field", target: pulls,
			body: `{"head":"sproozi/run-abc-uid/a","base":"main","head_repo":"fork/home-ops"}`},
		{name: "issue conversion", target: pulls,
			body: `{"head":"sproozi/run-abc-uid/a","base":"main","issue":7}`},
		{name: "wrong type", target: pulls,
			body: `{"head":"sproozi/run-abc-uid/a","base":"main","draft":"yes"}`},
		{name: "query string", target: pulls + "?head=other",
			body: `{"head":"sproozi/run-abc-uid/a","base":"main"}`},
		{name: "trailing data", target: pulls,
			body: `{"head":"sproozi/run-abc-uid/a","base":"main"}{"head":"other"}`},
		{name: "allowed fields", target: pulls, body: allowed, forwarded: allowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received string
			apiSrv := customServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					body, _ := io.ReadAll(r.Body)
					received = string(body)
					if r.URL.RawQuery != "" {
						t.Errorf("query forwarded upstream: %q", r.URL.RawQuery)
					}
				}
				_, _ = w.Write([]byte(created))
			}), "http://api.test")
			h := buildHandler(t, makeIdentity([]string{testRepo}), echoServer(t), apiSrv)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(tc.body)))
			if tc.forwarded == "" {
				if rr.Code != http.StatusForbidden || received != "" {
					t.Fatalf("expected 403 before GitHub, got %d and upstream body %q", rr.Code, received)
				}
				return
			}
			if rr.Code != http.StatusOK || received != tc.forwarded {
				t.Fatalf("expected 200 forwarding %s, got %d forwarding %q", tc.forwarded, rr.Code, received)
			}
		})
	}
}
