package destination_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	egress "github.com/andrewmccall/sproozi/internal/endpoints/destination"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	testPackageURL      = "https://registry.example:8443/pkg"
	testRegistryProfile = "registry"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func makeIdentity(selected, allowed []string) *gateway.RunIdentity {
	return &gateway.RunIdentity{
		Run:      &api.AgentRun{Spec: api.AgentRunSpec{Capabilities: []api.CapabilityKind{api.CapabilityNetworkEgress}}},
		Policy:   &api.AgentPolicy{Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{api.CapabilityNetworkEgress}, EgressProfiles: allowed}},
		Template: &api.AgentTemplate{Spec: api.AgentTemplateSpec{EgressProfiles: selected}},
	}
}
func TestInspectedEgressEnforcesDestinationAndLiveGrant(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		status       int
		mutate       func(*gateway.RunIdentity)
	}{
		{"approved", testPackageURL, 200, nil},
		{"other host", "https://evil.example:8443/pkg", 403, nil},
		{"subdomain", "https://evil.registry.example:8443/pkg", 403, nil},
		{"wrong port", "https://registry.example/pkg", 403, nil},
		{"IP literal", "https://127.0.0.1:8443/pkg", 400, nil},
		{"trailing dot", "https://registry.example.:8443/pkg", 400, nil},
		{"plain HTTP", "http://registry.example:8443/pkg", 403, nil},
		{"unrequested", testPackageURL, 403, func(i *gateway.RunIdentity) { i.Run.Spec.Capabilities = nil }},
		{"revoked capability", testPackageURL, 403, func(i *gateway.RunIdentity) { i.Policy.Spec.AllowedCapabilities = nil }},
		{"unselected profile", testPackageURL, 403, func(i *gateway.RunIdentity) { i.Template.Spec.EgressProfiles = nil }},
		{"revoked profile", testPackageURL, 403, func(i *gateway.RunIdentity) { i.Policy.Spec.EgressProfiles = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles, err := egress.LoadProfileStore([]byte(`{"registry":[{"scheme":"https","host":"registry.example","port":8443}]}`))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			var events strings.Builder
			h := &egress.Handler{Config: egress.HandlerConfig{Profiles: profiles, AuditLogger: audit.NewLogger(&events), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
					t.Error("forwarded credentials")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("package"))}, nil
			})}}}
			id := makeIdentity([]string{testRegistryProfile}, []string{testRegistryProfile})
			if tc.mutate != nil {
				tc.mutate(id)
			}
			req := httptest.NewRequest("GET", tc.target, nil)
			req.Header.Set("Authorization", "secret")
			req.Header.Set("Proxy-Authorization", "secret")
			req = req.WithContext(gateway.WithIdentity(req.Context(), id))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, tc.status, rr.Body)
			}
			if tc.status != 200 && calls != 0 {
				t.Fatal("denied request reached upstream")
			}
			if !strings.Contains(events.String(), `"semanticLevel":"destination"`) {
				t.Fatal("missing destination tier audit")
			}
		})
	}
}
func TestEgressRedirectRequiresNewAuthorization(t *testing.T) {
	ps, _ := egress.LoadProfileStore([]byte(`{"registry":[{"scheme":"https","host":"registry.example","port":443}]}`))
	calls := 0
	h := &egress.Handler{Config: egress.HandlerConfig{Profiles: ps, AuditLogger: audit.NewLogger(io.Discard), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.example"}}, Body: http.NoBody}, nil
	})}}}
	req := httptest.NewRequest("GET", "https://registry.example/pkg", nil)
	req = req.WithContext(gateway.WithIdentity(req.Context(), makeIdentity([]string{testRegistryProfile}, []string{testRegistryProfile})))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if calls != 1 || rr.Code != 302 {
		t.Fatalf("redirect followed: calls=%d status=%d", calls, rr.Code)
	}
}
func TestEgressDeniesRawTunnelAndMissingIdentity(t *testing.T) {
	ps, _ := egress.LoadProfileStore([]byte(`{}`))
	h := &egress.Handler{Config: egress.HandlerConfig{Profiles: ps, AuditLogger: audit.NewLogger(io.Discard)}}
	req := httptest.NewRequest("CONNECT", "https://registry.example", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatal("missing identity accepted")
	}
	req = req.WithContext(gateway.WithIdentity(req.Context(), makeIdentity(nil, nil)))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatal("raw tunnel accepted")
	}
}
