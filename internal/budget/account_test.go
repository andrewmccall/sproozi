package budget_test

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"
)

const (
	testID         = "3f2a-run-uid"
	testCapability = "model.inference"
	slashed        = "b/c"
)

func TestAccountRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name, id, capability, key string
		wantID, wantCapability    string
	}{
		{"run UID and capability", testID, testCapability, testID + "/" + testCapability, testID, testCapability},
		{"named MCP capability", testID, "mcp.docs", testID + "/mcp.docs", testID, "mcp.docs"},
		{"empty capability", testID, "", testID + "/", testID, ""},
		{"empty id", "", testCapability, "/" + testCapability, "", testCapability},
		{"both empty", "", "", "/", "", ""},
		// An id with a slash does not round-trip: the first slash splits.
		{"slash in id", "a/b", "c", "a/" + slashed, "a", slashed},
		// A capability may carry one: the id ends at the first slash.
		{"slash in capability", "a", slashed, "a/" + slashed, "a", slashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := budget.Account(tc.id, tc.capability)
			if key != tc.key {
				t.Fatalf("Account(%q, %q) = %q, want %q", tc.id, tc.capability, key, tc.key)
			}
			id, capability, ok := budget.SplitAccount(key)
			if id != tc.wantID || capability != tc.wantCapability || !ok {
				t.Fatalf("SplitAccount(%q) = %q, %q, %v; want %q, %q, true", key, id, capability, ok,
					tc.wantID, tc.wantCapability)
			}
		})
	}
	if _, _, ok := budget.SplitAccount("no-separator"); ok {
		t.Fatal("a key with no slash was split")
	}
}
