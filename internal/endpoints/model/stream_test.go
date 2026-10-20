package model_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const (
	anthropicStreamRequest = `{"model":"claude-fixture","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"list"}]}`
	responsesStreamRequest = `{"model":"test","stream":true,"input":[]}`
	anthropicAccount       = testRunUID + "/model.inference"
	eventStream            = "text/event-stream"
)

func fixedClock() func() time.Time {
	now := time.Unix(1_800_000_000, 0)
	return func() time.Time { return now }
}

// manualClock is the injected clock that measures how long a call ran.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock { return &manualClock{now: time.Unix(1_800_000_000, 0)} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// streamRecorder is a ResponseWriter whose body the test reads while the
// handler is still relaying.
type streamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushed chan struct{}
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: http.Header{}, flushed: make(chan struct{}, 1)}
}

func (r *streamRecorder) Header() http.Header { return r.header }
func (r *streamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}
func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *streamRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}
func (r *streamRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// waitFor waits until the relayed body contains want.
func (r *streamRecorder) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !strings.Contains(r.String(), want) {
		select {
		case <-r.flushed:
		case <-deadline:
			t.Fatalf("relayed body %q never contained %q", r.String(), want)
		}
	}
}

// pipeProvider is a provider whose response body the test writes event by event.
type pipeProvider struct {
	body     *io.PipeWriter
	requests chan *http.Request
}

func newPipeProvider() (*pipeProvider, *http.Client) {
	reader, writer := io.Pipe()
	provider := &pipeProvider{body: writer, requests: make(chan *http.Request, 1)}
	return provider, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		provider.requests <- r
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader, Request: r}
		response.Header.Set("Content-Type", eventStream)
		return response, nil
	})}
}

func (p *pipeProvider) send(t *testing.T, events ...string) {
	t.Helper()
	for _, event := range events {
		if _, err := io.WriteString(p.body, event); err != nil {
			t.Fatal(err)
		}
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fixtureEvents splits a fixture into its events, each with its blank line.
func fixtureEvents(t *testing.T, name string) []string {
	t.Helper()
	parts := strings.SplitAfter(fixture(t, name), "\n\n")
	return parts[:len(parts)-1]
}

type streamGateway struct {
	tracker *budget.Tracker
	audit   *captureWriter
	logs    *bytes.Buffer
	serve   http.Handler
	account string
}

// anthropicGateway routes the Messages handler with shared accounting.
func anthropicGateway(t *testing.T, limits api.EndpointBudget, client *http.Client, now func() time.Time) *streamGateway {
	t.Helper()
	g := &streamGateway{tracker: budget.NewTracker(), audit: &captureWriter{}, logs: &bytes.Buffer{}, account: anthropicAccount}
	h := model.NewAnthropicHandler(model.HandlerConfig{Pricing: anthropicFixturePricing(),
		ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret}, UpstreamURL: testModelUpstream,
		HTTPClient: client, AuditLogger: g.audit, Logger: slog.New(slog.NewTextHandler(g.logs, nil)), Now: now})
	identity := testExecutionGrant()
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = limits
	routes := gateway.Dispatcher{model.AnthropicAuthority: {Capability: string(api.CapabilityModelInference), Handler: h, Budgets: g.tracker}}
	g.serve = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), identity)))
	})
	return g
}

// responsesGateway serves the OpenAI handler with the test identity.
func responsesGateway(t *testing.T, limits api.EndpointBudget, client *http.Client, now func() time.Time) *streamGateway {
	t.Helper()
	identity := testExecutionGrant()
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = limits
	h := newHandler(t, identity, testModelUpstream)
	g := &streamGateway{tracker: h.Config.BudgetTracker, audit: &captureWriter{}, logs: &bytes.Buffer{}, serve: h, account: testRunUID}
	h.Config.AuditLogger, h.Config.HTTPClient, h.Config.Now = g.audit, client, now
	h.Config.Logger = slog.New(slog.NewTextHandler(g.logs, nil))
	return g
}

// start serves one request in the background with a cancellable context.
func (g *streamGateway) start(path, body string) (*streamRecorder, context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		host := "https://api.openai.com"
		if g.account == anthropicAccount {
			host = "https://api.anthropic.com"
		}
		g.serve.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodPost, host+path, strings.NewReader(body)))
	}()
	return recorder, cancel, done
}

func (g *streamGateway) state(t *testing.T) budget.State {
	t.Helper()
	return observedState(t, g.tracker, g.account)
}

