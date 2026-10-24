package harness

import (
	"strings"
	"testing"
)

func TestLoginNamesAreShortDNSLabels(t *testing.T) {
	for _, name := range []string{"default", "work", "a", "team-2", strings.Repeat("a", LoginMaxLength)} {
		if err := ValidateLogin(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	for _, name := range []string{"", "Work", "-a", "a-", "a_b", "a.b", "a b", strings.Repeat("a", LoginMaxLength+1)} {
		if ValidateLogin(name) == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if LoginOrDefault("") != DefaultLogin || LoginOrDefault("work") != "work" {
		t.Fatal("default login")
	}
}
