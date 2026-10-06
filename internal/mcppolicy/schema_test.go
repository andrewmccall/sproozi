package mcppolicy_test

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/mcppolicy"
)

func TestDeclarativeArgumentScopeAcceptsOnlyApprovedValues(t *testing.T) {
	constraint, err := mcppolicy.Compile([]byte(`{"type":"object","required":["tenant","limit"],"properties":{"tenant":{"const":"home-ops"},"limit":{"type":"integer","minimum":1,"maximum":10}},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	value, err := mcppolicy.DecodeObject([]byte(`{"tenant":"home-ops","limit":4}`))
	if err != nil || constraint.Validate(value) != nil {
		t.Fatalf("approved scope rejected: %v", err)
	}
	for _, raw := range []string{`{"tenant":"other","limit":4}`, `{"tenant":"home-ops","limit":11}`, `{"limit":4}`, `{"tenant":"home-ops","limit":4,"url":"https://other"}`} {
		value, err := mcppolicy.DecodeObject([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if constraint.Validate(value) == nil {
			t.Fatalf("out-of-scope arguments accepted: %s", raw)
		}
	}
}

func TestArgumentSchemasFailClosedForUnsupportedOrAmbiguousRestrictions(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"tenant":{"allowedValues":["home-ops"]}}}`,
		`{"type":"object","properties":{"tenant":{"format":"hostname"}}}`,
		`{"type":"object","properties":{"tenant":{"$ref":"https://unapproved.example/schema"}}}`,
		`{"type":"object","type":"string"}`,
	} {
		if _, err := mcppolicy.Compile([]byte(raw)); err == nil {
			t.Fatalf("unsupported scope accepted: %s", raw)
		}
	}
	for _, raw := range []string{`{"tenant":"approved","tenant":"other"}`, `{"nested":{"tenant":"approved","tenant":"other"}}`, `{} {}`, `null`} {
		if _, err := mcppolicy.DecodeObject([]byte(raw)); err == nil {
			t.Fatal("ambiguous arguments accepted")
		}
	}
}

func TestNumbersCannotChangeMeaningDuringAuthorization(t *testing.T) {
	for _, raw := range []string{`{"id":9007199254740993}`, `{"id":1.0000000000000001}`, `{"id":1e-999999999}`} {
		if _, err := mcppolicy.DecodeObject([]byte(raw)); err == nil {
			t.Fatalf("rounded number accepted: %s", raw)
		}
		if _, err := mcppolicy.Compile([]byte(`{"const":` + raw + `}`)); err == nil {
			t.Fatalf("rounded policy number accepted: %s", raw)
		}
	}
	for _, raw := range []string{`{"id":9007199254740992}`, `{"id":0.1}`, `{"id":1e2}`} {
		if _, err := mcppolicy.DecodeObject([]byte(raw)); err != nil {
			t.Fatalf("supported number rejected: %s: %v", raw, err)
		}
	}
}

func TestSchemaEvaluationRejectsCyclesAndUnsupportedReferences(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","$ref":"#"}`,
		`{"$defs":{"node":{"properties":{"next":{"$ref":"#/$defs/node"}}}},"$ref":"#/$defs/node"}`,
		`{"$dynamicRef":"#node"}`,
		`{"$schema":"https://json-schema.org/draft/2019-09/schema"}`,
	} {
		if _, err := mcppolicy.Compile([]byte(raw)); err == nil {
			t.Fatalf("unsupported schema accepted: %s", raw)
		}
	}
	constraint, err := mcppolicy.Compile([]byte(`{"$defs":{"tenant":{"const":"home-ops"}},"properties":{"tenant":{"$ref":"#/$defs/tenant"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if constraint.Validate(map[string]any{"tenant": "other"}) == nil {
		t.Fatal("acyclic reference lost its restriction")
	}
}
