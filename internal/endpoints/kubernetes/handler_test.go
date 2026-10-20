package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	podsResource = "pods"
)

const (
	demoPodsPath    = "/api/v1/namespaces/demo/pods"
	demoSecretsPath = "/api/v1/namespaces/demo/secrets"
)

func identity() *gateway.ExecutionGrant {
	return &gateway.ExecutionGrant{Policy: gateway.PolicySnapshot{Spec: sprooziv1alpha1.AgentPolicySpec{
		AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
		KubernetesRead:      sprooziv1alpha1.KubernetesReadScope{Namespaces: []string{"demo"}, Resources: []sprooziv1alpha1.KubernetesReadResource{sprooziv1alpha1.KubernetesReadPods, sprooziv1alpha1.KubernetesReadPodLogs}},
	}, Name: "test-policy"},
		Execution:    gateway.Reference{Backend: gateway.BackendKubernetes, UID: "test-execution"},
		Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
		Template:     gateway.TemplateSnapshot{Name: "test-template"}}
}

func TestParseOperationTable(t *testing.T) {
	tests := []struct {
		path                  string
		ok                    bool
		resource, subresource string
		discovery             bool
		watch                 bool
	}{
		{coreDiscoveryPath, true, "", "", true, false},
		{"/api?timeout=32s", true, "", "", true, false},
		{"/apis?timeout=32s", true, "", "", true, false},
		{"/api/v1?timeout=32s", true, "", "", true, false},
		{"/apis/apps/v1?timeout=32s", true, "", "", true, false},
		{"/api?timeout=301s", false, "", "", false, false},
		{"/api?timeout=0s", false, "", "", false, false},
		{"/api?timeout=bad", false, "", "", false, false},
		{"/api?timeout=32s&timeout=1s", false, "", "", false, false},
		{"/api?timeout=32s&foo=bar", false, "", "", false, false},
		{coreV1DiscoveryPath, true, "", "", true, false},
		{groupsDiscoveryPath, true, "", "", true, false},
		{"/apis/apps", true, "", "", true, false},
		{appsV1DiscoveryPath, true, "", "", true, false},
		{"/apis/batch/v1", false, "", "", false, false},
		{demoPodsPath, true, podsResource, "", false, false},
		{"/api/v1/namespaces/demo/pods/p1", true, podsResource, "", false, false},
		{"/api/v1/namespaces/demo/pods/p1/log?tailLines=2", true, podsResource, "log", false, false},
		{"/api/v1/namespaces/demo/pods/p1/log?watch=true&timeoutSeconds=30", false, "", "", false, false},
		{"/apis/apps/v1/namespaces/demo/deployments", true, "deployments", "", false, false},
		{demoSecretsPath, true, "secrets", "", false, false},
		{"/api/v1/namespaces/demo/pods?watch=true&timeoutSeconds=301", false, "", "", false, false},
		{"/api/v1/namespaces/demo/pods?watch=maybe", false, "", "", false, false},
		{"/api/v1/namespaces/demo/pods?foo=bar", false, "", "", false, false},
		{"/api/v1/namespaces/demo/pods/p1/exec", true, podsResource, "exec", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			op, err := ParseOperation(req)
			if (err == nil) != tt.ok {
				t.Fatalf("error=%v", err)
			}
			if err == nil && (op.Resource != tt.resource || op.Subresource != tt.subresource || op.Discovery != tt.discovery || op.Watch != tt.watch) {
				t.Fatalf("operation=%+v", op)
			}
		})
	}
}

