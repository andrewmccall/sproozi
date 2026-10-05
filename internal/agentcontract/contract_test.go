package agentcontract

import (
	"fmt"
	"strings"
	"testing"
)

func validInputJSON() string {
	return `{"version":"sproozi.agentcontract/v1alpha1","run":{"namespace":"agents","name":"repair","uid":"run-123"},"trusted":{"instructions":"Inspect the workload."},"untrusted":{"task":"Repair the deployment","eventContext":{"alert":"CrashLoopBackOff"}},"capabilities":["kubernetes.read","github.pull_request"],"workspace":"/workspace","gateway":{"endpoint":"https://gateway.svc:8443","tokenPath":"/var/run/sproozi/tokens/gateway/token"}}`
}

func TestDecodeInputPreservesTrustedAndUntrustedContentSeparately(t *testing.T) {
	input, err := DecodeInput([]byte(validInputJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if input.Trusted.Instructions != "Inspect the workload." || input.Untrusted.Task != "Repair the deployment" {
		t.Fatalf("trusted and untrusted values were not preserved separately: %#v", input)
	}
	if string(input.Untrusted.EventContext) != `{"alert":"CrashLoopBackOff"}` {
		t.Fatalf("unexpected event context: %s", input.Untrusted.EventContext)
	}
}

func TestDecodeInputAcceptsKubernetesStyleUIDBeginningWithDigit(t *testing.T) {
	document := strings.Replace(validInputJSON(), `"uid":"run-123"`, `"uid":"8a6f3b50-0000-4000-9000-000000000001"`, 1)
	if _, err := DecodeInput([]byte(document)); err != nil {
		t.Fatalf("DecodeInput rejected valid Kubernetes UID: %v", err)
	}
}

func TestDecodeInputRejectsUnknownMissingDuplicateAndOversizedValues(t *testing.T) {
	cases := map[string]string{
		"unknown field":          strings.Replace(validInputJSON(), `"workspace"`, `"unexpected":true,"workspace"`, 1),
		"missing required":       strings.Replace(validInputJSON(), `"workspace":"/workspace",`, "", 1),
		"duplicate outer key":    strings.Replace(validInputJSON(), `"version":"sproozi.agentcontract/v1alpha1",`, `"version":"sproozi.agentcontract/v1alpha1","version":"sproozi.agentcontract/v1alpha1",`, 1),
		"duplicate nested key":   strings.Replace(validInputJSON(), `"alert":"CrashLoopBackOff"`, `"alert":"one","alert":"two"`, 1),
		"missing instructions":   strings.Replace(validInputJSON(), `"instructions":"Inspect the workload."`, `"instructions":""`, 1),
		"oversized instructions": strings.Replace(validInputJSON(), "Inspect the workload.", strings.Repeat("a", maxInstructionsRunes+1), 1),
		"duplicate capability":   strings.Replace(validInputJSON(), `"kubernetes.read","github.pull_request"`, `"kubernetes.read","kubernetes.read"`, 1),
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeInput([]byte(document)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestDecodeLifecycleEventAllowsOnlyAuditSafeFixedShape(t *testing.T) {
	valid := `{"version":"sproozi.agentcontract/v1alpha1","run":{"namespace":"agents","name":"repair","uid":"run-123"},"sequence":1,"type":"run_started","component":"runner","operation":"execution.start","outcome":"accepted","repository":"owner/repository","prNumber":42,"correlationID":"request-1"}`
	if _, err := DecodeLifecycleEvent([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"prompt", "response", "shellOutput", "patch", "credential"} {
		document := strings.Replace(valid, `"type"`, fmt.Sprintf(`"%s":"sproozi-secret-canary","type"`, field), 1)
		if _, err := DecodeLifecycleEvent([]byte(document)); err == nil {
			t.Fatalf("expected %q field to be rejected", field)
		}
	}
}