// settled returns the one audit event, after checking how the call was charged.
func (g *streamGateway) settled(t *testing.T, source, reason string, units int64) audit.Event {
	t.Helper()
	if len(g.audit.events) != 1 {
		t.Fatalf("audit events = %+v, want one", g.audit.events)
	}
	e := g.audit.events[0]
	state := g.state(t)
	if !e.Allowed || e.UsageSource != source || e.EndReason != reason || e.TokensUsed != units ||
		state.UnitsUsed != units || state.ReservedUnits != 0 {
		t.Fatalf("audit=%+v state=%+v, want %s %s %d units", e, state, source, reason, units)
	}
	return e
}

func finished(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(20 * time.Second): // generous for multi-MiB relays under -race
		t.Fatal("handler did not finish")
	}
}

func inputEstimate(body string) int64 { return int64((len(body) + 3) / 4) }

func TestAnthropicStreamCompletesWithThinkingAndToolUse(t *testing.T) {
	stream := fixture(t, "anthropic-thinking-tool-use.sse")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := modelResponse(r, stream)
		response.Header.Set("Content-Type", eventStream)
		return response, nil
	})}
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000, MaxCostMicros: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start("/v1/messages", anthropicStreamRequest)
	defer cancel()
	finished(t, done)
	if recorder.String() != stream || recorder.Header().Get("Content-Type") != eventStream {
		t.Fatalf("stream changed in relay: %q", recorder.String())
	}
	e := g.settled(t, "provider", "completed", 120+40+58)
	if e.InputTokens != 160 || e.OutputTokens != 58 || g.state(t).CostMicros != 218 {
		t.Fatalf("audit=%+v state=%+v", e, g.state(t))
	}
}

// Esc after message_start charges Anthropic's exact input and the time floor
// at R = 250 tokens a second, not the reservation.
func TestAnthropicStreamCutAfterMessageStartChargesExactInput(t *testing.T) {
	provider, client := newPipeProvider()
	clock := newManualClock()
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, clock.Now)
	recorder, cancel, done := g.start("/v1/messages", anthropicStreamRequest)
	events := fixtureEvents(t, "anthropic-thinking-tool-use.sse")
	provider.send(t, events[0], events[1], events[3])
	recorder.waitFor(t, "thinking_delta")
	clock.Advance(2 * time.Second)
	cancel()
	finished(t, done)
	g.settled(t, "estimated", "client_cancelled", 160+500)
	for _, want := range []string{`msg="Charged estimated model usage"`, "endReason=client_cancelled", "units=660",
		"inputSource=provider", "visibleBytes=31", "elapsed=2s"} {
		if !strings.Contains(g.logs.String(), want) {
			t.Fatalf("log %q lacks %q", g.logs.String(), want)
		}
	}
	if strings.Contains(g.logs.String(), "Charged model reservation in full") {
		t.Fatal("an estimated interrupt was charged in full")
	}
}

func TestAnthropicStreamCutBeforeMessageStartChargesRequestBytes(t *testing.T) {
	provider, client := newPipeProvider()
	clock := newManualClock()
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, clock.Now)
	_, cancel, done := g.start("/v1/messages", anthropicStreamRequest)
	<-provider.requests
	time.Sleep(20 * time.Millisecond) // let the relay take the response headers
	clock.Advance(time.Second)
	cancel()
	finished(t, done)
	g.settled(t, "estimated", "client_cancelled", inputEstimate(anthropicStreamRequest)+250)
	if !strings.Contains(g.logs.String(), "inputSource=request_bytes") {
		t.Fatalf("log %q", g.logs.String())
	}
}

// The provider's error event still reaches the harness; the call is charged
// the estimate.
func TestAnthropicStreamErrorEventRelaysAndChargesEstimate(t *testing.T) {
	events := fixtureEvents(t, "anthropic-thinking-tool-use.sse")
	failure := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := modelResponse(r, events[0]+events[7]+failure)
		response.Header.Set("Content-Type", eventStream)
		return response, nil
	})}
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start("/v1/messages", anthropicStreamRequest)
	defer cancel()
	finished(t, done)
	if !strings.HasSuffix(recorder.String(), failure) {
		t.Fatalf("provider error event was not relayed: %q", recorder.String())
	}
	g.settled(t, "estimated", "upstream_error", 160+int64(len("I'll list it.")))
}

// A response.completed event over 1 MiB, which carries the whole output, is
// relayed and settled from its usage.
func TestResponsesStreamSettlesLargeCompletion(t *testing.T) {
	events := fixtureEvents(t, "responses-reasoning.sse")
	completed := strings.Replace(events[len(events)-1], `"status":"completed"`,
		`"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"`+strings.Repeat("y", 2<<20)+`"}]}]`, 1)
	stream := strings.Join(events[:len(events)-1], "") + completed
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := modelResponse(r, stream)
		response.Header.Set("Content-Type", eventStream)
		return response, nil
	})}
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	defer cancel()
	finished(t, done)
	if recorder.String() != stream {
		t.Fatalf("relayed %d of %d bytes: %.400q", len(recorder.String()), len(stream), recorder.String())
	}
	g.settled(t, "provider", "completed", 1250)
}

