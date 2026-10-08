package model

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicJSONCountsDisjointCacheCategories(t *testing.T) {
	for _, test := range []struct {
		name, usage string
		want        anthropicUsage
	}{
		{"nullable optional counters", `{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"cache_creation":null}`, anthropicUsage{Input: 10, Output: 3, Total: 13}},
		{"uncached", `{"input_tokens":10,"output_tokens":3}`, anthropicUsage{Input: 10, Output: 3, Total: 13}},
		{"five minute default", `{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":20,"cache_creation_input_tokens":8}`, anthropicUsage{Input: 10, CacheRead: 20, CacheWrite5m: 8, Output: 3, Total: 41}},
		{"mixed TTLs", `{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":20,"cache_creation_input_tokens":8,"cache_creation":{"ephemeral_5m_input_tokens":6,"ephemeral_1h_input_tokens":2}}`, anthropicUsage{Input: 10, CacheRead: 20, CacheWrite5m: 6, CacheWrite1h: 2, Output: 3, Total: 41}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"type":"message","role":"assistant","model":"claude-test","stop_reason":"tool_use","usage":` + test.usage + `}`
			got, err := decodeAnthropicUsage([]byte(body), false, "application/json")
			if err != nil || got != test.want {
				t.Fatalf("usage=%+v err=%v, want %+v", got, err, test.want)
			}
		})
	}
}

func TestAnthropicJSONRejectsIncompleteOrInvalidUsage(t *testing.T) {
	valid := `{"type":"message","role":"assistant","model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`
	for _, body := range []string{
		`null`, `{}`, valid + valid,
		strings.Replace(valid, `"message"`, `"error"`, 1),
		strings.Replace(valid, `"assistant"`, `"user"`, 1),
		strings.Replace(valid, `"claude-test"`, `""`, 1),
		strings.Replace(valid, `"end_turn"`, `null`, 1),
		strings.Replace(valid, `"end_turn"`, `"unknown"`, 1),
		strings.Replace(valid, `"input_tokens":10,`, ``, 1),
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":null`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":-1`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":1.5`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":9223372036854775807`, 1),
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":3,"cache_creation_input_tokens":8,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":2}`, 1),
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":3,"cache_creation_input_tokens":8,"cache_creation":{"ephemeral_5m_input_tokens":8,"ephemeral_1h_input_tokens":null}`, 1),
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}`, 1),
	} {
		if got, err := decodeAnthropicUsage([]byte(body), false, "application/json"); err == nil {
			t.Fatalf("invalid response accepted: %s => %+v", body, got)
		}
	}
}

func TestAnthropicStreamCountsFinalCumulativeOutput(t *testing.T) {
	stream := anthropicTestStart + "event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":2}}\n\n" + anthropicTestFinish
	for _, body := range []string{stream, strings.ReplaceAll(stream, "\n", "\r\n")} {
		for _, contentType := range []string{"text/event-stream; charset=utf-8", ""} {
			got, err := decodeAnthropicUsage([]byte(body), true, contentType)
			want := anthropicUsage{Input: 10, CacheRead: 20, CacheWrite5m: 8, Output: 3, Total: 41}
			if err != nil || got != want {
				t.Fatalf("usage=%+v err=%v, want %+v", got, err, want)
			}
		}
	}
}

const anthropicTestStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":8,\"output_tokens\":1}}}\n\n"
const anthropicTestFinish = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestAnthropicStreamRejectsMissingOrAmbiguousCompletion(t *testing.T) {
	valid := anthropicTestStart + anthropicTestFinish
	for _, body := range []string{
		anthropicTestStart, anthropicTestFinish, strings.TrimSuffix(valid, "\n"),
		valid + anthropicTestFinish, anthropicTestStart + valid,
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":0`, 1),
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":null`, 1),
		strings.Replace(valid, `"output_tokens":3`, `"other":3`, 1),
		strings.Replace(valid, `"output_tokens":3`, `"output_tokens":3,"cache_read_input_tokens":19`, 1),
		strings.Replace(valid, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1),
		strings.Replace(valid, `"stop_reason":"end_turn"`, `"stop_reason":"unknown"`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":null`, 1),
		strings.Replace(valid, "event: message_start", "event: message_delta", 1),
		strings.Replace(valid, "event: message_start", "event: message_start\nevent: message_start", 1),
		strings.Replace(valid, "event: message_start\n", "", 1),
		anthropicTestStart + "event: error\ndata: {\"type\":\"error\"}\n\n" + anthropicTestFinish,
		valid + "event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: message_start\ndata: invalid\n\n" + anthropicTestFinish,
	} {
		if got, err := decodeAnthropicUsage([]byte(body), true, "text/event-stream"); err == nil {
			t.Fatalf("invalid stream accepted: %q => %+v", body, got)
		}
	}
	if _, err := decodeAnthropicUsage([]byte(valid), true, "application/json"); err == nil {
		t.Fatal("non-SSE content type accepted")
	}
}

