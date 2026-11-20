package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// LeakGuard's record of a refused credential names the route and never the
// credential, the URL or header it was found in, or anything that contains
// it. What the denial journal keeps for diagnose is that record.
func TestLeakGuardRecordNeverContainsTheCredential(t *testing.T) {
	const credential = "sk-ant-oat01-never-in-a-record-0123456789"
	for name, place := range map[string]func(*http.Request){
		"header":        func(r *http.Request) { r.Header.Set("X-Note", "token="+credential) },
		"authorization": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+credential) },
		"query":         func(r *http.Request) { r.URL.RawQuery = "q=" + credential },
		"path":          func(r *http.Request) { r.URL.Path = "/upload/" + credential },
	} {
		t.Run(name, func(t *testing.T) {
			stdout := &recordedEvents{}
			journal := diagnose.NewJournal(stdout, diagnose.Limits{})
			ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
			guard := gateway.NewLeakGuard(gateway.Dispatcher{
				providerAuthority: {Capability: api.CapabilityModelInference, Handler: ok, ProviderCredential: true, AuditLogger: journal},
				otherAuthority:    {Capability: api.CapabilityNetworkEgress, Handler: ok, AuditLogger: journal},
			}, nil)
			grant := leakGrant(fixtureExecutionUID)
			grant.Capabilities = []api.CapabilityKind{api.CapabilityModelInference, api.CapabilityNetworkEgress}
			grant.Policy.Spec.AllowedCapabilities = grant.Capabilities
			observe := leakRequest(fixtureExecutionUID, "https://provider.test/v1/messages", strings.NewReader("{}"))
			observe.Header.Set("Authorization", "Bearer "+credential)
			guard.ServeHTTP(httptest.NewRecorder(), observe.WithContext(gateway.WithIdentity(observe.Context(), grant)))
			stdout.taken()

			r := leakRequest(fixtureExecutionUID, "https://other.test/upload", nil)
			place(r)
			response := httptest.NewRecorder()
			guard.ServeHTTP(response, r.WithContext(gateway.WithIdentity(r.Context(), grant)))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status %d", response.Code)
			}
			events := stdout.taken()
			if len(events) != 1 || events[0].Allowed || events[0].Outcome != audit.RefusedPolicy ||
				events[0].Capability != string(api.CapabilityNetworkEgress) || events[0].RunUID != fixtureExecutionUID {
				t.Fatalf("leak refusal recorded %+v", events)
			}
			line, _ := json.Marshal(events[0])
			if strings.Contains(string(line), credential) || strings.Contains(string(line), "upload") {
				t.Fatalf("record carries request data: %s", line)
			}
			kept := journal.Denials(fixtureExecutionUID, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
			if len(kept) != 1 || !strings.Contains(kept[0].Event.DenyReason, "own provider credential") {
				t.Fatalf("diagnose would show %+v", kept)
			}
			if kept, _ := json.Marshal(kept); strings.Contains(string(kept), credential) {
				t.Fatalf("journal keeps the credential: %s", kept)
			}
		})
	}
}

// The dispatcher's own refusals reach the denial journal with their reason;
// a record it writes for a handler that wrote none does not.
func TestDispatcherRefusalsReachTheJournalAndOmissionsDoNot(t *testing.T) {
	journal := diagnose.NewJournal(nil, diagnose.Limits{})
	grant := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendDocker, UID: fixtureExecutionUID},
		Policy: gateway.PolicySnapshot{Name: fixturePolicyName, Spec: api.AgentPolicySpec{
			AllowedCapabilities: []api.CapabilityKind{api.CapabilityKubernetesRead},
			Budgets:             map[api.CapabilityKind]api.EndpointBudget{api.CapabilityKubernetesRead: {MaxUnits: 1}}}},
		Template:     gateway.TemplateSnapshot{Name: fixtureTemplateName},
		Capabilities: []api.CapabilityKind{api.CapabilityKubernetesRead}}
	silent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	d := gateway.Dispatcher{
		// A budget without its tracker cannot be accounted.
		fixtureAuthority: {Capability: api.CapabilityKubernetesRead, Handler: silent, AuditLogger: journal},
		"open.test:443":  {Admission: gateway.AdmitOpen, Reason: "fixture", Handler: silent, AuditLogger: journal},
	}
	for _, target := range []string{"https://service.test/secret-path?token=abc", "https://open.test/secret-path?token=abc"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		d.ServeHTTP(httptest.NewRecorder(), r.WithContext(gateway.WithIdentity(r.Context(), grant)))
	}
	kept := journal.Denials(fixtureExecutionUID, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if len(kept) != 1 || kept[0].Event.Outcome != audit.Unavailable ||
		kept[0].Event.DenyReason != "budget accounting for kubernetes.read is unavailable; retry later" {
		t.Fatalf("journal kept %+v", kept)
	}
	if line, _ := json.Marshal(kept); strings.Contains(string(line), "secret-path") || strings.Contains(string(line), "abc") {
		t.Fatalf("journal keeps request data: %s", line)
	}
}