func TestResponsesIncompleteSettlesItsUsage(t *testing.T) {
	stream := fixture(t, "responses-incomplete.sse")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := modelResponse(r, stream)
		response.Header.Set("Content-Type", eventStream)
		return response, nil
	})}
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	defer cancel()
	finished(t, done)
	if recorder.String() != stream {
		t.Fatalf("stream changed in relay: %q", recorder.String())
	}
	g.settled(t, "provider", "completed", 104)
}

// Esc while the model reasons, with its output hidden, still pays for the
// elapsed time; the bytes reach the harness before upstream finishes.
func TestResponsesStreamCutMidReasoningChargesElapsedTime(t *testing.T) {
	provider, client := newPipeProvider()
	clock := newManualClock()
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, clock.Now)
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	events := fixtureEvents(t, "responses-reasoning.sse")
	provider.send(t, events[0], events[1], events[2], events[3])
	recorder.waitFor(t, "reasoning_summary_text.delta")
	select {
	case <-done:
		t.Fatal("the handler finished before the provider")
	default:
	}
	clock.Advance(10 * time.Second)
	cancel()
	finished(t, done)
	// Codex sends no output bound, so the call reserved all 10000 units.
	g.settled(t, "estimated", "client_cancelled", inputEstimate(responsesStreamRequest)+2500)
}

func TestChatGPTStreamWithoutContentTypeRelaysAndSettles(t *testing.T) {
	stream := fixture(t, "chatgpt.sse")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, server.Client(), fixedClock())
	h := g.serve.(*identityHandler)
	h.Config.UpstreamURL = server.URL
	h.Config.ProviderAuth = modelauth.NewChatGPT(chatGPTStore{}, http.DefaultClient)
	recorder, cancel, done := g.start(testResponsesPath, `{"model":"gpt-5.3-codex","store":false,"stream":true,"input":[]}`)
	defer cancel()
	finished(t, done)
	if recorder.String() != stream || recorder.Header().Get("Content-Type") != eventStream {
		t.Fatalf("ChatGPT stream changed in relay: %q", recorder.String())
	}
	g.settled(t, "provider", "completed", 5020)
}

// When an unbounded call's observed usage, its input plus visible bytes ÷ 4,
// reaches the reservation, the harness receives
// the protocol's error event, the provider call is cancelled and the
// reservation is charged.
func TestStreamIsCutAtItsReservation(t *testing.T) {
	provider, client := newPipeProvider()
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 100}, client, fixedClock())
	// 11 input units; after two deltas 11 + 300 ÷ 4 = 86, after three 124.
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	defer cancel()
	upstream := <-provider.requests
	go func() {
		delta := `event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("z", 150) + `"}` + "\n\n"
		for range 3 {
			if _, err := io.WriteString(provider.body, delta); err != nil {
				return
			}
		}
	}()
	finished(t, done)
	if !strings.HasSuffix(recorder.String(), `"code":"budget_exhausted","message":"model budget exhausted: the call reached its reservation","param":null}`+"\n\n") ||
		strings.Count(recorder.String(), `"delta":"`) != 2 {
		t.Fatalf("budget cut was not relayed as an error event: %q", recorder.String())
	}
	if !errors.Is(upstream.Context().Err(), context.Canceled) {
		t.Fatal("the provider call was not cancelled")
	}
	g.settled(t, "reservation", "budget_cut", 100)
}

// A request that references input the gateway cannot size has no estimate,
// so an interrupt charges the whole reservation.
func TestUnsizableReferenceChargesTheReservationOnInterrupt(t *testing.T) {
	provider, client := newPipeProvider()
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	_, cancel, done := g.start(testResponsesPath, `{"model":"test","stream":true,"previous_response_id":"resp_1","max_output_tokens":50}`)
	<-provider.requests
	time.Sleep(20 * time.Millisecond)
	cancel()
	finished(t, done)
	g.settled(t, "reservation", "client_cancelled", 10_000)
	for _, want := range []string{`msg="Charged model reservation in full"`,
		`detail="request references input the gateway cannot size"`, "endReason=client_cancelled", "units=10000"} {
		if !strings.Contains(g.logs.String(), want) {
			t.Fatalf("log %q lacks %q", g.logs.String(), want)
		}
	}
}

