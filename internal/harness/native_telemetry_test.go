package harness

import (
	"slices"
	"strings"
	"testing"
)

const (
	testTraceParent  = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	testRelayGateway = "https://gateway.test"
)

func TestNativeTelemetryTargetsOnlyTheGatewayRelayWithContentOff(t *testing.T) {
	for _, tc := range []struct {
		kind            NativeKind
		gateway, parent string
	}{
		{NativeKind("opencode"), testRelayGateway, testTraceParent},
		{NativeCodex, "http://gateway.test", testTraceParent},
		{NativeCodex, "https://gateway.test/path", testTraceParent},
		{NativeCodex, testRelayGateway, "00-bad"},
	} {
		if _, err := WithTelemetry(nil, tc.kind, tc.gateway, tc.parent); err == nil {
			t.Fatalf("accepted %s telemetry to %q with parent %q", tc.kind, tc.gateway, tc.parent)
		}
	}
	codex, err := WithTelemetry(nil, NativeCodex, testRelayGateway, testTraceParent)
	if err != nil {
		t.Fatal(err)
	}
	options := strings.Join(codex.CodexOptions(), "\n")
	for _, want := range []string{`otel.log_user_prompt=false`,
		`otel.exporter={otlp-http={endpoint="https://gateway.test/otlp/v1/logs",protocol="binary"}}`,
		`otel.trace_exporter={otlp-http={endpoint="https://gateway.test/otlp/v1/traces",protocol="binary"}}`,
		`otel.metrics_exporter={otlp-http={endpoint="https://gateway.test/otlp/v1/metrics",protocol="binary"}}`} {
		if !strings.Contains(options, want) {
			t.Fatalf("Codex options lack %s:\n%s", want, options)
		}
	}
	if !slices.Contains(codex.TelemetryEnvironment(), "TRACEPARENT="+testTraceParent) {
		t.Fatal("Codex does not receive the execution trace parent")
	}
	claude, err := WithTelemetry(nil, NativeClaude, testRelayGateway, testTraceParent)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(claude.ClaudeTelemetryEnvironment(), "\n")
	if !strings.Contains(env, "OTEL_EXPORTER_OTLP_ENDPOINT=https://gateway.test/otlp\n") ||
		!strings.Contains(env, "CLAUDE_CODE_ENABLE_TELEMETRY=1") || strings.Contains(env, "OTEL_LOG_") {
		t.Fatalf("Claude telemetry environment:\n%s", env)
	}
	if (*NativeClientConfig)(nil).ClaudeTelemetryEnvironment() != nil || len((&NativeClientConfig{}).CodexOptions()) != 0 {
		t.Fatal("telemetry configured without the relay")
	}
}
