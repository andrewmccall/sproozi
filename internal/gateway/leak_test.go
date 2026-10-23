package gateway_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	providerAuthority = "provider.test:443"
	otherAuthority    = "other.test:443"
	leakedCredential  = "sk-ant-oat01-workload-own-credential"
)

func leakGrant(uid string) *gateway.ExecutionGrant {
	return &gateway.ExecutionGrant{Policy: gateway.PolicySnapshot{Name: fixturePolicyName},
		Execution: gateway.Reference{Backend: gateway.BackendKubernetes, UID: uid},
		Template:  gateway.TemplateSnapshot{Name: fixtureTemplateName}}
}

// leakRoutes serves a provider route that carries the workload's own
// credential and another route that records what reached it.
func leakRoutes(received *bytes.Buffer, readErr *error) gateway.Dispatcher {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return gateway.Dispatcher{
		providerAuthority: {Handler: ok, ProviderCredential: true},
		otherAuthority: {Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, *readErr = io.Copy(received, r.Body)
			w.WriteHeader(http.StatusNoContent)
		})},
	}
}

func leakRequest(uid, url string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, url, body)
	return r.WithContext(gateway.WithIdentity(r.Context(), leakGrant(uid)))
}

func observe(t *testing.T, guard *gateway.LeakGuard, uid string) {
	t.Helper()
	r := leakRequest(uid, "https://provider.test/v1/messages", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+leakedCredential)
	response := httptest.NewRecorder()
	guard.ServeHTTP(response, r)
	if response.Code != http.StatusNoContent {
		t.Fatalf("provider route with the workload credential: status=%d", response.Code)
	}
}

func TestLeakGuardRefusesTheRunsCredentialOnOtherRoutes(t *testing.T) {
	for name, place := range map[string]func(*http.Request){
		"header": func(r *http.Request) { r.Header.Set("X-Note", "token="+leakedCredential) },
		"query":  func(r *http.Request) { r.URL.RawQuery = "q=" + leakedCredential },
	} {
		t.Run(name, func(t *testing.T) {
			var received bytes.Buffer
			var readErr error
			guard := gateway.NewLeakGuard(leakRoutes(&received, &readErr), nil)
			observe(t, guard, fixtureExecutionUID)
			r := leakRequest(fixtureExecutionUID, "https://other.test/issues", strings.NewReader("plain"))
			place(r)
			response := httptest.NewRecorder()
			guard.ServeHTTP(response, r)
			assertDenial(t, response, http.StatusForbidden, "permission_error", "credential_leak_denied", "provider credential")
			if received.Len() != 0 {
				t.Fatalf("refused request reached the route: %q", received.String())
			}
		})
	}
}

func TestLeakGuardNeverForwardsACompleteCredentialInABody(t *testing.T) {
	var received bytes.Buffer
	var readErr error
	guard := gateway.NewLeakGuard(leakRoutes(&received, &readErr), nil)
	observe(t, guard, fixtureExecutionUID)
	// A slow body splits the credential across reads.
	body := io.MultiReader(strings.NewReader(strings.Repeat("a", 40000)+"PR body: "+leakedCredential[:10]),
		strings.NewReader(leakedCredential[10:]+" trailing"))
	guard.ServeHTTP(httptest.NewRecorder(), leakRequest(fixtureExecutionUID, "https://other.test/pulls", body))
	if !errors.Is(readErr, gateway.ErrCredentialLeak) {
		t.Fatalf("body read error = %v, want ErrCredentialLeak", readErr)
	}
	if strings.Contains(received.String(), leakedCredential) {
		t.Fatal("the complete credential reached the route")
	}
}

func TestLeakGuardPassesOrdinaryTrafficAndOtherRuns(t *testing.T) {
	var received bytes.Buffer
	var readErr error
	guard := gateway.NewLeakGuard(leakRoutes(&received, &readErr), nil)
	observe(t, guard, fixtureExecutionUID)
	payload := strings.Repeat("ordinary body ", 5000)
	response := httptest.NewRecorder()
	guard.ServeHTTP(response, leakRequest(fixtureExecutionUID, "https://other.test/pulls", strings.NewReader(payload)))
	if response.Code != http.StatusNoContent || readErr != nil || received.String() != payload {
		t.Fatalf("ordinary body: status=%d err=%v received %d of %d bytes", response.Code, readErr, received.Len(), len(payload))
	}
	// Another run never sent this credential, so it isn't watched for it.
	received.Reset()
	response = httptest.NewRecorder()
	guard.ServeHTTP(response, leakRequest("other-run", "https://other.test/pulls", strings.NewReader(leakedCredential)))
	if response.Code != http.StatusNoContent || received.String() != leakedCredential {
		t.Fatalf("other run: status=%d received=%q", response.Code, received.String())
	}
}

func TestLeakGuardForgetsCredentialsAfterTheirLifetime(t *testing.T) {
	var received bytes.Buffer
	var readErr error
	now := time.Unix(1_800_000_000, 0)
	guard := gateway.NewLeakGuard(leakRoutes(&received, &readErr), func() time.Time { return now })
	observe(t, guard, fixtureExecutionUID)
	if guard.Watched(fixtureExecutionUID) != 1 {
		t.Fatal("the provider credential was not watched")
	}
	now = now.Add(gateway.DefaultCredentialLifetime + time.Second)
	observe(t, guard, "later-run")
	if guard.Watched(fixtureExecutionUID) != 0 {
		t.Fatal("an expired run's credential is still held")
	}
}

func TestLeakGuardIgnoresShortCredentials(t *testing.T) {
	var received bytes.Buffer
	var readErr error
	guard := gateway.NewLeakGuard(leakRoutes(&received, &readErr), nil)
	r := leakRequest(fixtureExecutionUID, "https://provider.test/v1/messages", strings.NewReader("{}"))
	r.Header.Set("X-Api-Key", "short")
	guard.ServeHTTP(httptest.NewRecorder(), r)
	if guard.Watched(fixtureExecutionUID) != 0 {
		t.Fatal("a short value would refuse ordinary text")
	}
}

// A path route keeps its own credential: Copilot's GitHub token on
// /copilot_internal/ is refused on the same host's other paths, such as a pull
// request body on api.github.com.
func TestLeakGuardScopesACredentialToItsPathRoute(t *testing.T) {
	var received bytes.Buffer
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(&received, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	routes := gateway.Dispatcher{"api.github.test:443": {Handler: ok, Paths: []gateway.PathRoute{
		{Prefix: "/copilot_internal/", Route: gateway.Route{Handler: ok, ProviderCredential: true}}}}}
	guard := gateway.NewLeakGuard(routes, nil)
	r := leakRequest(fixtureExecutionUID, "https://api.github.test/copilot_internal/user", nil)
	r.Header.Set("Authorization", "Bearer "+leakedCredential)
	guard.ServeHTTP(httptest.NewRecorder(), r)
	if !guard.ForwardsCredential("api.github.test:443", "/copilot_internal/user") ||
		guard.ForwardsCredential("api.github.test:443", "/repos/o/r/pulls") ||
		guard.ForwardsCredential("api.github.test:443", "/copilot_internal/../repos/o/r/pulls") {
		t.Fatal("credential forwarding isn't scoped to the clean path route")
	}
	response := httptest.NewRecorder()
	guard.ServeHTTP(response, leakRequest(fixtureExecutionUID, "https://api.github.test/repos/o/r/pulls",
		strings.NewReader(`{"body":"`+leakedCredential+`"}`)))
	if strings.Contains(received.String(), leakedCredential) {
		t.Fatal("the path route's credential reached the host's other route")
	}
}