// An event larger than the parser's bound is relayed unparsed, and the call
// is charged its reservation.
func TestOversizedEventChargesTheReservation(t *testing.T) {
	events := fixtureEvents(t, "responses-reasoning.sse")
	large := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("w", 8<<20) + `"}` + "\n\n"
	stream := events[0] + large + events[len(events)-1]
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := modelResponse(r, stream)
		response.Header.Set("Content-Type", eventStream)
		return response, nil
	})}
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	defer cancel()
	finished(t, done)
	if recorder.String() != stream {
		t.Fatalf("relayed %d of %d bytes: %.400q", len(recorder.String()), len(stream), recorder.String())
	}
	g.settled(t, "reservation", "parse_limit", 10_000)
}

// A provider error before any response header reached the harness releases
// the reservation.
func TestProviderFailureBeforeHeadersReleasesTheReservation(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	defer cancel()
	finished(t, done)
	if state := g.state(t); recorder.status != http.StatusBadGateway || state.UnitsUsed != 0 || state.ReservedUnits != 0 {
		t.Fatalf("status=%d state=%+v", recorder.status, state)
	}
}

// Elapsed time alone never cuts a stream: a long unbounded call with few
// visible bytes runs on, and only an interrupt is priced by the time floor.
func TestLongQuietStreamIsNotCut(t *testing.T) {
	provider, client := newPipeProvider()
	clock := newManualClock()
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 1000}, client, clock.Now)
	recorder, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	defer cancel()
	events := fixtureEvents(t, "responses-reasoning.sse")
	provider.send(t, events[0], events[1], events[2])
	recorder.waitFor(t, "response.output_item.added")
	for _, event := range []string{events[3], events[4]} {
		clock.Advance(time.Hour)            // far past (1000 units) ÷ 250 tokens a second
		time.Sleep(1100 * time.Millisecond) // let the relay's one-second check run
		provider.send(t, event)
	}
	recorder.waitFor(t, "response.output_item.done")
	provider.send(t, events[5], strings.Replace(events[6], `"total_tokens":1250`, `"total_tokens":900`, 1))
	_ = provider.body.Close()
	finished(t, done)
	if strings.Contains(recorder.String(), "budget_exhausted") {
		t.Fatalf("a quiet stream was cut by elapsed time: %q", recorder.String())
	}
	g.settled(t, "provider", "completed", 900)
}

// A bounded call is never cut: Claude Code writing a large file relays far
// more visible bytes than its small request reserved, and the provider's own
// max_tokens bounds it. Usage above the reservation settles as exceeded.
func TestBoundedStreamIsNotCut(t *testing.T) {
	provider, client := newPipeProvider()
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	request := `{"model":"claude-fixture","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"write"}]}`
	recorder, cancel, done := g.start("/v1/messages", request)
	defer cancel()
	events := fixtureEvents(t, "anthropic-thinking-tool-use.sse")
	delta := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"" +
		strings.Repeat("f", 4000) + "\"}}\n\n"
	provider.send(t, events[0], events[6], delta, delta, events[8],
		strings.Replace(events[len(events)-2], `"output_tokens":58`, `"output_tokens":2000`, 1), events[len(events)-1])
	_ = provider.body.Close()
	finished(t, done)
	if strings.Contains(recorder.String(), "budget_error") || strings.Count(recorder.String(), strings.Repeat("f", 4000)) != 2 {
		t.Fatalf("a bounded call was cut: %.300q", recorder.String())
	}
	// 160 input + 2000 output exceeds the reservation of request bytes + 100.
	e := g.audit.events[0]
	if e.UsageSource != "provider" || e.EndReason != "exceeded" || e.TokensUsed != 2160 {
		t.Fatalf("audit = %+v", e)
	}
}

// blockingProvider reads the whole request, then sends nothing until the call ends.
func blockingProvider(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		received <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server, received
}

func anthropicGatewayAt(t *testing.T, upstream string, client *http.Client, now func() time.Time, drain ...*model.Drain) *streamGateway {
	t.Helper()
	g := &streamGateway{tracker: budget.NewTracker(), audit: &captureWriter{}, logs: &bytes.Buffer{}, account: anthropicAccount}
	h := model.NewAnthropicHandler(model.HandlerConfig{ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret},
		UpstreamURL: upstream, HTTPClient: client, AuditLogger: g.audit,
		Logger: slog.New(slog.NewTextHandler(g.logs, nil)), Now: now})
	if len(drain) > 0 {
		h.Config.Drain = drain[0]
	}
	identity := testExecutionGrant()
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 100_000}
	routes := gateway.Dispatcher{model.AnthropicAuthority: {Capability: string(api.CapabilityModelInference), Handler: h, Budgets: g.tracker}}
	g.serve = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), identity)))
	})
	return g
}

