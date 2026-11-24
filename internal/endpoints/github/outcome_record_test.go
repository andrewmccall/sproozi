package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const headerContentType = "Content-Type"

// The upstream requests a case can answer differently.
const (
	upstreamGit           = "git"
	upstreamRESTCreate    = "rest.create"
	upstreamRESTReadback  = "rest.readback"
	upstreamGraphQLCreate = "graphql.create"
	upstreamGraphQLRead   = "graphql.read"
)

var updateRecords = flag.Bool("update-records", false,
	"rewrite testdata/outcomes.txt from the current code")

// compareRecorded compares lines with the recorded file, or rewrites it under
// -update-records.
func compareRecorded(t *testing.T, file string, lines []string) {
	t.Helper()
	got := strings.Join(lines, "\n") + "\n"
	if *updateRecords {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d lines, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("outcome changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

// recordedEvents keeps every audit event a route or handler writes.
type recordedEvents struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordedEvents) Log(e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordedEvents) taken() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// outcomeLine is one recorded case: status, the headers that matter, the
// body, the ledger before and after, and each audit event as sorted-key JSON
// without its timestamp.
func outcomeLine(t *testing.T, name string, response *httptest.ResponseRecorder, ledger string, events []audit.Event) string {
	t.Helper()
	var headers []string
	for _, h := range []string{headerContentType, "X-Content-Type-Options", "Cache-Control", "Connection"} {
		if v := response.Header().Get(h); v != "" {
			headers = append(headers, strings.ToLower(h)+"="+v)
		}
	}
	recorded := make([]string, 0, len(events))
	for _, e := range events {
		recorded = append(recorded, eventJSON(t, e))
	}
	if len(recorded) == 0 {
		recorded = append(recorded, "none")
	}
	return strings.Join([]string{name, strconv.Itoa(response.Code), strings.Join(headers, " "),
		strconv.Quote(response.Body.String()), ledger, strings.Join(recorded, " ; ")}, " | ")
}

func eventJSON(t *testing.T, e audit.Event) string {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "timestamp")
	sorted, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(sorted)
}

// failingLedger refuses every read and write, as an unavailable store does.
type failingLedger struct{}

var errLedgerUnavailable = errors.New("ledger store unavailable")

func (failingLedger) Initialize(context.Context, budget.AccountKey) error {
	return errLedgerUnavailable
}
func (failingLedger) Reserve(context.Context, budget.AccountKey, budget.UsageReservation) (*budget.LedgerReservation, error) {
	return nil, errLedgerUnavailable
}
func (failingLedger) Settle(context.Context, *budget.LedgerReservation, budget.TrustedUsage) error {
	return errLedgerUnavailable
}
func (failingLedger) Release(context.Context, *budget.LedgerReservation) error {
	return errLedgerUnavailable
}
func (failingLedger) State(context.Context, budget.AccountKey) (budget.State, error) {
	return budget.State{}, errLedgerUnavailable
}
func (failingLedger) Seal(context.Context, budget.AccountKey) (budget.TrustedUsage, error) {
	return budget.TrustedUsage{}, errLedgerUnavailable
}

func ledgerReading(tracker *budget.Tracker, key string) string {
	if tracker == nil || key == "" {
		return "-"
	}
	state, err := tracker.State(key)
	if err != nil {
		return "error"
	}
	return fmt.Sprintf("used=%d reserved=%d cost=%d", state.UnitsUsed, state.ReservedUnits, state.CostMicros)
}

// recordTransport answers upstream requests in process.
type recordTransport func(*http.Request) (*http.Response, error)

func (f recordTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func recordResponse(r *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// reply answers an upstream request with a body of contentType.
func reply(code int, contentType, body string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		return recordResponse(r, code, http.Header{headerContentType: {contentType}}, body), nil
	}
}

// status answers an upstream request with a JSON body.
func status(code int, body string) func(*http.Request) (*http.Response, error) {
	return reply(code, "application/json", body)
}

// recordedGitHubUpstream answers each upstream request by its kind; replies
// replaces the default answer of a kind.
func recordedGitHubUpstream(replies map[string]func(*http.Request) (*http.Response, error)) *http.Client {
	const created = `{"number":42,"html_url":"https://github.com/andrewmccall/home-ops/pull/42",` +
		`"head":{"ref":"sproozi/run-abc-uid/a","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
		`"base":{"ref":"main","repo":{"full_name":"andrewmccall/home-ops"}}}`
	defaults := map[string]func(*http.Request) (*http.Response, error){
		upstreamGit:           reply(200, "application/x-git-upload-pack-advertisement", "001e# service=git-upload-pack\n0000"),
		upstreamRESTCreate:    status(201, created),
		upstreamRESTReadback:  status(200, created),
		"graphql.resolve":     status(200, `{"data":{"node":{"nameWithOwner":"andrewmccall/home-ops"}}}`),
		upstreamGraphQLCreate: status(200, `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_fixture","url":"https://github.com/andrewmccall/home-ops/pull/42"}}}}`),
		"graphql.readback":    status(200, `{"data":{"repository":{"pullRequest":{"number":42,"url":"https://github.com/andrewmccall/home-ops/pull/42","headRefName":"sproozi/run-abc-uid/a","headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","baseRefName":"main","repository":{"nameWithOwner":"andrewmccall/home-ops"}}}}}`),
		upstreamGraphQLRead:   status(200, `{"data":{"repository":{"id":"R_fixture"}}}`),
	}
	return &http.Client{Transport: recordTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		kind := "unknown"
		switch {
		case r.URL.Host == "git.test":
			kind = upstreamGit
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			kind = upstreamRESTCreate
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			kind = upstreamRESTReadback
		case bytes.Contains(body, []byte("RepositoryByID")):
			kind = "graphql.resolve"
		case bytes.Contains(body, []byte("mutation PullRequestCreate")):
			kind = upstreamGraphQLCreate
		case bytes.Contains(body, []byte("PullRequestByNumber")):
			kind = "graphql.readback"
		case bytes.Contains(body, []byte("RepositoryInfo")):
			kind = upstreamGraphQLRead
		}
		if reply, ok := replies[kind]; ok {
			return reply(r)
		}
		if reply, ok := defaults[kind]; ok {
			return reply(r)
		}
		return nil, fmt.Errorf("unexpected upstream request %s %s", r.Method, r.URL)
	})}
}

