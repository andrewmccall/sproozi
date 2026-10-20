// Package otlp relays a harness's own OpenTelemetry export to the
// administrator's collector. The harness is untrusted: the relay replaces its
// sproozi.* resource claims with the authenticated identity, removes attributes
// that carry content or credentials, and bounds every value it forwards.
package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/telemetry"
)

const (
	// Path prefixes the OTLP/HTTP signal paths on the gateway's own authority.
	Path = "/otlp"

	maxBodyBytes   = 4 << 20
	maxStringBytes = 1024
	protobufMedia  = "application/x-protobuf"
	keyValueName   = protoreflect.FullName("opentelemetry.proto.common.v1.KeyValue")
	anyValueName   = protoreflect.FullName("opentelemetry.proto.common.v1.AnyValue")
	resourceName   = protoreflect.FullName("opentelemetry.proto.resource.v1.Resource")
)

const (
	outcomeForwarded = "forwarded"
	outcomeRejected  = "rejected"
	outcomeFailed    = "failed"
)

var relayed, _ = telemetry.Meter().Int64Counter("sproozi.telemetry.relay.requests",
	metric.WithUnit("{request}"), metric.WithDescription("Harness OTLP exports handled by the gateway relay"))

// Config names the collector that receives relayed harness telemetry.
type Config struct {
	// Endpoint is the collector's OTLP/HTTP base URL; signals go to /v1/<signal>.
	Endpoint *url.URL
	Headers  http.Header
	Client   *http.Client
}

// ConfigFromEnvironment enables the relay when SPROOZI_HARNESS_TELEMETRY=relay.
func ConfigFromEnvironment() (*Config, error) {
	switch mode := os.Getenv("SPROOZI_HARNESS_TELEMETRY"); mode {
	case "":
		return nil, nil
	case "relay":
		return CollectorFromEnvironment()
	default:
		return nil, fmt.Errorf("SPROOZI_HARNESS_TELEMETRY=%q is unsupported; use relay", mode)
	}
}