const nonStreamingRequest = `{"model":"claude-fixture","max_tokens":64000,"messages":[{"role":"user","content":"write"}]}`

// A call that does not stream sends headers only when generation finishes.
// Once the provider has the whole request, cancelling before the headers is
// charged the estimate, timed from the write; it is not free.
func TestCancelBeforeHeadersAfterTheRequestWasWrittenIsCharged(t *testing.T) {
	server, received := blockingProvider(t)
	clock := newManualClock()
	g := anthropicGatewayAt(t, server.URL, server.Client(), clock.Now)
	_, cancel, done := g.start("/v1/messages", nonStreamingRequest)
	<-received
	clock.Advance(4 * time.Second)
	cancel()
	finished(t, done)
	e := g.settledDenied(t, "estimated", "client_cancelled", inputEstimate(nonStreamingRequest)+1000)
	if !e.Allowed || e.DenyReason != "" {
		t.Fatalf("a harness cancel was audited as a provider failure: %+v", e)
	}
}

// The provider's header wait timing out after the write is charged too.
func TestHeaderTimeoutAfterTheRequestWasWrittenIsCharged(t *testing.T) {
	server, _ := blockingProvider(t)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	g := anthropicGatewayAt(t, server.URL, &http.Client{Transport: transport}, fixedClock())
	_, cancel, done := g.start("/v1/messages", nonStreamingRequest)
	defer cancel()
	finished(t, done)
	if e := g.settledDenied(t, "estimated", "upstream_error", inputEstimate(nonStreamingRequest)); e.Allowed {
		t.Fatalf("a provider timeout was not reported as a failure: %+v", e)
	}
}

// A provider that was never reached did no work: the reservation is released.
func TestUnreachedProviderReleasesTheReservation(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	g := anthropicGatewayAt(t, server.URL, server.Client(), fixedClock())
	_, cancel, done := g.start("/v1/messages", nonStreamingRequest)
	defer cancel()
	finished(t, done)
	if state := g.state(t); state.UnitsUsed != 0 || state.ReservedUnits != 0 || g.audit.events[0].UsageSource != "" {
		t.Fatalf("state=%+v audit=%+v", state, g.audit.events[0])
	}
}

// settledDenied checks a call that ended in an error reply but was charged.
func (g *streamGateway) settledDenied(t *testing.T, source, reason string, units int64) audit.Event {
	t.Helper()
	if len(g.audit.events) != 1 {
		t.Fatalf("audit events = %+v, want one", g.audit.events)
	}
	e, state := g.audit.events[0], g.state(t)
	if e.UsageSource != source || e.EndReason != reason || e.TokensUsed != units || state.UnitsUsed != units || state.ReservedUnits != 0 {
		t.Fatalf("audit=%+v state=%+v, want %s %s %d units", e, state, source, reason, units)
	}
	return e
}

// After message_delta reports exact output, an interrupt charges it rather
// than the time floor.
func TestAnthropicCancelAfterMessageDeltaChargesExactOutput(t *testing.T) {
	provider, client := newPipeProvider()
	clock := newManualClock()
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, clock.Now)
	recorder, cancel, done := g.start("/v1/messages", anthropicStreamRequest)
	events := fixtureEvents(t, "anthropic-thinking-tool-use.sse")
	provider.send(t, events[0], events[7], events[8], events[len(events)-2])
	recorder.waitFor(t, "message_delta")
	clock.Advance(10 * time.Second) // a time floor of 2500, capped at max_tokens 1000
	cancel()
	finished(t, done)
	g.settled(t, "estimated", "client_cancelled", 160+58)
}

