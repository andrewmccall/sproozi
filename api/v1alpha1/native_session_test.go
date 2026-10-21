package v1alpha1

import (
	"strings"
	"testing"
)

func TestRuntimeNativeSessionBoundary(t *testing.T) {
	c := RuntimeClientConfig{Harness: "codex", NativeSession: &NativeSessionConfig{
		Model: "fixture-model", Options: []string{`mcp_servers."fixture-name".enabled=true`},
	}}
	if err := c.validateNativeSession(); err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]string{
		{`"check_for_update_on_startup"=true`}, {`developer_instructions .nested=true`},
		{"fixture=true\nother=false"}, {strings.Repeat("x", 4097)}, make([]string, 33),
	} {
		c.NativeSession.Options = options
		if err := c.validateNativeSession(); err == nil {
			t.Fatal("unsafe runtime native options accepted")
		}
	}
	c.NativeSession.Options = nil
	c.Harness = "hermes"
	if err := c.validateNativeSession(); err == nil {
		t.Fatal("Codex native options accepted for another harness")
	}
}
