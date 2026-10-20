package model

import (
	"context"
	"math"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// A self-contained request with an output bound reserves its bytes plus the
// bound, priced at the highest input-side rate and the output rate.
func TestPlanReservationBoundsSelfContainedRequests(t *testing.T) {
	h := NewAnthropicHandler(HandlerConfig{Pricing: &PricingTable{Models: map[string]ModelPrice{"m": {
		InputMicrosPerMillionTokens: 3_000_000, CachedInputMicrosPerMillionTokens: 300_000,
		CacheWrite5mMicrosPerMillionTokens: 3_750_000, OutputMicrosPerMillionTokens: 15_000_000}}}})
	body := []byte(`{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`)
	plan := h.planReservation(body, api.EndpointBudget{MaxUnits: 100_000, MaxCostMicros: 1_000_000})
	// ceil((75 × 3.75 + 1000 × 15) micro-dollars) = 15282.
	if !plan.bounded || !plan.sizable || plan.units != int64(len(body))+1000 || plan.costMicros != 15282 {
		t.Fatalf("plan = %+v for %d bytes", plan, len(body))
	}
	for name, unbounded := range map[string]string{
		"no output bound":           `{"model":"m","input":[]}`,
		"stored response":           `{"model":"m","max_output_tokens":10,"previous_response_id":"resp_1"}`,
		"provider web tool":         `{"model":"m","max_tokens":10,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`,
		"chat web search":           `{"model":"m","max_tokens":10,"web_search_options":{},"messages":[]}`,
		"reasoning replay":          `{"model":"m","max_output_tokens":10,"input":[{"type":"reasoning","id":"rs_1","summary":[]}]}`,
		"empty encrypted reasoning": `{"model":"m","max_output_tokens":10,"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"","summary":[]}]}`,
		"remote image":              `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]}]}`,
		"uploaded file":             `{"model":"m","max_output_tokens":10,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`,
		"chat remote image":         `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.test/a.png"}}]}]}`,
		"nested tool result":        `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"document","source":{"type":"file","file_id":"f"}}]}]}]}`,
	} {
		if plan := h.planReservation([]byte(unbounded), api.EndpointBudget{MaxUnits: 100}); plan.bounded || plan.units != 0 {
			t.Errorf("%s: plan = %+v, want all remaining capacity", name, plan)
		}
	}
}

// Only message content is a reference; tool inputs and inline data are not.
func TestUnsizableReferenceInspectsOnlyContent(t *testing.T) {
	for body, want := range map[string]bool{
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"WebFetch","input":{"url":"https://example.test"}}]}]}`: false,
		`{"input":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`:                                                  false,
		`{"input":[{"type":"input_image","image_url":"https://example.test/a.png"}]}`:                                                  true,
		`{"conversation":"conv_1","input":[]}`:                                                                                         true,
		`{"prompt":"legacy completion text"}`:                                                                                          false,
		`{"prompt":{"id":"pmpt_1"}}`:                                                                                                   true,
		`{"input":[{"type":"item_reference","id":"msg_1"}]}`:                                                                           true,
		`{"input":[{"id":"msg_1"}]}`:                                                                                                   true,
		`{"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]}]}`:                                       false,
		`{"input":[{"type":"message","id":"msg_1","role":"user","content":"hi"}]}`:                                                     false,
	} {
		if got := unsizableReference([]byte(body)); got != want {
			t.Errorf("%s: unsizable = %v, want %v", body, got, want)
		}
	}
}

func TestEventScannerSplitsWholeEventsAcrossChunks(t *testing.T) {
	var scanner eventScanner
	var events []string
	for _, chunk := range []string{"data: a\n", "\ndata: b\r\n", "\r\nda", "ta: c\n\n", "data: partial"} {
		scanner.add([]byte(chunk))
		for event := scanner.next(); event != nil; event = scanner.next() {
			events = append(events, string(event))
		}
	}
	if len(events) != 3 || events[0] != "data: a\n\n" || events[1] != "data: b\r\n\r\n" || events[2] != "data: c\n\n" ||
		string(scanner.rest()) != "data: partial" {
		t.Fatalf("events = %q", events)
	}
}

// If its gateway stops, a call is charged its request bytes ÷ 4 and its whole
// maximum duration at R, capped at its output bound; an unsizable request is
// charged its whole reservation. The lease expires after the call's deadline.
func TestRecoveryChargesTheWholeCallDuration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	call := &modelCall{h: &Handler{}, ctx: ctx, plan: reservationPlan{sizable: true}, body: make([]byte, 400),
		limits: api.EndpointBudget{MaxUnits: 1_000_000}}
	recovery := call.recovery()
	if recovery.Units < 100+299_000 || recovery.Units > 100+300_000 || !recovery.ExpiresAt.Equal(deadline.Add(recoveryMargin)) {
		t.Fatalf("recovery = %+v", recovery)
	}
	call.plan.outputBound = 1000
	if recovery := call.recovery(); recovery.Units != 1100 {
		t.Fatalf("bounded recovery = %+v", recovery)
	}
	call.plan.sizable = false
	if recovery := call.recovery(); recovery.Units != math.MaxInt64 {
		t.Fatalf("unsizable recovery = %+v", recovery)
	}
}
