package harness

import (
	"strings"
	"testing"
)

func TestNativeArgsRefuseOnlyFlagsSprooziControls(t *testing.T) {
	allowed := map[NativeKind][][]string{
		NativeCodex: {
			{nativeModelFlag, "gpt-5.5"}, {"-m", "gpt-5.5"}, {"--search"}, {"--add-dir", "/tmp/x"},
			{"-c", "model_reasoning_effort=high"}, {"--config=model=gpt-5.5"}, {"--enable", "x"},
		},
		NativeClaude: {
			{nativeModelFlag, "sonnet"}, {"--effort", "high"}, {"--add-dir", "/tmp"}, {"-d"}, {"--verbose"},
			{"--disallowedTools", "Edit"},
		},
	}
	for kind, cases := range allowed {
		for _, args := range cases {
			if err := ValidateNativeArgs(kind, args); err != nil {
				t.Errorf("%s %q refused: %v", kind, args, err)
			}
		}
	}
	refused := map[NativeKind][][]string{
		NativeCodex: {
			{"--sandbox", "read-only"}, {"-s", "read-only"}, {"-sread-only"}, {"--sandbox=read-only"},
			{"-a", "on-request"}, {"--ask-for-approval=never"}, {"--yolo"}, {"--full-auto"},
			{"-C", "/"}, {"--cd=/"}, {"--worktree"}, {"--oss"}, {"--local-provider", "ollama"},
			{"--profile", "x"}, {"--remote", "ws://x"}, {"--no-daemon"},
			{"-c", "developer_instructions=x"}, {"-c", `"model_provider"="openai"`},
			{"--config", "model_providers.openai.base_url=http://x"}, {"-cprojects.x.trust_level=trusted"},
			{"-c", `mcp_servers."sproozi".url=x`}, {"-c", "approval_policy=on-request"}, {"--", "fix it"},
		},
		NativeClaude: {
			{"--permission-mode", "default"}, {"--dangerously-skip-permissions"}, {"--tools", "Edit"},
			{"--allowedTools=Edit"}, {"--mcp-config", "x.json"}, {"--settings", "{}"}, {"--bare"},
			{"--system-prompt", "x"}, {"--append-system-prompt=x"}, {"-r", "id"}, {"--continue"}, {"-c"},
			{"--session-id", "x"}, {"-p"}, {"--print"}, {"-w", "tree"}, {"--no-session-persistence"},
		},
	}
	for kind, cases := range refused {
		for _, args := range cases {
			err := ValidateNativeArgs(kind, args)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Errorf("%s %q accepted: %v", kind, args, err)
			}
		}
	}
	if ValidateNativeArgs(NativeCodex, make([]string, NativeArgsMax+1)) == nil ||
		ValidateNativeArgs(NativeCodex, []string{strings.Repeat("x", NativeArgMaxBytes+1)}) == nil ||
		ValidateNativeArgs(NativeCodex, []string{"a\x00b"}) == nil {
		t.Fatal("unbounded harness arguments accepted")
	}
}

func TestNativeArgsSelectModel(t *testing.T) {
	for _, tc := range []struct {
		kind NativeKind
		args []string
		want bool
	}{
		{NativeCodex, []string{nativeModelFlag, "x"}, true}, {NativeCodex, []string{"-mx"}, true},
		{NativeCodex, []string{"-c", "model=x"}, true}, {NativeCodex, []string{"-c", "model_reasoning_effort=x"}, false},
		{NativeCodex, []string{"--search"}, false},
		{NativeClaude, []string{"--model=x"}, true}, {NativeClaude, []string{"-m"}, false},
	} {
		if got := NativeArgsSelectModel(tc.kind, tc.args); got != tc.want {
			t.Errorf("%s %q selects model = %v", tc.kind, tc.args, got)
		}
	}
}
