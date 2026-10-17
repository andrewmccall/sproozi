package agentcontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func validInputJSON() string {
	return `{"version":"sproozi.agentcontract/v1alpha1","run":{"namespace":"agents","name":"repair","uid":"run-123"},"trusted":{"instructions":"Inspect the workload."},"untrusted":{"task":"Repair the deployment","eventContext":{"alert":"CrashLoopBackOff"}},"capabilities":["kubernetes.read","github.pull_request"],"workspace":"/workspace","gateway":{"endpoint":"https://gateway.svc:8443","tokenPath":"/var/run/sproozi/tokens/gateway/token"}}`
}

func validateInputJSON(t *testing.T, document string) error {
	t.Helper()
	var input Input
	if err := json.Unmarshal([]byte(document), &input); err != nil {
		t.Fatal(err)
	}
	return input.Validate()
}

func TestInputValidateAcceptsKubernetesStyleUIDBeginningWithDigit(t *testing.T) {
	document := strings.Replace(validInputJSON(), `"uid":"run-123"`, `"uid":"8a6f3b50-0000-4000-9000-000000000001"`, 1)
	if err := validateInputJSON(t, document); err != nil {
		t.Fatalf("Validate rejected valid Kubernetes UID: %v", err)
	}
}

func TestInputValidateRejectsMissingDuplicateAndOversizedValues(t *testing.T) {
	cases := map[string]string{
		"missing required":       strings.Replace(validInputJSON(), `"workspace":"/workspace",`, "", 1),
		"duplicate nested key":   strings.Replace(validInputJSON(), `"alert":"CrashLoopBackOff"`, `"alert":"one","alert":"two"`, 1),
		"missing instructions":   strings.Replace(validInputJSON(), `"instructions":"Inspect the workload."`, `"instructions":""`, 1),
		"oversized instructions": strings.Replace(validInputJSON(), "Inspect the workload.", strings.Repeat("a", maxInstructionsRunes+1), 1),
		"duplicate capability":   strings.Replace(validInputJSON(), `"kubernetes.read","github.pull_request"`, `"kubernetes.read","kubernetes.read"`, 1),
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInputJSON(t, document); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
