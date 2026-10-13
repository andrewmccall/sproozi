package nativeconfig

import (
	"strings"
	"testing"
)

const nativeTestModel = "model"

func TestNativeHarnessBoundary(t *testing.T) {
	for _, tc := range []struct {
		kind, model string
		options     []string
		valid       bool
	}{
		{"codex", "gpt-5", []string{"projects.\"/workspace/session/workspace\".trust_level=\"trusted\""}, true},
		{nativeClaudeSelector, "claude-sonnet-4-5", nil, true},
		{nativeClaudeSelector, nativeTestModel, []string{"hooks={}"}, false},
		{nativeClaudeSelector, strings.Repeat("m", 129), nil, false},
		{nativeClaudeSelector, "model\nother", nil, false},
		{"", nativeTestModel, nil, false}, {"other", nativeTestModel, nil, false},
	} {
		err := ValidateHarness(tc.kind, tc.model, tc.options)
		if (err == nil) != tc.valid {
			t.Fatalf("kind=%q accepted=%v error=%v", tc.kind, err == nil, err)
		}
	}
}