// Work the response does not carry or bound is refused before inference.
func TestDetachedWorkIsRefused(t *testing.T) {
	nMessage := func(key string) string {
		return key + " must be 1 (or omitted): the model gateway bounds one output per request; send one request per output"
	}
	background := "background responses are not supported by the model gateway; background must be omitted or the boolean false"
	for _, tc := range []struct{ path, body, message string }{
		{testResponsesPath, `{"model":"test","background":true,"input":[]}`, background},
		{testResponsesPath, `{"model":"test","background":"false","input":[]}`, background},
		{testChatCompletionsPath, `{"model":"test","n":128,"max_tokens":1000,"messages":[]}`, nMessage("n")},
		{testChatCompletionsPath, `{"model":"test","n":0,"messages":[]}`, nMessage("n")},
		{testChatCompletionsPath, `{"model":"test","n":"1","messages":[]}`, nMessage("n")},
		{testChatCompletionsPath, `{"model":"test","best_of":2,"messages":[]}`, nMessage("best_of")},
		{testChatCompletionsPath, `{"model":"test","messages":[]} {}`,
			"request body must be exactly one JSON value; remove the data after it"},
	} {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return modelResponse(r, `{"usage":{"total_tokens":1}}`), nil
		})}
		g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
		recorder, cancel, done := g.start(tc.path, tc.body)
		finished(t, done)
		cancel()
		if recorder.status != http.StatusBadRequest || calls != 0 || !strings.Contains(recorder.String(), `"message":"`+tc.message+`"`) {
			t.Fatalf("%s: status=%d calls=%d body=%s", tc.body, recorder.status, calls, recorder.String())
		}
	}
	for _, allowed := range []string{`{"model":"test","background":false,"n":1,"messages":[]}`, `{"model":"test","n":null,"messages":[]}`,
		`{"model":"test","n":1.0,"messages":[]}`, `{"model":"test","n":1e0,"best_of":10e-1,"messages":[]}`} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return modelResponse(r, `{"usage":{"total_tokens":1}}`), nil
		})}
		g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
		recorder, cancel, done := g.start(testChatCompletionsPath, allowed)
		finished(t, done)
		cancel()
		if recorder.status != http.StatusOK {
			t.Fatalf("%s refused: %s", allowed, recorder.String())
		}
	}
}

// A call that does not stream sends its headers only once generation has
// finished; a harness that drops between the headers and the body still pays
// for the time the provider worked, timed from the request write.
func TestNonStreamingCallIsTimedFromTheWrite(t *testing.T) {
	received, headers := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(received)
		<-headers
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	clock := newManualClock()
	g := anthropicGatewayAt(t, server.URL, server.Client(), clock.Now)
	recorder, cancel, done := g.start("/v1/messages", nonStreamingRequest)
	<-received
	clock.Advance(120 * time.Second)
	close(headers)
	select {
	case <-recorder.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("headers were not relayed")
	}
	cancel()
	finished(t, done)
	g.settled(t, "estimated", "client_cancelled", inputEstimate(nonStreamingRequest)+30_000)
}

// A JSON body that arrived whole settles from its usage even if the harness
// leaves before the provider closes the response.
func TestWholeJSONBodySettlesFromUsageAfterCancel(t *testing.T) {
	provider, client := newPipeProvider()
	g := anthropicGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	recorder, cancel, done := g.start("/v1/messages", `{"model":"claude-fixture","max_tokens":100,"messages":[]}`)
	provider.send(t, anthropicSuccess)
	recorder.waitFor(t, `"end_turn"`)
	cancel()
	finished(t, done)
	g.settled(t, "provider", "client_cancelled", 22)
}

// A stopping gateway ends its model calls as interrupted, settles each one,
// and refuses new calls.
func TestDrainSettlesCallsInFlight(t *testing.T) {
	provider, client := newPipeProvider()
	drain := model.NewDrain()
	g := &streamGateway{tracker: budget.NewTracker(), audit: &captureWriter{}, logs: &bytes.Buffer{}, account: anthropicAccount}
	h := model.NewAnthropicHandler(model.HandlerConfig{ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret},
		UpstreamURL: testModelUpstream, HTTPClient: client, AuditLogger: g.audit, Now: fixedClock(), Drain: drain,
		Logger: slog.New(slog.NewTextHandler(g.logs, nil))})
	identity := testExecutionGrant()
	routes := gateway.Dispatcher{model.AnthropicAuthority: {Capability: string(api.CapabilityModelInference), Handler: h, Budgets: g.tracker}}
	g.serve = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), identity)))
	})
	recorder, cancel, done := g.start("/v1/messages", anthropicStreamRequest)
	defer cancel()
	events := fixtureEvents(t, "anthropic-thinking-tool-use.sse")
	provider.send(t, events[0])
	recorder.waitFor(t, "message_start")
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := drain.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	finished(t, done)
	g.settled(t, "estimated", "gateway_restart", 160)
	late, cancelLate, lateDone := g.start("/v1/messages", anthropicStreamRequest)
	defer cancelLate()
	finished(t, lateDone)
	if late.status != http.StatusServiceUnavailable || !strings.Contains(late.String(), "the model gateway is shutting down; retry the call") {
		t.Fatalf("call after shutdown: %d %s", late.status, late.String())
	}
}

