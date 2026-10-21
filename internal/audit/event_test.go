package audit_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/audit"
)

const (
	modelInferenceOperation = "model.inference"
)

func assertNoForbiddenFields[T any](t *testing.T) {
	t.Helper()
	forbidden := []string{"prompt", "credential", "authorization", "apikey", "response", "body", "password", "header", "patch"}
	typ := reflect.TypeFor[T]()
	for field := range typ.Fields() {
		name := strings.ToLower(field.Name)
		tag := strings.ToLower(field.Tag.Get("json"))
		for _, bad := range forbidden {
			if strings.Contains(name, bad) || strings.Contains(tag, bad) {
				t.Errorf("forbidden field %q found in %s", field.Name, typ.Name())
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

// A denied request's path is workload input; the audit record keeps it bounded.
func TestAuditLoggerBoundsWorkloadText(t *testing.T) {
	var buf bytes.Buffer
	audit.NewLogger(&buf).Log(audit.Event{Operation: "GET /" + strings.Repeat("a", 32<<10), Resource: strings.Repeat("é", 1000)})
	var decoded audit.Event
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Operation) > 520 || len(decoded.Resource) > 520 || !utf8.ValidString(decoded.Resource) {
		t.Fatalf("operation %d bytes, resource %d bytes valid=%v", len(decoded.Operation), len(decoded.Resource), utf8.ValidString(decoded.Resource))
	}
}