// CollectorFromEnvironment forwards to the same OTEL_EXPORTER_OTLP_ENDPOINT and
// headers as the process's own telemetry, so one collector receives both.
func CollectorFromEnvironment() (*Config, error) {
	endpoint, err := url.Parse(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("the harness telemetry relay requires an http(s) OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	headers := http.Header{}
	for item := range strings.SplitSeq(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		key, value, ok := strings.Cut(item, "=")
		if strings.TrimSpace(item) == "" {
			continue
		}
		decoded, err := url.QueryUnescape(strings.TrimSpace(value))
		if !ok || err != nil || strings.TrimSpace(key) == "" {
			return nil, errors.New("invalid OTEL_EXPORTER_OTLP_HEADERS")
		}
		headers.Add(strings.TrimSpace(key), decoded)
	}
	return &Config{Endpoint: endpoint, Headers: headers, Client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Mount serves the relay under Path and every other path with next.
func Mount(relay *Handler, next http.Handler) http.Handler {
	if relay == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, Path+"/") {
			relay.ServeHTTP(w, r)
			return
		}
		if next == nil {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type Handler struct{ config Config }

func NewHandler(config *Config) *Handler {
	if config == nil {
		return nil
	}
	return &Handler{config: *config}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := gateway.IdentityFromContext(r.Context())
	signal := strings.TrimPrefix(r.URL.Path, Path+"/v1/")
	outcome := outcomeForwarded
	defer func() {
		relayed.Add(r.Context(), 1, metric.WithAttributes(attribute.String("sproozi.telemetry.signal", signal),
			attribute.String("sproozi.telemetry.outcome", outcome)))
	}()
	var request proto.Message
	switch signal {
	case "traces":
		request = &tracepb.ExportTraceServiceRequest{}
	case "metrics":
		request = &metricspb.ExportMetricsServiceRequest{}
	case "logs":
		request = &logspb.ExportLogsServiceRequest{}
	default:
		signal, outcome = "unknown", outcomeRejected
		http.NotFound(w, r)
		return
	}
	if !ok {
		outcome = outcomeRejected
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != protobufMedia {
		outcome = outcomeRejected
		http.Error(w, "OTLP/HTTP protobuf POST required", http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBody(r)
	if err != nil || proto.Unmarshal(body, request) != nil {
		outcome = outcomeRejected
		http.Error(w, "invalid or oversized OTLP export", http.StatusBadRequest)
		return
	}
	scrub(request.ProtoReflect(), resourceIdentity(identity))
	body, err = proto.Marshal(request)
	if err != nil {
		outcome = outcomeRejected
		http.Error(w, "OTLP export could not be re-encoded", http.StatusBadRequest)
		return
	}
	status := h.forward(r.Context(), signal, body)
	if status != http.StatusOK {
		outcome = outcomeFailed
		http.Error(w, "collector unavailable", status)
		return
	}
	w.Header().Set("Content-Type", protobufMedia)
	w.WriteHeader(http.StatusOK)
}

// readBody bounds the request before and after optional gzip decoding.
func readBody(r *http.Request) ([]byte, error) {
	var reader = io.LimitReader(r.Body, maxBodyBytes+1)
	switch r.Header.Get("Content-Encoding") {
	case "":
	case "gzip":
		decoded, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		reader = decoded
	default:
		return nil, errors.New("unsupported content encoding")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes {
		return nil, errors.New("OTLP export exceeded bounds")
	}
	return data, nil
}

// forward returns 200 on success, the collector's own backpressure status so
// the harness exporter retries, or 502 for any other failure.
func (h *Handler) forward(ctx context.Context, signal string, body []byte) int {
	target := h.config.Endpoint.JoinPath("v1", signal)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return http.StatusBadGateway
	}
	maps.Copy(req.Header, h.config.Headers)
	req.Header.Set("Content-Type", protobufMedia)
	client := *h.config.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return http.StatusBadGateway
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return http.StatusOK
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		return resp.StatusCode
	default:
		return http.StatusBadGateway
	}
}

func resourceIdentity(identity *gateway.ExecutionGrant) []*commonpb.KeyValue {
	attrs := append(identity.TelemetryAttributes(), attribute.String(telemetry.TelemetrySource, "harness"))
	out := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, &commonpb.KeyValue{Key: string(attr.Key),
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: attr.Value.String()}}})
	}
	return out
}

// scrub walks every OTLP message generically, so a new signal field cannot
// bypass it. Resources lose harness-claimed sproozi.* attributes and gain the
// trusted identity; every attribute list loses sensitive keys; strings are bounded.
func scrub(m protoreflect.Message, identity []*commonpb.KeyValue) {
	if fd := m.Descriptor().Fields().ByName("resource"); fd != nil && fd.Message() != nil && fd.Message().FullName() == resourceName {
		m.Mutable(fd) // An export without a resource still gets the trusted identity.
	}
	var fields []protoreflect.FieldDescriptor
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		fields = append(fields, fd)
		return true
	})
	for _, fd := range fields {
		switch {
		case fd.Message() == nil:
			if fd.Kind() == protoreflect.StringKind && !fd.IsList() {
				m.Set(fd, protoreflect.ValueOfString(bound(m.Get(fd).String())))
			}
		case fd.IsList():
			list := m.Mutable(fd).List()
			kept := make([]protoreflect.Value, 0, list.Len())
			for i := range list.Len() {
				item := list.Get(i)
				if fd.Message().FullName() == keyValueName && sensitive(item.Message().Interface().(*commonpb.KeyValue).GetKey()) {
					continue
				}
				scrub(item.Message(), identity)
				kept = append(kept, item)
			}
			list.Truncate(0)
			for _, item := range kept {
				list.Append(item)
			}
		case fd.Message().FullName() == anyValueName:
			value := m.Mutable(fd).Message().Interface().(*commonpb.AnyValue)
			if _, binary := value.Value.(*commonpb.AnyValue_BytesValue); binary {
				value.Value = &commonpb.AnyValue_BytesValue{}
			}
			scrub(value.ProtoReflect(), identity)
		default:
			scrub(m.Mutable(fd).Message(), identity)
		}
	}
	if m.Descriptor().FullName() == resourceName {
		list := m.Mutable(m.Descriptor().Fields().ByName("attributes")).List()
		kept := make([]protoreflect.Value, 0, list.Len()+len(identity))
		for i := range list.Len() {
			if !strings.HasPrefix(list.Get(i).Message().Interface().(*commonpb.KeyValue).GetKey(), "sproozi.") {
				kept = append(kept, list.Get(i))
			}
		}
		list.Truncate(0)
		for _, item := range kept {
			list.Append(item)
		}
		for _, attr := range identity {
			list.Append(protoreflect.ValueOfMessage(proto.Clone(attr).ProtoReflect()))
		}
	}
}

// contentWords are attribute name segments that carry prompts, responses,
// tool input or output, or credentials in the harnesses' own conventions,
// such as gen_ai.input.messages, prompt, tool_parameters or arguments.
var contentWords = []string{"prompt", "prompts", "messages", "instructions", "definitions", "arguments", "args",
	"input", "output", "result", "results", "content", "contents", "body", "command", "parameters", "query",
	"response", "responses", "completion", "authorization", "cookie", "token", "secret", "password", "email",
	"api_key", "apikey", "header"}

func sensitive(key string) bool {
	key = strings.ToLower(key)
	segments := strings.Split(key, ".")
	last := segments[len(segments)-1]
	if strings.HasPrefix(key, "http.request.header.") || strings.HasPrefix(key, "http.response.header.") {
		return true
	}
	for _, word := range contentWords {
		if last == word || strings.HasSuffix(last, "_"+word) {
			return true
		}
	}
	return false
}

func bound(value string) string {
	if len(value) <= maxStringBytes {
		return value
	}
	value = value[:maxStringBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