// A harness that leaves while its call waits for the budget is audited as
// client_cancelled, not as a refusal; it still gets the 408 if it listens.
func TestCancelWhileWaitingForTheBudgetIsAuditedHonestly(t *testing.T) {
	provider, client := newPipeProvider()
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	_, cancelFirst, firstDone := g.start(testResponsesPath, responsesStreamRequest)
	<-provider.requests
	waiting, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	time.Sleep(20 * time.Millisecond)
	cancel()
	finished(t, done)
	e := g.audit.events[0]
	if waiting.status != http.StatusRequestTimeout || !e.Allowed || e.DenyReason != "" || e.EndReason != "client_cancelled" ||
		!strings.Contains(waiting.String(), "model request cancelled while waiting for its budget") {
		t.Fatalf("status=%d audit=%+v body=%s", waiting.status, e, waiting.String())
	}
	cancelFirst()
	finished(t, firstDone)
}

// A harness that leaves before the provider has the whole request releases
// the reservation, and the audit says it was cancelled, not a provider failure.
func TestCancelBeforeTheWriteReleasesAndIsAuditedAsCancelled(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
	_, cancel, done := g.start(testResponsesPath, responsesStreamRequest)
	<-started
	cancel()
	finished(t, done)
	e, state := g.audit.events[0], g.state(t)
	if !e.Allowed || e.DenyReason != "" || e.EndReason != "client_cancelled" || state.UnitsUsed != 0 || state.ReservedUnits != 0 {
		t.Fatalf("audit=%+v state=%+v", e, state)
	}
}

// A refusal before inference names its cause: missing pricing or an
// unsupported operation.
func TestRefusalCodesNameTheirCause(t *testing.T) {
	for body, code := range map[string]string{
		`{"model":"unpriced","input":[]}`:                       "pricing_unavailable",
		`{"model":"test","service_tier":"priority","input":[]}`: "unsupported_operation",
	} {
		g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000, MaxCostMicros: 1000}, http.DefaultClient, fixedClock())
		g.serve.(*identityHandler).Config.Pricing = &model.PricingTable{Models: map[string]model.ModelPrice{"test": {
			InputMicrosPerMillionTokens: 1, CachedInputMicrosPerMillionTokens: 1, OutputMicrosPerMillionTokens: 1}}}
		recorder, cancel, done := g.start(testResponsesPath, body)
		finished(t, done)
		cancel()
		if recorder.status != http.StatusForbidden || !strings.Contains(recorder.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s: %d %s", body, recorder.status, recorder.String())
		}
	}
}

// At the cap on pending recoverable reservations, a call is told to retry,
// not that its budget is exhausted.
func TestTooManyPendingCallsIsRetryable(t *testing.T) {
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 1_000_000}, http.DefaultClient, fixedClock())
	meter := g.tracker.Meter(testRunUID, api.EndpointBudget{MaxUnits: 1_000_000})
	for range 64 {
		if _, _, err := meter.ReserveRequest(t.Context(), 10, 0, true,
			&budget.Recovery{ExpiresAt: time.Now().Add(time.Hour), Units: 10}); err != nil {
			t.Fatal(err)
		}
	}
	recorder, cancel, done := g.start(testResponsesPath, `{"model":"test","max_output_tokens":10,"input":[]}`)
	defer cancel()
	finished(t, done)
	if recorder.status != http.StatusTooManyRequests || !strings.Contains(recorder.String(), `"code":"too_many_pending_calls"`) ||
		!strings.Contains(recorder.String(), "too many model calls are pending on this budget; retry when one finishes") {
		t.Fatalf("%d %s", recorder.status, recorder.String())
	}
}

// A drain answers a call waiting for its headers, or for its budget, with
// the retryable 503 rather than a provider failure or a timeout.
func TestDrainRepliesGatewayStopping(t *testing.T) {
	server, received := blockingProvider(t)
	drain := model.NewDrain()
	g := anthropicGatewayAt(t, server.URL, server.Client(), fixedClock(), drain)
	unbounded := `{"model":"claude-fixture","max_tokens":64000,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]}]}`
	first, cancelFirst, firstDone := g.start("/v1/messages", unbounded)
	defer cancelFirst()
	<-received
	waiting, cancelWaiting, waitingDone := g.start("/v1/messages", nonStreamingRequest)
	defer cancelWaiting()
	time.Sleep(20 * time.Millisecond)
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := drain.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	finished(t, firstDone)
	finished(t, waitingDone)
	for name, recorder := range map[string]*streamRecorder{"waiting for headers": first, "waiting for the budget": waiting} {
		if recorder.status != http.StatusServiceUnavailable || !strings.Contains(recorder.String(), "the model gateway is shutting down; retry the call") {
			t.Fatalf("%s: %d %s", name, recorder.status, recorder.String())
		}
	}
}

