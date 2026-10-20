package home

import (
	"bytes"
	"crypto/tls"
	"strings"
	"testing"

	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/harness"
)

func TestHarnessTelemetryIsSkippedWithoutCollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	c := Config{Gateway: GatewayConfig{HarnessTelemetry: true, MCPAuthority: "sproozi-mcp.internal:443"}}
	c.Local.Harness = harness.NativeCodex
	gw := gatewaydocker.GatewayConfig{Certificates: map[string]tls.Certificate{}}
	var log bytes.Buffer
	if err := harnessTelemetry(c, tls.Certificate{}, &gw, &log); err != nil {
		t.Fatalf("missing collector stopped the daemon: %v", err)
	}
	if gw.Telemetry != nil || !strings.Contains(log.String(), "relay disabled") {
		t.Fatalf("relay served without a collector; log %q", log.String())
	}
	c.Gateway.MCPAuthority = ""
	if err := harnessTelemetry(c, tls.Certificate{}, &gw, &log); err == nil {
		t.Fatal("relay accepted without an authority to serve it on")
	}
	c.Gateway.HarnessTelemetry = false
	if err := harnessTelemetry(c, tls.Certificate{}, &gw, &log); err != nil || gw.Telemetry != nil {
		t.Fatal("telemetry off changed the gateway")
	}
}
