package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	fixturePolicyName   = "test-policy"
	fixtureTemplateName = "test-template"
	fixtureExecutionUID = "test-execution"
	fixtureAuthority    = "service.test:443"
)

func TestDispatcherRequiresContextIdentity(t *testing.T) {
	called := false
	d := gateway.Dispatcher{fixtureAuthority: {Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})}}
	req := httptest.NewRequest("GET", "https://service.test/path", nil)
	denied := httptest.NewRecorder()
	d.ServeHTTP(denied, req)
	if called || denied.Code != http.StatusUnauthorized {
		t.Fatal("accepted request without context identity")
	}
	req = req.WithContext(gateway.WithIdentity(req.Context(), &gateway.ExecutionGrant{Policy: gateway.PolicySnapshot{Name: fixturePolicyName},
		Execution: gateway.Reference{Backend: gateway.BackendKubernetes, UID: fixtureExecutionUID},
		Template:  gateway.TemplateSnapshot{Name: fixtureTemplateName}}))
	allowed := httptest.NewRecorder()
	d.ServeHTTP(allowed, req)
	if !called || allowed.Code != http.StatusNoContent {
		t.Fatal("authenticated request not dispatched")
	}
}

func TestDispatcherEnforcesRouteCapabilityAndAuditsRouteTier(t *testing.T) {
	const capability = api.CapabilityKubernetesRead
	for _, tc := range []struct {
		name               string
		execution, policy  []api.CapabilityKind
		wantStatus         int
		wantHandler        bool
		wantAuditedAllowed bool
	}{
		{"granted", []api.CapabilityKind{capability}, []api.CapabilityKind{capability}, http.StatusNoContent, true, true},
		{"not requested by execution", nil, []api.CapabilityKind{capability}, http.StatusForbidden, false, false},
		{"not allowed by policy", []api.CapabilityKind{capability}, nil, http.StatusForbidden, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events bytes.Buffer
			logger := audit.NewLogger(&events)
			called := false
			d := gateway.Dispatcher{fixtureAuthority: {Capability: string(capability), Tier: audit.TierSemantic,
				AuditLogger: logger, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					event := gateway.AuditEvent(r.Context(), "read")
					event.Allowed = true
					logger.Log(event)
					w.WriteHeader(http.StatusNoContent)
				})}}
			grant := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendDocker, UID: fixtureExecutionUID},
				Policy:       gateway.PolicySnapshot{Name: fixturePolicyName, Spec: api.AgentPolicySpec{AllowedCapabilities: tc.policy}},
				Template:     gateway.TemplateSnapshot{Name: fixtureTemplateName},
				Capabilities: tc.execution}
			req := httptest.NewRequest(http.MethodGet, "https://service.test/path", nil)
			response := httptest.NewRecorder()
			d.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), grant)))
			if response.Code != tc.wantStatus || called != tc.wantHandler {
				t.Fatalf("status=%d handler=%v", response.Code, called)
			}
			var event audit.Event
			if err := json.Unmarshal(events.Bytes(), &event); err != nil {
				t.Fatalf("audit event: %v", err)
			}
			if event.Allowed != tc.wantAuditedAllowed || event.Capability != string(capability) ||
				event.SemanticLevel != audit.TierSemantic || event.RunUID != fixtureExecutionUID {
				t.Fatalf("audit event did not come from the route: %+v", event)
			}
		})
	}
}

func TestDispatcherDeliversFirstChunkBeforeHandlerCompletes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	d := gateway.Dispatcher{fixtureAuthority: {Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
	})}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "service.test"
		d.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), &gateway.ExecutionGrant{Policy: gateway.PolicySnapshot{Name: fixturePolicyName},
			Execution: gateway.Reference{Backend: gateway.BackendKubernetes, UID: fixtureExecutionUID},
			Template:  gateway.TemplateSnapshot{Name: fixtureTemplateName}})))
	}))
	defer server.Close()
	got := make(chan error, 1)
	go func() {
		resp, err := http.Get(server.URL)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			b := make([]byte, 6)
			_, err = io.ReadFull(resp.Body, b)
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher buffered streaming response")
	}
	// Release before httptest waits for the active handler.
	release <- struct{}{}
}
