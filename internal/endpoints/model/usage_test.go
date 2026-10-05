package model

import (
	"strings"
	"testing"
)

func TestResponsesUsageSupportsJSONAndBufferedSSE(t *testing.T) {
	response := `{"status":"completed","usage":{"total_tokens":12,"input_tokens":9,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}}`
	stream := "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	for _, body := range []string{response, stream, strings.ReplaceAll(stream, "\n", "\r\n")} {
		streaming := strings.HasPrefix(body, "event:")
		usage, err := decodeUsage([]byte(body), responsesPath, streaming, "text/event-stream; charset=utf-8")
		if err != nil || usage.Usage == nil || *usage.Usage.TotalTokens != 12 || *usage.Usage.PromptTokens != 9 || *usage.Usage.CompletionTokens != 3 || *usage.Usage.PromptTokensDetails.CachedTokens != 4 {
			t.Fatalf("usage=%+v err=%v", usage, err)
		}
	}
}

func TestResponsesStreamRejectsMissingFailedOrAmbiguousCompletion(t *testing.T) {
	complete := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":9,\"output_tokens\":3}}}\n\n"
	for _, body := range []string{
		"data: {\"type\":\"response.created\"}\n\n",
		"data: {\"type\":\"response.failed\"}\n\n",
		"data: {\"type\":\"error\"}\n\n",
		strings.TrimSuffix(complete, "\n"), complete + complete,
		"data: invalid\n\n" + complete,
	} {
		if _, err := decodeUsage([]byte(body), responsesPath, true, "text/event-stream"); err == nil {
			t.Fatalf("invalid stream accepted: %q", body)
		}
	}
	if _, err := decodeUsage([]byte(complete), responsesPath, true, "application/json"); err == nil {
		t.Fatal("non-SSE content type accepted")
	}
}

func TestResponsesStreamVerifiesMissingContentType(t *testing.T) {
	complete := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12}}}\n\n"
	usage, err := decodeUsage([]byte(complete), responsesPath, true, "")
	if err != nil || usage.Usage == nil || usage.Usage.TotalTokens == nil || *usage.Usage.TotalTokens != 12 {
		t.Fatalf("valid SSE with absent Content-Type rejected: %v", err)
	}
	for _, body := range []string{
		`{"usage":{"total_tokens":12}}`,
		"data: {\"type\":\"response.failed\"}\n\n",
		strings.TrimSuffix(complete, "\n"), complete + complete,
	} {
		if _, err := decodeUsage([]byte(body), responsesPath, true, ""); err == nil {
			t.Fatal("absent Content-Type bypassed stream verification")
		}
	}
}
