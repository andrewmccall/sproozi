package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func compositionIdentity(cap sprooziv1alpha1.CapabilityKind) *gateway.RunIdentity {
	return &gateway.RunIdentity{
		Run:    &sprooziv1alpha1.AgentRun{Spec: sprooziv1alpha1.AgentRunSpec{Capabilities: []sprooziv1alpha1.CapabilityKind{cap}}},
		Policy: &sprooziv1alpha1.AgentPolicy{Spec: sprooziv1alpha1.AgentPolicySpec{AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{cap}, Budgets: map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 100}}, KubernetesRead: sprooziv1alpha1.KubernetesReadScope{Namespaces: []string{"demo"}, Resources: []sprooziv1alpha1.KubernetesReadResource{sprooziv1alpha1.KubernetesReadPods}}}},
	}
}

func TestDispatchCompositionReachesKubernetesUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream, _ := url.Parse("https://kubernetes.test")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"kind":"PodList"}`)), Request: r}, nil
	})}
	h := kubernetesgateway.NewHandler(kubernetesgateway.HandlerConfig{Upstream: upstream, Client: client})
	id := compositionIdentity(sprooziv1alpha1.CapabilityKubernetesRead)
	req := httptest.NewRequest(http.MethodGet, "https://kubernetes.default.svc:443/api/v1/namespaces/demo/pods", nil)
	req.Header.Set("Proxy-Authorization", "Bearer run-token")
	response := httptest.NewRecorder()
	gateway.Dispatcher{kubernetesgateway.Authority: gateway.Route{Handler: h, Capability: string(sprooziv1alpha1.CapabilityKubernetesRead), Budgets: budget.NewTracker()}}.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), id)))
	if response.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("response=%v calls=%d", response, calls.Load())
	}
}

func TestDispatchCompositionReachesModelUpstream(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"usage":{"total_tokens":1}}`)), Request: r}, nil
	})}
	h := &modelgateway.Handler{Config: modelgateway.HandlerConfig{AuditLogger: audit.NewLogger(io.Discard), UpstreamURL: "https://model.test", ProviderAuth: modelauth.APIKey{Key: "provider"}, HTTPClient: client}}
	id := compositionIdentity(sprooziv1alpha1.CapabilityModelInference)
	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com:443/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`))
	req.Header.Set("Proxy-Authorization", "Bearer run-token")
	response := httptest.NewRecorder()
	gateway.Dispatcher{"api.openai.com:443": gateway.Route{Handler: h, Capability: string(sprooziv1alpha1.CapabilityModelInference), Budgets: budget.NewTracker()}}.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), id)))
	if response.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("response=%v calls=%d", response, calls.Load())
	}
}

func TestEndpointBudgetStopsUpstreamAndKeepsCapabilitiesSeparate(t *testing.T) {
	var calls atomic.Int32
	upstream := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	target, _ := url.Parse("https://cluster.test")
	handler := kubernetesgateway.NewHandler(kubernetesgateway.HandlerConfig{Upstream: target, Client: upstream, AuditLogger: audit.NewLogger(io.Discard)})
	tracker := budget.NewTracker()
	id := compositionIdentity(sprooziv1alpha1.CapabilityKubernetesRead)
	id.Policy.Spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
		sprooziv1alpha1.CapabilityKubernetesRead: {MaxUnits: 1},
	}
	d := gateway.Dispatcher{kubernetesgateway.Authority: {Handler: handler, Capability: string(sprooziv1alpha1.CapabilityKubernetesRead), Budgets: tracker}}
	request := func() int {
		req := httptest.NewRequest(http.MethodGet, "https://kubernetes.default.svc/api/v1/namespaces/demo/pods", nil)
		response := httptest.NewRecorder()
		d.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), id)))
		return response.Code
	}
	if got := request(); got != http.StatusOK {
		t.Fatalf("first request: %d", got)
	}
	if got := request(); got == http.StatusOK || calls.Load() != 1 {
		t.Fatalf("exhausted budget forwarded: status=%d calls=%d", got, calls.Load())
	}
	// A separate capability does not inherit this endpoint's consumption.
	meter := tracker.Meter(string(id.Run.UID)+"/model.inference", sprooziv1alpha1.EndpointBudget{MaxUnits: 1})
	if err := meter.Spend(1, 0); err != nil {
		t.Fatal(err)
	}
	// Removing a limit means no consumption ceiling, while authorization remains.
	delete(id.Policy.Spec.Budgets, sprooziv1alpha1.CapabilityKubernetesRead)
	if got := request(); got != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("optional budget: status=%d calls=%d", got, calls.Load())
	}
}
