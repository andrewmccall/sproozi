package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDemoPullRequestRequiresRunScopeAndNarrowChange(t *testing.T) {
	var pr pullRequestDocument
	data := `{"number":3,"url":"https://github.com/example/demo/pull/3",` +
		`"headRefName":"sproozi/uid/fix-demo","headRefOid":"` + strings.Repeat("a", 40) +
		`","baseRefName":"main","headRepository":{"nameWithOwner":"example/demo"},` +
		`"files":[{"path":"demo-workload.yaml"}]}`
	if err := json.Unmarshal([]byte(data), &pr); err != nil {
		t.Fatal(err)
	}
	if err := validateDemoPullRequest(pr, "example/demo", "main", "uid"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*pullRequestDocument){
		func(p *pullRequestDocument) { p.HeadRefName = "sproozi/another/fix-demo" },
		func(p *pullRequestDocument) { p.BaseRefName = "production" },
		func(p *pullRequestDocument) { p.URL = "https://github.com/other/demo/pull/3" },
		func(p *pullRequestDocument) { p.HeadRepository = nil },
		func(p *pullRequestDocument) { p.HeadRefOID = "missing" },
		func(p *pullRequestDocument) { p.Files = nil },
	} {
		bad := pr
		change(&bad)
		if validateDemoPullRequest(bad, "example/demo", "main", "uid") == nil {
			t.Fatal("invalid PR counted as proof")
		}
	}
}

func TestDemoAuditRequiresSuccessAndSpecificPolicyDenials(t *testing.T) {
	prURL := "https://github.com/example/demo/pull/3"
	events := []auditEvidence{
		{Capability: kubernetesCapability, Allowed: true, UpstreamStatus: 200},
		{Capability: kubernetesCapability, Resource: "/api/v1/namespaces/sproozi-demo/secrets",
			DenyReason: "operation not permitted by policy"},
		{Capability: githubCapability, Resource: "sproozi-denied/not-authorized",
			DenyReason: "repository not permitted by policy"},
		{Capability: githubCapability, Allowed: true, UpstreamStatus: 201, ArtifactURL: prURL},
		{Capability: "model.inference", Allowed: true, UpstreamStatus: 200, TokensUsed: 42},
	}
	for i := range events {
		events[i].PolicyName, events[i].PolicyGeneration, events[i].SemanticLevel = "sre", 1, "semantic"
	}
	if err := validateDemoAudit(events, prURL); err != nil {
		t.Fatal(err)
	}
	for i := range events {
		incomplete := append([]auditEvidence{}, events[:i]...)
		incomplete = append(incomplete, events[i+1:]...)
		if validateDemoAudit(incomplete, prURL) == nil {
			t.Fatalf("missing event %d accepted", i)
		}
	}
	events[2].DenyReason = "upstream request failed"
	if validateDemoAudit(events, prURL) == nil {
		t.Fatal("transport failure counted as policy denial")
	}
}