// TestGitHubOutcomesAreRecorded records what a github.pullRequest route
// replies, charges and audits for git, REST and GraphQL requests.
func TestGitHubOutcomesAreRecorded(t *testing.T) {
	const capability = api.CapabilityGitHubPullRequest
	const runUID = "run-abc-uid"
	grant := func(units int64) *gateway.ExecutionGrant {
		g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "run-abc", UID: runUID},
			Authority: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "run-abc-sa", UID: "sa-uid"},
			Policy: gateway.PolicySnapshot{Name: "test-policy", Generation: 6, Spec: api.AgentPolicySpec{
				AllowedCapabilities: []api.CapabilityKind{capability},
				GitHubPullRequest: api.GitHubPullRequestScope{Repositories: []string{"andrewmccall/home-ops"},
					AllowedBaseBranches: []string{"main"}},
			}},
			Template:     gateway.TemplateSnapshot{Name: "test-template"},
			Capabilities: []api.CapabilityKind{capability}}
		if units > 0 {
			g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{capability: {MaxUnits: units}}
		}
		return g
	}
	failed := func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") }
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	zero := strings.Repeat("0", 40)
	newOID := strings.Repeat("b", 40)
	push := func(old, ref string) string {
		return pkt(old+" "+newOID+" "+ref+"\x00report-status") + "0000PACK"
	}
	graphql := func(query string, variables map[string]any) string {
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	const createDocument = `mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id url } } }`
	const readDocument = `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id name owner { login } defaultBranchRef { name } } }`
	createInput := map[string]any{"input": map[string]any{"baseRefName": "main", "body": "fixture body", "draft": false,
		"headRefName": "sproozi/run-abc-uid/a", "maintainerCanModify": true, "repositoryId": "R_fixture", "title": "fixture change"}}
	readVariables := func(owner, name string) map[string]any { return map[string]any{"owner": owner, "name": name} }
	const restCreate = `{"title":"t","body":"b","head":"sproozi/run-abc-uid/a","base":"main"}`
	const git = "https://github.com/andrewmccall/home-ops.git"
	const pulls = "https://api.github.com/repos/andrewmccall/home-ops/pulls"
	const graphqlURL = "https://api.github.com/graphql"
	key := runUID + "/" + string(capability)
	cases := []struct {
		name, method, target, body string
		replies                    map[string]func(*http.Request) (*http.Response, error)
		tokenErr                   bool
		units, spent               int64
		failingLedger              bool
	}{
		{name: "git/discovery served", method: http.MethodGet, target: git + "/info/refs?service=git-upload-pack", units: 5},
		{name: "git/upload-pack served", method: http.MethodPost, target: git + "/git-upload-pack", body: "0000", units: 5},
		{name: "git/receive-pack served", method: http.MethodPost, target: git + "/git-receive-pack",
			body: push(zero, "refs/heads/sproozi/"+runUID+"/a"), units: 5},
		{name: "git/receive-pack update refused", method: http.MethodPost, target: git + "/git-receive-pack",
			body: push(strings.Repeat("c", 40), "refs/heads/main"), units: 5},
		{name: "git/invalid git URL", method: http.MethodGet, target: "https://github.com/a/b/c.git/info/refs?service=git-upload-pack", units: 5},
		{name: "git/repository not permitted", method: http.MethodGet, target: "https://github.com/other/repo.git/info/refs?service=git-upload-pack", units: 5},
		{name: "git/discovery service refused", method: http.MethodGet, target: git + "/info/refs?service=other", units: 5},
		{name: "git/upload-pack with a query refused", method: http.MethodPost, target: git + "/git-upload-pack?x=1", body: "0000", units: 5},
		{name: "git/token unavailable", method: http.MethodGet, target: git + "/info/refs?service=git-upload-pack", tokenErr: true, units: 5},
		{name: "git/budget exhausted", method: http.MethodGet, target: git + "/info/refs?service=git-upload-pack", units: 1, spent: 1},
		{name: "git/ledger unavailable", method: http.MethodGet, target: git + "/info/refs?service=git-upload-pack", units: 1, failingLedger: true},
		{name: "git/upstream transport error", method: http.MethodGet, target: git + "/info/refs?service=git-upload-pack",
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamGit: failed}, units: 5},
		{name: "git/upstream non-2xx", method: http.MethodGet, target: git + "/info/refs?service=git-upload-pack",
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamGit: status(404, `{"message":"Not Found"}`)}, units: 5},
		{name: "rest/operation not permitted", method: http.MethodGet, target: "https://api.github.com/repos/andrewmccall/home-ops", units: 5},
		{name: "rest/pull request served", method: http.MethodPost, target: pulls, body: restCreate, units: 5},
		{name: "rest/pull request head outside the run", method: http.MethodPost, target: pulls,
			body: `{"head":"feature","base":"main"}`, units: 5},
		{name: "rest/pull request query refused", method: http.MethodPost, target: pulls + "?head=x", body: restCreate, units: 5},
		{name: "rest/pull request repository not permitted", method: http.MethodPost,
			target: "https://api.github.com/repos/other/repo/pulls", body: restCreate, units: 5},
		{name: "rest/budget exhausted at create", method: http.MethodPost, target: pulls, body: restCreate, units: 1, spent: 1},
		{name: "rest/budget exhausted at read-back", method: http.MethodPost, target: pulls, body: restCreate, units: 1},
		{name: "rest/ledger unavailable", method: http.MethodPost, target: pulls, body: restCreate, units: 1, failingLedger: true},
		{name: "rest/upstream transport error", method: http.MethodPost, target: pulls, body: restCreate,
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamRESTCreate: failed}, units: 5},
		{name: "rest/upstream non-2xx at create", method: http.MethodPost, target: pulls, body: restCreate,
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamRESTCreate: status(422, `{"message":"Validation Failed"}`)}, units: 5},
		{name: "rest/read-back failed", method: http.MethodPost, target: pulls, body: restCreate,
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamRESTReadback: status(404, `{"message":"Not Found"}`)}, units: 5},
		{name: "graphql/read served", method: http.MethodPost, target: graphqlURL,
			body: graphql(readDocument, readVariables("andrewmccall", "home-ops")), units: 5},
		{name: "graphql/read repository not permitted", method: http.MethodPost, target: graphqlURL,
			body: graphql(readDocument, readVariables("other", "repo")), units: 5},
		{name: "graphql/unknown operation", method: http.MethodPost, target: graphqlURL,
			body: graphql(`query Viewer { viewer { login } }`, map[string]any{}), units: 5},
		{name: "graphql/read upstream transport error", method: http.MethodPost, target: graphqlURL,
			body:    graphql(readDocument, readVariables("andrewmccall", "home-ops")),
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamGraphQLRead: failed}, units: 5},
		{name: "graphql/read budget exhausted", method: http.MethodPost, target: graphqlURL,
			body: graphql(readDocument, readVariables("andrewmccall", "home-ops")), units: 1, spent: 1},
		{name: "graphql/create served", method: http.MethodPost, target: graphqlURL, body: graphql(createDocument, createInput), units: 5},
		{name: "graphql/create budget exhausted at resolve", method: http.MethodPost, target: graphqlURL,
			body: graphql(createDocument, createInput), units: 1, spent: 1},
		{name: "graphql/create budget exhausted at create", method: http.MethodPost, target: graphqlURL,
			body: graphql(createDocument, createInput), units: 1},
		{name: "graphql/create budget exhausted at read-back", method: http.MethodPost, target: graphqlURL,
			body: graphql(createDocument, createInput), units: 2},
		{name: "graphql/create upstream non-2xx at create", method: http.MethodPost, target: graphqlURL,
			body:    graphql(createDocument, createInput),
			replies: map[string]func(*http.Request) (*http.Response, error){upstreamGraphQLCreate: status(502, `{"message":"Bad Gateway"}`)}, units: 5},
	}
	lines := make([]string, 0, len(cases))
	for _, tc := range cases {
		events := &recordedEvents{}
		tracker := budget.NewTracker()
		if tc.failingLedger {
			tracker = budget.NewTrackerWithLedger(failingLedger{})
		}
		if tc.spent > 0 {
			if err := tracker.Meter(key, api.EndpointBudget{MaxUnits: tc.spent}).Spend(context.Background(), tc.spent, 0); err != nil {
				t.Fatal(err)
			}
		}
		tokens := &githubgateway.FakeTokenProvider{GitHubToken: "ghs_recorded"}
		if tc.tokenErr {
			tokens.Err = errors.New("token unavailable")
		}
		handler := &githubgateway.Handler{Config: githubgateway.HandlerConfig{TokenProvider: tokens, AuditLogger: events,
			GitBaseURL: "https://git.test", APIBaseURL: "https://api.test", HTTPClient: recordedGitHubUpstream(tc.replies)}}
		route := gateway.Route{Capability: capability, Tier: audit.TierSemantic, Handler: handler,
			Budgets: tracker, AuditLogger: events}
		routes := gateway.Dispatcher{githubgateway.GitAuthority: route, githubgateway.APIAuthority: route}
		before := ledgerReading(tracker, key)
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, tc.target, body)
		req.Header.Set("Authorization", "Bearer projected-run-token")
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), grant(tc.units))))
		lines = append(lines, outcomeLine(t, tc.name, response, before+" -> "+ledgerReading(tracker, key), events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}