// Each body the JSON check refuses names its own fault; ambiguous_json means
// only duplicate keys.
func TestJSONRefusalsNameTheirFault(t *testing.T) {
	for body, want := range map[string][2]string{
		`{"model":"test","n":1e1000000000,"messages":[]}`: {"number_too_large", "request body contains a number too large to read"},
		`{"model":"test","model":"other","messages":[]}`:  {"ambiguous_json", "request body must be one JSON value without duplicate keys"},
		`{"model":"test","Model":"other","messages":[]}`:  {"ambiguous_json", "keys of the request, its messages and their content blocks are compared without case"},
		`{"model":"test","messages":[}`:                   {invalidJSONCode, "request body is not valid JSON"},
		`{"model":"test","messages":[]} {}`:               {"trailing_data", "request body must be exactly one JSON value; remove the data after it"},
	} {
		g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, http.DefaultClient, fixedClock())
		recorder, cancel, done := g.start(testChatCompletionsPath, body)
		finished(t, done)
		cancel()
		if recorder.status != http.StatusBadRequest || !strings.Contains(recorder.String(), `"code":"`+want[0]+`"`) ||
			!strings.Contains(recorder.String(), want[1]) {
			t.Fatalf("%s: %d %s", body, recorder.status, recorder.String())
		}
	}
}

// settleFailingLedger refuses every settlement with err.
type settleFailingLedger struct {
	budget.Ledger
	err error
}

func (l settleFailingLedger) Settle(context.Context, *budget.LedgerReservation, budget.TrustedUsage) error {
	return l.err
}

// A settlement the ledger refuses is audited as unsettled, or as recovered
// when the lease was already recovered, and is never logged as charged.
func TestFailedSettlementIsNotAuditedAsCharged(t *testing.T) {
	for _, tc := range []struct {
		name, body, source, log string
		err                     error
	}{
		{"provider usage", `{"usage":{"total_tokens":7}}`, "unsettled", "Failed to settle model reservation", errors.New("ledger down")},
		{"estimate", `{"id":"no usage"}`, "unsettled", "Failed to settle model reservation", errors.New("ledger down")},
		{"recovered", `{"usage":{"total_tokens":7}}`, "recovered", "Model reservation was already recovered", budget.ErrReservationRecovered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return modelResponse(r, tc.body), nil
			})}
			g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
			h := g.serve.(*identityHandler)
			h.Config.BudgetTracker = budget.NewTrackerWithLedger(settleFailingLedger{Ledger: budget.NewMemoryLedger(), err: tc.err})
			_, cancel, done := g.start(testChatCompletionsPath, `{"model":"test","messages":[]}`)
			defer cancel()
			finished(t, done)
			e := g.audit.events[0]
			if e.UsageSource != tc.source || e.TokensUsed != 0 || !strings.Contains(g.logs.String(), tc.log) ||
				strings.Contains(g.logs.String(), "Charged") {
				t.Fatalf("audit=%+v logs=%s", e, g.logs.String())
			}
		})
	}
}

// A body nested deeper than encoding/json decodes is refused as invalid JSON,
// so no check that parses it can skip a field: at 10,000 levels the stream
// check still applies, and beyond it nothing reaches the provider.
func TestDeeplyNestedBodiesFailClosed(t *testing.T) {
	for depth, code := range map[int]string{10_000: "streaming_not_supported", 10_001: invalidJSONCode, 10_002: invalidJSONCode} {
		inner := depth - 1 // the outer object is one level
		body := `{"model":"test","n":5,"background":true,"stream":true,"messages":[],"x":` +
			strings.Repeat("[", inner) + strings.Repeat("]", inner) + `}`
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return modelResponse(r, `{"usage":{"total_tokens":1}}`), nil
		})}
		g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, client, fixedClock())
		recorder, cancel, done := g.start(testChatCompletionsPath, body)
		finished(t, done)
		cancel()
		if recorder.status < 400 || recorder.status >= 500 || calls != 0 || !strings.Contains(recorder.String(), `"code":"`+code+`"`) {
			t.Fatalf("depth %d: status=%d calls=%d body=%.200s", depth, recorder.status, calls, recorder.String())
		}
	}
}

// A field of the wrong type is refused rather than read as absent.
func TestWrongTypedStreamIsRefused(t *testing.T) {
	g := responsesGateway(t, api.EndpointBudget{MaxUnits: 10_000}, http.DefaultClient, fixedClock())
	recorder, cancel, done := g.start(testChatCompletionsPath, `{"model":"test","stream":"yes","messages":[]}`)
	defer cancel()
	finished(t, done)
	if recorder.status != http.StatusBadRequest || !strings.Contains(recorder.String(), "request body field stream must be a boolean") {
		t.Fatalf("%d %s", recorder.status, recorder.String())
	}
}

const invalidJSONCode = "invalid_json"
