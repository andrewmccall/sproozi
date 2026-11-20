package docker

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

var updateRecords = flag.Bool("update-records", false,
	"rewrite testdata/routes.txt from the current code")

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
			t.Errorf("route changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

// routeLines describes every route and path route by authority: capability,
// tier, handler type, whether it forwards the workload's provider credential
// and whether budgets and audit are attached.
func routeLines(prefix string, routes gateway.Dispatcher) []string {
	describe := func(name string, r gateway.Route) string {
		handler := "none"
		if r.Handler != nil {
			handler = fmt.Sprintf("%T", r.Handler)
		}
		capability := string(r.Capability)
		if capability == "" {
			capability = "-"
		}
		return strings.Join([]string{prefix, name, capability, string(r.Tier), handler,
			fmt.Sprintf("providerCredential=%t", r.ProviderCredential),
			fmt.Sprintf("budgets=%t", r.Budgets != nil), fmt.Sprintf("audit=%t", r.AuditLogger != nil)}, " | ")
	}
	var lines []string
	for _, authority := range slices.Sorted(maps.Keys(routes)) {
		route := routes[authority]
		lines = append(lines, describe(authority, route))
		for _, p := range route.Paths {
			lines = append(lines, describe(authority+p.Prefix, p.Route))
		}
	}
	return lines
}

type fixtureTokens struct{}

func (fixtureTokens) Token(context.Context) (string, error) { return "", errors.New("not served") }

// recordedRoutes composes the routes NewGateway serves, with the GitHub and
// Kubernetes capabilities configured.
func recordedRoutes(kind harness.NativeKind, auth modelauth.Authenticator) (gateway.Dispatcher, error) {
	upstream, _ := url.Parse("https://kubernetes.fixture")
	owner := &Owner{policy: gateway.PolicySnapshot{Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{
		api.CapabilityModelInference, api.CapabilityGitHubPullRequest, api.CapabilityKubernetesRead}}},
		nativeCapabilities: []api.CapabilityKind{api.CapabilityGitHubPullRequest, api.CapabilityKubernetesRead}}
	config := GatewayConfig{Harness: kind, AuditLogger: audit.NewLogger(io.Discard),
		Certificates: map[string]tls.Certificate{"gateway.fixture": {}}, MCPAuthority: "gateway.fixture:443",
		GitHub: &githubgateway.HandlerConfig{TokenProvider: fixtureTokens{}, HTTPClient: http.DefaultClient,
			GitBaseURL: "https://github.com", APIBaseURL: "https://api.github.com"},
		Kubernetes: &kubernetesgateway.HandlerConfig{Upstream: upstream, Client: http.DefaultClient},
		Model:      modelgateway.HandlerConfig{UpstreamURL: "https://api.openai.com", ProviderAuth: auth, HTTPClient: http.DefaultClient}}
	return composeRoutes(owner, config, budget.NewTracker(), diagnose.NewJournal(config.AuditLogger, diagnose.Limits{}))
}

// TestDockerRoutesAreRecorded records the Docker composition's route table
// for every managed harness and credential mechanism.
func TestDockerRoutesAreRecorded(t *testing.T) {
	var lines []string
	for _, kind := range harness.ManagedKinds() {
		for _, mechanism := range []struct {
			name string
			auth modelauth.Authenticator
		}{
			{"key", modelauth.APIKey{Key: "sk-fixture"}},
			{"token", modelauth.Token{Value: "fixture-token"}},
			{"chatgpt", &modelauth.ChatGPT{}},
			{"sign-in", modelauth.Workload{}},
		} {
			prefix := string(kind) + "/" + mechanism.name
			routes, err := recordedRoutes(kind, mechanism.auth)
			if err != nil {
				lines = append(lines, prefix+" | error: "+err.Error())
				continue
			}
			lines = append(lines, routeLines(prefix, routes)...)
		}
	}
	compareRecorded(t, filepath.Join("testdata", "routes.txt"), lines)
}