func TestAnthropicRequestValidatesOperationAndBillableOptions(t *testing.T) {
	base := `{"model":"claude-test","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"Hi"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`
	for _, suffix := range []string{"", "?beta=true"} {
		r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages"+suffix, nil)
		if err := validateAnthropicRequest(r, []byte(base), true); err != nil {
			t.Fatalf("valid request rejected: %v", err)
		}
	}
	for _, test := range []struct {
		method, path, body string
		capped             bool
	}{
		{http.MethodGet, anthropicMessagesPath, base, true},
		{http.MethodPost, "/v1/models", base, true},
		{http.MethodPost, "/v1/messages?host=evil", base, true},
		{http.MethodPost, anthropicMessagesPath, `null`, true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"claude-test"`, `""`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `128`, `0`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `128`, `null`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `true`, `null`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"name":"read_file"`, `"type":"web_search_20250305","name":"web_search"`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"content":"Hi"`, `"content":[{"type":"text","text":"Hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"model":`, `"speed":"fast","model":`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"model":`, `"service_tier":"auto","model":`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"model":`, `"inference_geo":"us","model":`, 1), true},
		{http.MethodPost, anthropicMessagesPath, strings.Replace(base, `"model":`, `"container":{},"model":`, 1), true},
	} {
		r := httptest.NewRequest(test.method, "https://api.anthropic.com"+test.path, nil)
		if err := validateAnthropicRequest(r, []byte(test.body), test.capped); err == nil {
			t.Fatalf("invalid request accepted: %s %s %s", test.method, test.path, test.body)
		}
	}
	uncapped := strings.Replace(base, `"model":`, `"speed":"fast","model":`, 1)
	if err := validateAnthropicRequest(httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil), []byte(uncapped), false); err != nil {
		t.Fatalf("uncapped options rejected: %v", err)
	}
	fiveMinutes := strings.Replace(base, `"content":"Hi"`, `"content":[{"type":"text","text":"Hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]`, 1)
	if err := validateAnthropicRequest(httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil), []byte(fiveMinutes), true); err != nil {
		t.Fatalf("default cache TTL rejected: %v", err)
	}
}

func TestAnthropicStreamUsesUpdatedCumulativeInputCounts(t *testing.T) {
	stream := anthropicTestStart + strings.Replace(anthropicTestFinish, `"output_tokens":3`, `"output_tokens":3,"input_tokens":12,"cache_read_input_tokens":22,"cache_creation_input_tokens":10`, 1)
	got, err := decodeAnthropicUsage([]byte(stream), true, "text/event-stream")
	want := anthropicUsage{Input: 12, CacheRead: 22, CacheWrite5m: 10, Output: 3, Total: 47}
	if err != nil || got != want {
		t.Fatalf("usage=%+v err=%v, want %+v", got, err, want)
	}
}

func TestAnthropicStreamRetainsKnownCountsOnNullableDelta(t *testing.T) {
	stream := anthropicTestStart + strings.Replace(anthropicTestFinish, `"output_tokens":3`, `"output_tokens":3,"input_tokens":null,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"cache_creation":null`, 1)
	got, err := decodeAnthropicUsage([]byte(stream), true, "text/event-stream")
	want := anthropicUsage{Input: 10, CacheRead: 20, CacheWrite5m: 8, Output: 3, Total: 41}
	if err != nil || got != want {
		t.Fatalf("usage=%+v err=%v, want %+v", got, err, want)
	}
}

func TestAnthropicRequestSeparatesCacheMetadataFromToolData(t *testing.T) {
	body := `{"model":"claude-test","max_tokens":128,"cache_control":null,"tools":[{"name":"inspect","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tool1","name":"inspect","input":{"cache_control":"no-cache","nested":{"cache_control":{"ttl":"1h"}}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool1","content":[{"type":"text","text":"{\"cache_control\":\"no-cache\"}","cache_control":null}]}]}]}`
	r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err := validateAnthropicRequest(r, []byte(body), true); err != nil {
		t.Fatal("tool data rejected as provider metadata", err)
	}
	for _, capped := range []string{
		strings.Replace(body, `"cache_control":null`, `"cache_control":{"ttl":"1h"}`, 1),
		strings.Replace(body, `"name":"inspect","input_schema"`, `"name":"inspect","cache_control":{"ttl":"1h"},"input_schema"`, 1),
		strings.Replace(body, `"cache_control":null}]}]}]`, `"cache_control":{"ttl":"1h"}}]}]}]`, 1),
	} {
		if err := validateAnthropicRequest(r, []byte(capped), true); err == nil {
			t.Fatal("provider TTL metadata escaped validation")
		}
	}
}