func TestAllowedDiscoveryIsBoundedToGrantedCoreAndAppsAPIs(t *testing.T) {
	calls := 0
	u, _ := url.Parse("https://kubernetes.test")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(), nil })}
	h := &Handler{Config: HandlerConfig{Upstream: u, Client: client}}
	for _, path := range []string{coreDiscoveryPath, coreV1DiscoveryPath, groupsDiscoveryPath, appsV1DiscoveryPath, "/api?timeout=32s", "/apis?timeout=32s"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, err := h.authorizeAndForward(context.Background(), identity(), req); err != nil {
			t.Errorf("%s was denied: %v", path, err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/apis/batch/v1", nil)
	if _, err := h.authorizeAndForward(context.Background(), identity(), req); err == nil {
		t.Fatal("ungranted API discovery was allowed")
	}
	if calls != 6 {
		t.Fatalf("upstream calls=%d, want 6", calls)
	}
}

func TestDeniedOperationsNeverCallUpstream(t *testing.T) {
	calls := 0
	u, _ := url.Parse("https://kubernetes.test")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(), nil })}
	h := &Handler{Config: HandlerConfig{Upstream: u, Client: client}}
	for _, path := range []string{
		demoSecretsPath, "/api/v1/namespaces/other/pods", "/api/v1/namespaces/demo/pods/p1/exec",
		"/api/v1/namespaces/demo/pods?watch=true&timeoutSeconds=301", "/api/v1/namespaces/demo/pods?foo=bar",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, err := h.authorizeAndForward(context.Background(), identity(), req); err == nil {
			t.Errorf("%s was allowed", path)
		}
	}
	if calls != 0 {
		t.Fatalf("upstream calls=%d, want 0", calls)
	}
}

// A request outside a bound the workload controls says which bound; a policy
// denial says only that the request was denied.
func TestDenialBodyNamesWorkloadFixableBounds(t *testing.T) {
	u, _ := url.Parse("https://kubernetes.test")
	h := &Handler{Config: HandlerConfig{Upstream: u, Client: &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) { return response(), nil })}}}
	for path, want := range map[string]string{
		"/api/v1/namespaces/demo/pods?watch=1&timeoutSeconds=301": "kubernetes request denied: timeoutSeconds must be an integer from 1 to 300",
		"/api/v1/namespaces/demo/pods?limit=5001":                 "kubernetes request denied: limit must be an integer from 1 to 5000",
		demoSecretsPath: "kubernetes request denied",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req.WithContext(gateway.WithIdentity(req.Context(), identity())))
		if recorder.Code != http.StatusForbidden || strings.TrimSpace(recorder.Body.String()) != want {
			t.Errorf("%s = %d %q, want 403 %q", path, recorder.Code, recorder.Body.String(), want)
		}
	}
}

func TestKubernetesAuditRecordsAllowedReadAndPolicyDenial(t *testing.T) {
	var output bytes.Buffer
	u, _ := url.Parse("https://kubernetes.test")
	h := NewHandler(HandlerConfig{Upstream: u, AuditLogger: audit.NewLogger(&output), Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(), nil })}})
	routes := gateway.Dispatcher{Authority: {Capability: string(sprooziv1alpha1.CapabilityKubernetesRead), Tier: audit.TierSemantic, Handler: h}}
	for _, path := range []string{demoPodsPath, demoSecretsPath} {
		req := httptest.NewRequest(http.MethodGet, "https://"+Authority+path, nil)
		routes.ServeHTTP(httptest.NewRecorder(), req.WithContext(gateway.WithIdentity(req.Context(), identity())))
	}
	decoder := json.NewDecoder(&output)
	var allowed, denied audit.Event
	if err := decoder.Decode(&allowed); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&denied); err != nil {
		t.Fatal(err)
	}
	if !allowed.Allowed || allowed.UpstreamStatus != 200 || allowed.Resource != demoPodsPath || allowed.Capability != "kubernetes.read" || allowed.SemanticLevel != "semantic" {
		t.Fatalf("allowed=%+v", allowed)
	}
	if denied.Allowed || denied.UpstreamStatus != 0 || denied.Resource != demoSecretsPath || denied.DenyReason != "operation not permitted by policy" {
		t.Fatalf("denied=%+v", denied)
	}
}

func TestAllowedOperationForwardsWithoutWorkloadCredentials(t *testing.T) {
	called := make(chan *http.Request, 1)
	u, _ := url.Parse("https://kubernetes.test")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { called <- r; return response(), nil })}
	h := &Handler{Config: HandlerConfig{Upstream: u, Client: client}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/demo/pods?limit=10", nil)
	req.Header.Set("Proxy-Authorization", "Bearer gateway-token")
	req.Header.Set("Authorization", "Bearer old-token")
	if _, err := h.authorizeAndForward(context.Background(), identity(), req); err != nil {
		t.Fatal(err)
	}
	r := <-called
	if r.URL.Path != demoPodsPath || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
		t.Fatalf("forwarded path/auth=%s/%q/%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Proxy-Authorization"))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}
}
