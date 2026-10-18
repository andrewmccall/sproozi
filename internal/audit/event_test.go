package audit_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
)

const (
	modelInferenceOperation = "model.inference"
)

func assertNoForbiddenFields[T any](t *testing.T) {
	t.Helper()
	forbidden := []string{"prompt", "credential", "authorization", "apikey", "response", "body", "password", "header", "patch"}
	typ := reflect.TypeFor[T]()
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		tag := strings.ToLower(typ.Field(i).Tag.Get("json"))
		for _, bad := range forbidden {
			if strings.Contains(name, bad) || strings.Contains(tag, bad) {
				t.Errorf("forbidden field %q found in %s", typ.Field(i).Name, typ.Name())
			}
		}
	}
}

func TestAuditEventMarshalIncludesRequiredFields(t *testing.T) {
	remaining := int64(9850)
	e := audit.Event{
		Timestamp:        time.Now(),
		RunID:            "run-abc",
		SAName:           "sproozi-uid123",
		PolicyName:       "default-policy",
		PolicyGeneration: 7,
		Operation:        modelInferenceOperation,
		Allowed:          true,
		DenyReason:       "budget_exhausted",
		UpstreamStatus:   200,
		TokensUsed:       150,
		TokenBudget:      10000,
		TokensRemaining:  &remaining,
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	// Acceptance evidence and the Kind verifier read these names from the
	// gateway's audit stream.
	for _, field := range []string{"runID", "saName", "policyName", "policyGeneration", "operation", "allowed", "denyReason", "upstreamStatus", "tokensUsed", "timestamp"} {
		if !bytes.Contains(data, []byte(`"`+field+`"`)) {
			t.Errorf("missing required field %q in JSON", field)
		}
	}
}

func TestAuditRemainingDistinguishesUnavailableFromExhausted(t *testing.T) {
	zero := int64(0)
	for _, tc := range []struct {
		remaining *int64
		want      string
	}{{nil, `"tokensRemaining":null`}, {&zero, `"tokensRemaining":0`}} {
		data, err := json.Marshal(audit.Event{TokensRemaining: tc.remaining})
		if err != nil || !bytes.Contains(data, []byte(tc.want)) {
			t.Fatalf("remaining budget: got %s, error %v; want %s", data, err, tc.want)
		}
	}
}

// TestAuditEventStructHasNoForbiddenFields verifies no field named after
// sensitive data (prompt, credential, authorization, apiKey, response) exists.
func TestAuditEventStructHasNoForbiddenFields(t *testing.T) {
	assertNoForbiddenFields[audit.Event](t)
	assertNoForbiddenFields[audit.WebhookEvent](t)
}

func TestAuditLoggerWritesWebhookJSONLine(t *testing.T) {
	var buf bytes.Buffer
	l := audit.NewLogger(&buf)
	l.LogWebhook(audit.WebhookEvent{
		TemplateNamespace: "default",
		TemplateName:      "sre-remediation",
		Operation:         "webhook.ingest",
		Allowed:           false,
		DenyReason:        "signature verification failed",
		Source:            "alertmanager",
	})
	if !bytes.Contains(buf.Bytes(), []byte("sre-remediation")) {
		t.Error("expected TemplateName in output")
	}
	if !bytes.Contains(buf.Bytes(), []byte("signature verification failed")) {
		t.Error("expected DenyReason in output")
	}
	var decoded audit.WebhookEvent
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Errorf("logger output is not valid JSON: %v", err)
	}
}

func TestAuditLoggerSerializesConcurrentGatewayEvents(t *testing.T) {
	var buf bytes.Buffer
	logger := audit.NewLogger(&buf)
	const count = 100
	var writers sync.WaitGroup
	writers.Add(count)
	for range count {
		go func() {
			defer writers.Done()
			logger.Log(audit.Event{RunID: "agents/demo", Operation: modelInferenceOperation, Allowed: true})
		}()
	}
	writers.Wait()

	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for i := range count {
		var event audit.Event
		if err := decoder.Decode(&event); err != nil {
			t.Fatalf("decode audit event %d: %v", i, err)
		}
	}
	if decoder.More() {
		t.Fatal("unexpected extra audit events")
	}
}
