package model_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// Paths, headers and bodies several record cases share.
const (
	testMessagesPath = "/v1/messages"
	testModelsPath   = "/v1/models"
	contentEncoding  = "Content-Encoding"
	gzipEncoding     = "gzip"
	trailingDataBody = `{"model":"test","messages":[]} {}`
	// An image by URL leaves the call unsizable, so it reserves all that remains.
	unsizableAnthropicRequest = `{"model":"claude-fixture","max_tokens":64000,"messages":[{"role":"user",` +
		`"content":[{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]}]}`
)

var updateRecords = flag.Bool("update-records", false,
	"rewrite testdata/outcomes.txt and testdata/relay-outcomes.txt from the current code")

// compareRecorded compares lines with the recorded file, or rewrites it under
// -update-records.
func compareRecorded(t *testing.T, file string, lines []string) {
	t.Helper()
	got := strings.Join(lines, "\n") + "\n"
	if *updateRecords {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d lines, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("outcome changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

// recordedEvents keeps every audit event a route or handler writes.
type recordedEvents struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordedEvents) Log(e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordedEvents) taken() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// outcomeLine is one recorded case: status, the headers that matter, the
// body, the ledger before and after, and each audit event as sorted-key JSON
// without its timestamp. A body over 2 KiB is recorded by its digest.
func outcomeLine(t *testing.T, name string, status int, header http.Header, body, ledger string, events []audit.Event) string {
	t.Helper()
	var headers []string
	for _, h := range []string{"Content-Type", "X-Content-Type-Options", "Cache-Control", "Connection"} {
		if v := header.Get(h); v != "" {
			headers = append(headers, strings.ToLower(h)+"="+v)
		}
	}
	recorded := make([]string, 0, len(events))
	for _, e := range events {
		recorded = append(recorded, eventJSON(t, e))
	}
	if len(recorded) == 0 {
		recorded = append(recorded, "none")
	}
	quoted := strconv.Quote(body)
	if len(body) > 2048 {
		quoted = fmt.Sprintf("sha256:%x (%d bytes)", sha256.Sum256([]byte(body)), len(body))
	}
	code := "none"
	if status != 0 {
		code = strconv.Itoa(status)
	}
	return strings.Join([]string{name, code, strings.Join(headers, " "), quoted, ledger,
		strings.Join(recorded, " ; ")}, " | ")
}

func eventJSON(t *testing.T, e audit.Event) string {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "timestamp")
	sorted, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(sorted)
}

var errLedgerUnavailable = errors.New("ledger store unavailable")

// failingLedger refuses every read and write, as an unavailable store does.
type failingLedger struct{}

func (failingLedger) Initialize(context.Context, budget.AccountKey) error {
	return errLedgerUnavailable
}
func (failingLedger) Reserve(context.Context, budget.AccountKey, budget.UsageReservation) (*budget.LedgerReservation, error) {
	return nil, errLedgerUnavailable
}
func (failingLedger) Settle(context.Context, *budget.LedgerReservation, budget.TrustedUsage) error {
	return errLedgerUnavailable
}
func (failingLedger) Release(context.Context, *budget.LedgerReservation) error {
	return errLedgerUnavailable
}
func (failingLedger) State(context.Context, budget.AccountKey) (budget.State, error) {
	return budget.State{}, errLedgerUnavailable
}
func (failingLedger) Seal(context.Context, budget.AccountKey) (budget.TrustedUsage, error) {
	return budget.TrustedUsage{}, errLedgerUnavailable
}

// releaseFailingLedger records everything but refuses to release.
type releaseFailingLedger struct{ budget.Ledger }

func (releaseFailingLedger) Release(context.Context, *budget.LedgerReservation) error {
	return errLedgerUnavailable
}

const recordAccount = testRunUID + "/model.inference"

func ledgerReading(tracker *budget.Tracker) string {
	state, err := tracker.State(recordAccount)
	if err != nil {
		return "error"
	}
	return fmt.Sprintf("used=%d reserved=%d cost=%d", state.UnitsUsed, state.ReservedUnits, state.CostMicros)
}

// outcomeWriter is the workload's side of the connection; it can refuse
// writes or flushes, as a workload that left does.
type outcomeWriter struct {
	mu        sync.Mutex
	header    http.Header
	status    int
	body      bytes.Buffer
	failWrite bool
	failFlush bool
	// failAfter, when positive, fails every write that starts once the body
	// holds this many bytes, as a workload that leaves mid-response does.
	failAfter int
	flushed   chan struct{}
}

func newOutcomeWriter() *outcomeWriter {
	return &outcomeWriter{header: http.Header{}, flushed: make(chan struct{}, 1)}
}

func (w *outcomeWriter) Header() http.Header { return w.header }
func (w *outcomeWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}
func (w *outcomeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.failWrite || w.failAfter > 0 && w.body.Len() >= w.failAfter {
		return 0, errors.New("workload connection closed")
	}
	return w.body.Write(p)
}
func (w *outcomeWriter) FlushError() error {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
	if w.failFlush {
		return errors.New("workload connection closed")
	}
	return nil
}
func (w *outcomeWriter) Flush() { _ = w.FlushError() }
func (w *outcomeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}
func (w *outcomeWriter) code() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *outcomeWriter) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(w.String(), want) {
		select {
		case <-w.flushed:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("relayed body %q never contained %q", w.String(), want)
		}
	}
}

// caseRun is one recorded model request and what its case drives around it.
type caseRun struct {
	t       *testing.T
	tracker *budget.Tracker
	clock   *manualClock
	drain   *model.Drain
	// blockerDrain stops the blocker apart from the case's own call, so a
	// case decides which of the two stops first.
	blockerDrain *model.Drain
	cancel       context.CancelFunc
	writer       *outcomeWriter
	config       model.HandlerConfig
	identity     *gateway.ExecutionGrant
	cleanups     []func()
}

// blocker starts another call on the same account, through its own handler
// and audit writer, that holds all remaining capacity until the case ends.
// Under a drain the blocker has its own, which shutdownBlocker stops: were
// both calls in one drain, the blocker could release its reservation before
// the case's call saw the stop, and the case would be admitted instead.
func (run *caseRun) blocker(anthropic bool) {
	started := make(chan struct{})
	var once sync.Once
	config := run.config
	config.AuditLogger = audit.NewLogger(io.Discard)
	if run.drain != nil {
		run.blockerDrain = model.NewDrain()
		config.Drain = run.blockerDrain
	}
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	// The blocker holds its reservation until the case ends, whatever call
	// deadline the case gives its own request.
	identity := run.identity.Clone()
	identity.Policy.Spec.MaxModelCallSeconds = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveModel(anthropic, config, run.tracker, identity, newOutcomeWriter(),
			httptest.NewRequestWithContext(ctx, http.MethodPost, target(anthropic, ""), strings.NewReader(unboundedBody(anthropic))))
	}()
	<-started
	run.cleanups = append(run.cleanups, func() { cancel(); <-done })
}

func unboundedBody(anthropic bool) string {
	if anthropic {
		return unsizableAnthropicRequest
	}
	return `{"model":"test","input":[]}`
}

func target(anthropic bool, path string) string {
	if anthropic {
		if path == "" {
			path = testMessagesPath
		}
		return "https://api.anthropic.com" + path
	}
	if path == "" {
		path = testResponsesPath
	}
	return "https://api.openai.com" + path
}

// serveModel routes one request as the compositions do: the route carries
// model.inference, its tier, the tracker and the same audit writer as the
// handler.
func serveModel(anthropic bool, config model.HandlerConfig, tracker *budget.Tracker,
	identity *gateway.ExecutionGrant, w http.ResponseWriter, r *http.Request,
) {
	if identity != nil {
		r = r.WithContext(gateway.WithIdentity(r.Context(), identity))
	}
	var handler http.Handler = &model.Handler{Config: config}
	authority := model.OpenAIAuthority
	if anthropic {
		handler, authority = model.NewAnthropicHandler(config), model.AnthropicAuthority
	}
	routes := gateway.Dispatcher{authority: {Capability: api.CapabilityModelInference, Tier: audit.TierSemantic,
		Handler: handler, Budgets: tracker, AuditLogger: config.AuditLogger}}
	routes.ServeHTTP(w, r)
}

type modelCase struct {
	name      string
	anthropic bool
	method    string
	path      string
	body      string
	header    map[string]string
	noBudget  bool
	// noIdentity sends the request without a gateway identity.
	noIdentity bool
	limits     *api.EndpointBudget
	grant      func(*gateway.ExecutionGrant)
	ledger     func() budget.Ledger
	useDrain   bool
	failWrite  bool
	failFlush  bool
	failAfter  int
	// bubble runs the case in a synctest bubble: its timers run on the
	// bubble's clock and drive can wait until every goroutine is blocked.
	bubble bool
	// client answers the provider call; nil answers 200 with okBody.
	client  func(run *caseRun) *http.Client
	config  func(run *caseRun, c *model.HandlerConfig)
	prepare func(run *caseRun)
	drive   func(run *caseRun)
}

func respond(status int, contentType, body string) func(*caseRun) *http.Client {
	return func(*caseRun) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := modelResponse(r, body)
			response.StatusCode, response.Status = status, http.StatusText(status)
			if contentType != "" {
				response.Header.Set("Content-Type", contentType)
			}
			return response, nil
		})}
	}
}

// respondWritten reports the whole request written, as the transport does
// before a provider answers, then answers status.
func respondWritten(status int, contentType, body string) func(*caseRun) *http.Client {
	return func(*caseRun) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reportWritten(r)
			response := modelResponse(r, body)
			response.StatusCode, response.Status = status, http.StatusText(status)
			response.Header.Set("Content-Type", contentType)
			return response, nil
		})}
	}
}

func reportWritten(r *http.Request) {
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
}

// waitingForever never answers until the call ends, without writing the request.
func waitingForever(*caseRun) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, context.Cause(r.Context())
	})}
}

// pipeClient answers 200 at once and relays what the case writes to the body.
func pipeClient(contentType string, body **io.PipeWriter) func(*caseRun) *http.Client {
	return func(run *caseRun) *http.Client {
		reader, writer := io.Pipe()
		*body = writer
		run.cleanups = append(run.cleanups, func() { _ = writer.Close() })
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader, Request: r}
			if contentType != "" {
				response.Header.Set("Content-Type", contentType)
			}
			return response, nil
		})}
	}
}

// failingBody returns its text, then an error.
type failingBody struct{ text *strings.Reader }

func (b failingBody) Read(p []byte) (int, error) {
	if b.text.Len() == 0 {
		return 0, errors.New("provider connection reset")
	}
	return b.text.Read(p)
}
func (failingBody) Close() error { return nil }

func respondFailing(status int, text string) func(*caseRun) *http.Client {
	return respondFailingAs(status, "application/json", text)
}

func respondFailingAs(status int, contentType, text string) func(*caseRun) *http.Client {
	return func(*caseRun) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := modelResponse(r, "")
			response.StatusCode = status
			response.Header.Set("Content-Type", contentType)
			response.Body = failingBody{strings.NewReader(text)}
			return response, nil
		})}
	}
}

// blockingServer reads the whole request, signals, then answers nothing
// until the call ends, so the request is written. It signals only once the
// handler has also seen the write: the transport reports it after the bytes
// are sent, so the server can read them first, and a case that moved the
// clock or ended the call then would see the write timed later or missed.
// The handler's first reading of the clock is that report.
func blockingServer(received chan<- struct{}, timeout time.Duration) func(*caseRun) *http.Client {
	return func(run *caseRun) *http.Client {
		wrote := make(chan struct{})
		var once sync.Once
		now := run.config.Now
		run.config.Now = func() time.Time {
			at := now()
			once.Do(func() { close(wrote) })
			return at
		}
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			select {
			case <-wrote:
			case <-r.Context().Done():
				return
			}
			if received != nil {
				received <- struct{}{}
			}
			<-r.Context().Done()
		}))
		run.cleanups = append(run.cleanups, server.Close)
		run.config.UpstreamURL = server.URL
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = timeout
		return &http.Client{Transport: transport}
	}
}

func shutdown(run *caseRun) { stopDrain(run.t, run.drain) }

// shutdownBlocker stops the blocker and waits until it has settled.
func shutdownBlocker(run *caseRun) { stopDrain(run.t, run.blockerDrain) }

func stopDrain(t *testing.T, drain *model.Drain) {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := drain.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func oneSecondCall(g *gateway.ExecutionGrant) {
	one := int32(1)
	g.Policy.Spec.MaxModelCallSeconds = &one
}

func budgetOf(units, cost int64) *api.EndpointBudget {
	return &api.EndpointBudget{MaxUnits: units, MaxCostMicros: cost}
}

func gzipped(text string) string {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = io.WriteString(w, text)
	_ = w.Close()
	return b.String()
}

const (
	chatPath       = testChatCompletionsPath
	chatBody       = `{"model":"test","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	chatSuccess    = `{"id":"chatcmpl-1","usage":{"total_tokens":7,"prompt_tokens":5,"completion_tokens":2}}`
	messagesBody   = `{"model":"claude-fixture","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	chatGPTRequest = `{"model":"gpt-5.3-codex","store":false,"stream":true,"input":[]}`
)

func runModelCase(t *testing.T, tc modelCase) string {
	t.Helper()
	if tc.bubble {
		var line string
		synctest.Test(t, func(t *testing.T) {
			bubbled := tc
			bubbled.bubble = false
			line = runModelCase(t, bubbled)
		})
		return line
	}
	run := &caseRun{t: t, clock: newManualClock()}
	defer func() {
		for _, v := range slices.Backward(run.cleanups) {
			v()
		}
	}()
	var ledger budget.Ledger = budget.NewMemoryLedger()
	if tc.ledger != nil {
		ledger = tc.ledger()
	}
	run.tracker = budget.NewTrackerWithLedger(ledger)
	if tc.useDrain {
		run.drain = model.NewDrain()
	}
	run.identity = testExecutionGrant()
	if tc.noBudget {
		delete(run.identity.Policy.Spec.Budgets, api.CapabilityModelInference)
	}
	if tc.limits != nil {
		run.identity.Policy.Spec.Budgets[api.CapabilityModelInference] = *tc.limits
	}
	if tc.grant != nil {
		tc.grant(run.identity)
	}
	events := &recordedEvents{}
	run.config = model.HandlerConfig{AuditLogger: events, UpstreamURL: testModelUpstream,
		ProviderAuth: modelauth.APIKey{Key: "sk-fixture-openai-key-0123"},
		Logger:       slog.New(slog.DiscardHandler), Now: run.clock.Now, Drain: run.drain}
	if tc.anthropic {
		run.config.ProviderAuth = modelauth.AnthropicAPIKey{Key: testAnthropicSecret}
		run.config.Pricing = anthropicFixturePricing()
	}
	client := tc.client
	if client == nil {
		okBody := chatSuccess
		if tc.anthropic {
			okBody = anthropicSuccess
		}
		client = respond(http.StatusOK, "application/json", okBody)
	}
	run.config.HTTPClient = client(run)
	if tc.config != nil {
		tc.config(run, &run.config)
	}
	if tc.prepare != nil {
		tc.prepare(run)
	}
	before := ledgerReading(run.tracker)
	method := tc.method
	if method == "" {
		method = http.MethodPost
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run.cancel = cancel
	req := httptest.NewRequestWithContext(ctx, method, target(tc.anthropic, tc.path), strings.NewReader(tc.body))
	for k, v := range tc.header {
		req.Header.Set(k, v)
	}
	run.writer = newOutcomeWriter()
	run.writer.failWrite, run.writer.failFlush, run.writer.failAfter = tc.failWrite, tc.failFlush, tc.failAfter
	done := make(chan struct{})
	identity := run.identity
	if tc.noIdentity {
		identity = nil
	}
	go func() {
		defer close(done)
		serveModel(tc.anthropic, run.config, run.tracker, identity, run.writer, req)
	}()
	if tc.drive != nil {
		tc.drive(run)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: the handler did not finish", tc.name)
	}
	recorded := events.taken()
	after := ledgerReading(run.tracker)
	return outcomeLine(t, tc.name, run.writer.code(), run.writer.header, run.writer.String(), before+" -> "+after, recorded)
}

// TestModelOutcomesAreRecorded records what the model handler replies,
// charges and audits for each way a call ends, before and after admission,
// for both provider protocols.
func TestModelOutcomesAreRecorded(t *testing.T) {
	anthropicEvents := fixtureEvents(t, "anthropic-thinking-tool-use.sse")
	responsesEvents := fixtureEvents(t, "responses-reasoning.sse")
	anthropicStream := strings.Join(anthropicEvents, "")
	responsesStream := strings.Join(responsesEvents, "")
	anthropicFailure := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	responsesFailed := strings.Replace(strings.Replace(responsesEvents[len(responsesEvents)-1], "response.completed", "response.failed", 2),
		`"status":"completed"`, `"status":"failed"`, 1)
	responsesNoUsage := `event: response.completed` + "\n" +
		`data: {"type":"response.completed","sequence_number":6,"response":{"id":"resp_fixture","object":"response","status":"completed","usage":null}}` + "\n\n"
	largeEvent := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("w", 8<<20) + `"}` + "\n\n"
	largeJSON := `{"id":"chatcmpl-1","pad":"` + strings.Repeat("p", 8<<20) + `","usage":{"total_tokens":7,"prompt_tokens":5,"completion_tokens":2}}`
	unsizable := unsizableAnthropicRequest
	responsesTerminal := responsesEvents[len(responsesEvents)-1]
	anthropicStop := anthropicEvents[len(anthropicEvents)-1]
	// A comment longer than one read, so what follows a terminal event
	// arrives both with it and in later reads.
	padding := ": " + strings.Repeat("k", 64<<10) + "\n\n"
	refreshFailed := func(_ *caseRun, c *model.HandlerConfig) {
		c.ProviderAuth = failingChatGPT{fmt.Errorf("wrapped: %w", &modelauth.RefreshError{Refused: true})}
	}
	releaseFails := func() budget.Ledger { return releaseFailingLedger{budget.NewMemoryLedger()} }
	settleFails := func() budget.Ledger {
		return settleFailingLedger{Ledger: budget.NewMemoryLedger(), err: errors.New("ledger down")}
	}
	priced := func(_ *caseRun, c *model.HandlerConfig) {
		c.Pricing = &model.PricingTable{Models: map[string]model.ModelPrice{"test": {
			InputMicrosPerMillionTokens: 1, CachedInputMicrosPerMillionTokens: 1, OutputMicrosPerMillionTokens: 1}}}
	}
	var pipe *io.PipeWriter
	send := func(run *caseRun, events ...string) {
		for _, event := range events {
			if _, err := io.WriteString(pipe, event); err != nil {
				run.t.Fatal(err)
			}
		}
	}
	received := make(chan struct{}, 1)

	cases := []modelCase{
		// Pre-admission (inventory section 3.8).
		{name: "pre/no identity is refused by the dispatcher (handler 401 unreachable)", path: chatPath, body: chatBody,
			noIdentity: true},
		{name: "pre/managed allocation exhausted is refused by the dispatcher (handler 403 unreachable)", path: chatPath, body: chatBody,
			grant: func(g *gateway.ExecutionGrant) {
				g.Managed = true
				g.Allocations = map[api.CapabilityKind]gateway.Allocation{api.CapabilityModelInference: {AccountKey: recordAccount,
					Ceiling: api.EndpointBudget{MaxUnits: 10}, Exhausted: true}}
			}},
		{name: "pre/openai operation not allowed", method: http.MethodGet, path: "/v1/files", body: ""},
		{name: "pre/openai unsupported content encoding", path: chatPath, body: chatBody, header: map[string]string{contentEncoding: "br"}},
		{name: "pre/openai undecodable gzip body", path: chatPath, body: "not gzip", header: map[string]string{contentEncoding: gzipEncoding}},
		{name: "pre/openai body over the gateway limit", path: chatPath, body: `{"x":"` + strings.Repeat("a", 1<<20) + `"}`},
		{name: "pre/anthropic body over the gateway limit", anthropic: true, body: `{"x":"` + strings.Repeat("a", 1<<20) + `"}`},
		{name: "pre/openai gzip body decoded and served", path: chatPath, body: gzipped(chatBody), header: map[string]string{contentEncoding: gzipEncoding}},
		{name: "pre/openai invalid JSON", path: chatPath, body: `{"model":"test","messages":[}`},
		{name: "pre/openai duplicate key", path: chatPath, body: `{"model":"test","model":"other","messages":[]}`},
		{name: "pre/openai trailing data", path: chatPath, body: trailingDataBody},
		{name: "pre/openai number too large", path: chatPath, body: `{"model":"test","n":1e1000000000,"messages":[]}`},
		{name: "pre/anthropic invalid JSON", anthropic: true, body: `{"model":"claude-fixture",`},
		{name: "pre/anthropic unsupported operation", anthropic: true, path: "/v1/messages/batches", body: messagesBody},
		{name: "pre/anthropic pricing unavailable under a cost cap", anthropic: true, limits: budgetOf(10_000, 1_000_000),
			body: `{"model":"claude-unpriced","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "pre/openai stream must be a boolean", path: chatPath, body: `{"model":"test","stream":"yes","messages":[]}`},
		{name: "pre/openai chat streaming not supported", path: chatPath, body: `{"model":"test","stream":true,"messages":[]}`},
		{name: "pre/openai detached work", path: testResponsesPath, body: `{"model":"test","background":true,"input":[]}`},
		{name: "pre/openai provider auth absent", path: chatPath, body: chatBody,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = nil }},
		{name: "pre/anthropic workload credential missing", anthropic: true, body: messagesBody,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = modelauth.Workload{} }},
		{name: "pre/openai chatgpt request not supported", path: chatPath, body: chatBody,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = failingChatGPT{errors.New("unused")} }},
		{name: "pre/openai chatgpt request refused under a cost cap", path: testResponsesPath, limits: budgetOf(10_000, 1000),
			body:   `{"model":"test","stream":true,"store":false,"input":[]}`,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = failingChatGPT{errors.New("unused")} }},
		{name: "pre/openai pricing unavailable under a cost cap", path: chatPath, limits: budgetOf(10_000, 1000), config: priced,
			body: `{"model":"unpriced","max_tokens":10,"messages":[]}`},
		{name: "pre/openai unsupported option under a cost cap", path: testResponsesPath, limits: budgetOf(10_000, 1000), config: priced,
			body: `{"model":"test","service_tier":"priority","input":[]}`},
		{name: "pre/openai model discovery forwarded without a reservation", method: http.MethodGet, path: testModelsPath,
			client: respond(http.StatusOK, "application/json", `{"data":[{"id":"test"}]}`)},
		{name: "pre/openai model discovery with chatgpt sign-in that fails", method: http.MethodGet, path: testModelsPath,
			config: func(_ *caseRun, c *model.HandlerConfig) {
				c.ProviderAuth = failingChatGPT{errors.New("refresh failed")}
			}},
		{name: "pre/anthropic drain closed", anthropic: true, body: messagesBody, useDrain: true,
			prepare: func(run *caseRun) { shutdown(run) }},

		// Reservation (rows 1-4).
		{name: "row 1/openai cancelled while waiting for the budget", path: testResponsesPath, body: chatBody,
			prepare: func(run *caseRun) { run.blocker(false) },
			drive:   func(run *caseRun) { time.Sleep(50 * time.Millisecond); run.cancel() }},
		{name: "row 1/anthropic deadline while waiting for the budget", anthropic: true, body: messagesBody, grant: oneSecondCall,
			prepare: func(run *caseRun) { run.blocker(true) }},
		{name: "row 2/anthropic gateway stopping while waiting for the budget", anthropic: true, body: messagesBody, useDrain: true,
			bubble:  true,
			prepare: func(run *caseRun) { run.blocker(true) },
			// Stop the call once it waits for the blocker, then the blocker.
			drive: func(run *caseRun) { synctest.Wait(); shutdown(run); shutdownBlocker(run) }},
		{name: "row 3/openai too many pending calls", path: testResponsesPath, body: `{"model":"test","max_output_tokens":10,"input":[]}`,
			limits: budgetOf(1_000_000, 0),
			prepare: func(run *caseRun) {
				meter := run.tracker.Meter(recordAccount, api.EndpointBudget{MaxUnits: 1_000_000})
				for range 64 {
					if _, _, err := meter.ReserveRequest(t.Context(), 10, 0, true,
						&budget.Recovery{ExpiresAt: time.Now().Add(time.Hour), Units: 10}); err != nil {
						t.Fatal(err)
					}
				}
			}},
		{name: "row 4/openai budget exhausted", path: chatPath, body: chatBody, limits: budgetOf(100, 0),
			prepare: func(run *caseRun) {
				if err := run.tracker.Meter(recordAccount, api.EndpointBudget{MaxUnits: 100}).Spend(t.Context(), 100, 0); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "row 4/anthropic budget exhausted", anthropic: true, body: messagesBody, limits: budgetOf(100, 0),
			prepare: func(run *caseRun) {
				if err := run.tracker.Meter(recordAccount, api.EndpointBudget{MaxUnits: 100}).Spend(t.Context(), 100, 0); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "row 4/openai ledger unavailable reported as exhaustion", path: chatPath, body: chatBody,
			ledger: func() budget.Ledger { return failingLedger{} }},

		// Forward before the response (rows 5-12).
		{name: "row 5/openai build request failed", path: chatPath, body: chatBody,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.UpstreamURL = "http://model.invalid/\x7f" }},
		{name: "row 5/openai build request failed with a failing release (Q1)", path: chatPath, body: chatBody,
			ledger: func() budget.Ledger { return releaseFailingLedger{budget.NewMemoryLedger()} },
			config: func(_ *caseRun, c *model.HandlerConfig) { c.UpstreamURL = "http://model.invalid/\x7f" }},
		{name: "row 6/openai provider auth failed", path: chatPath, body: chatBody,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = modelauth.APIKey{} }},
		{name: "row 6/anthropic provider auth failed", anthropic: true, body: messagesBody,
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = modelauth.AnthropicAPIKey{} }},
		{name: "row 6/openai provider auth failed with a failing release (Q1)", path: chatPath, body: chatBody,
			ledger: func() budget.Ledger { return releaseFailingLedger{budget.NewMemoryLedger()} },
			config: func(_ *caseRun, c *model.HandlerConfig) { c.ProviderAuth = modelauth.APIKey{} }},
		{name: "row 6/openai chatgpt sign-in refresh failed (message names it)", path: testResponsesPath, body: chatGPTRequest,
			config: refreshFailed},
		{name: "row 7/anthropic written, workload left (estimate)", anthropic: true, body: nonStreamingRequest,
			client: blockingServer(received, 0),
			drive:  func(run *caseRun) { <-received; run.clock.Advance(4 * time.Second); run.cancel() }},
		{name: "row 7/anthropic written, workload left, unsizable (reservation)", anthropic: true, body: unsizable,
			client: blockingServer(received, 0),
			drive:  func(run *caseRun) { <-received; run.cancel() }},
		{name: "row 8/anthropic written, gateway stopping", anthropic: true, body: nonStreamingRequest, useDrain: true,
			client: blockingServer(received, 0),
			drive:  func(run *caseRun) { <-received; run.clock.Advance(2 * time.Second); shutdown(run) }},
		{name: "row 9/anthropic written, call deadline", anthropic: true, body: nonStreamingRequest, grant: oneSecondCall,
			client: blockingServer(received, 0),
			drive:  func(*caseRun) { <-received }},
		{name: "row 9/anthropic written, call deadline, settlement refused (unsettled)", anthropic: true, body: nonStreamingRequest,
			grant: oneSecondCall, ledger: settleFails, client: blockingServer(received, 0),
			drive: func(*caseRun) { <-received }},
		{name: "row 9/anthropic written, provider header timeout", anthropic: true, body: nonStreamingRequest,
			client: blockingServer(received, 50*time.Millisecond),
			drive:  func(*caseRun) { <-received }},
		{name: "row 10/openai not written, workload left", path: testResponsesPath, body: chatBody,
			client: func(run *caseRun) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					received <- struct{}{}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}
			},
			drive: func(run *caseRun) { <-received; run.cancel() }},
		{name: "row 10/openai not written, workload left, with a failing release (unsettled)", path: testResponsesPath, body: chatBody,
			ledger: releaseFails,
			client: func(run *caseRun) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					received <- struct{}{}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}
			},
			drive: func(run *caseRun) { <-received; run.cancel() }},
		{name: "row 11/anthropic not written, gateway stopping, with a failing release (unsettled)", anthropic: true, body: messagesBody,
			useDrain: true, ledger: releaseFails,
			client: func(run *caseRun) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					received <- struct{}{}
					<-r.Context().Done()
					return nil, context.Cause(r.Context())
				})}
			},
			drive: func(run *caseRun) { <-received; shutdown(run) }},
		{name: "row 11/anthropic not written, gateway stopping", anthropic: true, body: messagesBody, useDrain: true,
			client: func(run *caseRun) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					received <- struct{}{}
					<-r.Context().Done()
					return nil, context.Cause(r.Context())
				})}
			},
			drive: func(run *caseRun) { <-received; shutdown(run) }},
		{name: "row 12/anthropic not written, call deadline (empty endReason)", anthropic: true, body: messagesBody,
			grant: oneSecondCall, client: waitingForever},
		{name: "row 12/openai provider unreachable", path: chatPath, body: chatBody,
			client: func(run *caseRun) *http.Client {
				server := httptest.NewServer(http.NotFoundHandler())
				server.Close()
				run.config.UpstreamURL = server.URL
				return server.Client()
			}},
		{name: "row 12/openai provider unreachable with a failing release (Q1)", path: chatPath, body: chatBody,
			ledger: func() budget.Ledger { return releaseFailingLedger{budget.NewMemoryLedger()} },
			client: func(*caseRun) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("connection refused")
				})}
			}},

		// Provider non-2xx (rows 13-15).
		{name: "row 13/openai non-2xx body read failed", path: chatPath, body: chatBody,
			client: respondFailing(http.StatusInternalServerError, `{"error":`)},
		{name: "row 13/openai non-2xx body read failed with a failing release (Q1)", path: chatPath, body: chatBody,
			ledger: releaseFails, client: respondFailing(http.StatusInternalServerError, `{"error":`)},
		{name: "row 14/anthropic non-2xx replaced", anthropic: true, body: messagesBody,
			client: respond(http.StatusTooManyRequests, "application/json", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)},
		{name: "row 14/anthropic non-2xx with a failing release (Q1)", anthropic: true, body: messagesBody,
			ledger: func() budget.Ledger { return releaseFailingLedger{budget.NewMemoryLedger()} },
			client: respond(http.StatusTooManyRequests, "application/json", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)},
		{name: "row 15/openai non-2xx relayed", path: chatPath, body: chatBody,
			client: respond(http.StatusTooManyRequests, "application/json", `{"error":{"message":"slow down"}}`)},

		{name: "row 15/openai non-2xx relayed with a failing release (Q1)", path: chatPath, body: chatBody, ledger: releaseFails,
			client: respond(http.StatusTooManyRequests, "application/json", `{"error":{"message":"slow down"}}`)},
		{name: "row 14/anthropic written, then 429 replaced", anthropic: true, body: messagesBody,
			client: respondWritten(http.StatusTooManyRequests, "application/json", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)},
		{name: "row 14/anthropic written, then 500 replaced", anthropic: true, body: messagesBody,
			client: respondWritten(http.StatusInternalServerError, "application/json", `{"type":"error","error":{"type":"api_error","message":"boom"}}`)},
		{name: "row 15/openai written, then 429 relayed", path: chatPath, body: chatBody,
			client: respondWritten(http.StatusTooManyRequests, "application/json", `{"error":{"message":"slow down"}}`)},
		{name: "row 15/openai written, then 500 relayed", path: chatPath, body: chatBody,
			client: respondWritten(http.StatusInternalServerError, "application/json", `{"error":{"message":"boom"}}`)},
		{name: "row 15/openai written, then 429 relayed with a failing release (Q1)", path: chatPath, body: chatBody,
			ledger: releaseFails,
			client: respondWritten(http.StatusTooManyRequests, "application/json", `{"error":{"message":"slow down"}}`)},
		// The provider read the whole request, but the transport had not yet
		// reported the write when the call failed: released, not estimated.
		{name: "row 12/openai provider read the request, write not yet reported (released)", path: chatPath, body: chatBody,
			client: func(*caseRun) *http.Client {
				return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					_, _ = io.ReadAll(r.Body)
					return nil, errors.New("connection reset by peer")
				})}
			}},

		// Relay (rows 16-30).
		{name: "row 16/openai stream with the wrong content type", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, "application/json", responsesStream)},
		{name: "row 16/anthropic stream with the wrong content type", anthropic: true, body: anthropicStreamRequest,
			client: respond(http.StatusOK, "text/plain", anthropicStream)},
		{name: "row 17/anthropic first flush failed", anthropic: true, body: messagesBody, failFlush: true},
		{name: "row 18/anthropic JSON relay write failed after the whole body", anthropic: true, body: messagesBody, failWrite: true},
		{name: "row 18/openai JSON relay write failed before the whole body (estimate)", path: chatPath, body: chatBody,
			failAfter: 1, client: respond(http.StatusOK, "application/json", largeJSON[:40<<10])},
		{name: "row 18/openai JSON relay write failed past the parse bound (reservation)", path: chatPath, body: chatBody,
			failAfter: 8 << 20, client: respond(http.StatusOK, "application/json", largeJSON)},
		{name: "row 19/openai stream relay write failed", path: testResponsesPath, body: responsesStreamRequest,
			failWrite: true, client: respond(http.StatusOK, eventStream, responsesStream)},
		{name: "row 20/openai event over the parse bound, write failed", path: testResponsesPath,
			body: responsesStreamRequest, failWrite: true,
			client: respond(http.StatusOK, eventStream, largeEvent+responsesEvents[len(responsesEvents)-1])},
		{name: "row 20/openai event over the parse bound, relayed", path: testResponsesPath,
			body:   responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, responsesEvents[0]+largeEvent+responsesEvents[len(responsesEvents)-1])},
		{name: "row 20/openai event over the parse bound, a later write failed", path: testResponsesPath,
			body: responsesStreamRequest, failAfter: 8<<20 + 32<<10,
			client: respond(http.StatusOK, eventStream, responsesEvents[0]+largeEvent+responsesTerminal+padding)},
		{name: "row 21/anthropic provider error event", anthropic: true, body: anthropicStreamRequest,
			client: respond(http.StatusOK, eventStream, anthropicEvents[0]+anthropicEvents[7]+anthropicFailure)},
		{name: "row 21/openai responses error event", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, responsesEvents[0]+
				"event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n")},
		{name: "row 22/anthropic message_stop settles provider usage", anthropic: true, body: anthropicStreamRequest,
			client: respond(http.StatusOK, eventStream, anthropicStream)},
		{name: "row 22/anthropic message_stop without message_delta", anthropic: true, body: anthropicStreamRequest,
			client: respond(http.StatusOK, eventStream, strings.Join(anthropicEvents[:12], "")+anthropicEvents[len(anthropicEvents)-1])},
		{name: "row 22/anthropic message_stop write failed (provider usage)", anthropic: true, body: anthropicStreamRequest,
			failAfter: len(anthropicStream) - len(anthropicStop), client: respond(http.StatusOK, eventStream, anthropicStream)},
		{name: "row 23/openai responses completed", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, responsesStream)},
		{name: "row 23/openai responses completed without a content type", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, "", responsesStream)},
		{name: "row 23/openai responses completed, the rest relayed unparsed", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, responsesStream+padding)},
		{name: "row 23/openai terminal write failed (provider usage)", path: testResponsesPath, body: responsesStreamRequest,
			failAfter: len(responsesStream) - len(responsesTerminal), client: respond(http.StatusOK, eventStream, responsesStream)},
		{name: "row 23/openai write after the terminal event failed, same read", path: testResponsesPath,
			body: responsesStreamRequest, failAfter: len(responsesStream),
			client: respond(http.StatusOK, eventStream, responsesStream+padding)},
		{name: "row 23/openai write after the terminal event failed, later read", path: testResponsesPath,
			body: responsesStreamRequest, failAfter: 32 << 10,
			client: respond(http.StatusOK, eventStream, responsesStream+padding)},
		{name: "row 23/openai responses failed", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, strings.Join(responsesEvents[:len(responsesEvents)-1], "")+responsesFailed)},
		{name: "row 23/openai responses terminal without usage", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, strings.Join(responsesEvents[:len(responsesEvents)-1], "")+responsesNoUsage)},
		{name: "row 24/openai stream cut at its reservation", path: testResponsesPath, body: responsesStreamRequest,
			limits: budgetOf(100, 0),
			client: respond(http.StatusOK, eventStream, strings.Repeat(`event: response.output_text.delta`+"\n"+
				`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("z", 150)+`"}`+"\n\n", 3))},
		{name: "row 24/anthropic stream cut at its reservation", anthropic: true,
			body:   `{"model":"claude-fixture","max_tokens":1000,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`,
			limits: budgetOf(400, 0),
			client: respond(http.StatusOK, eventStream, anthropicEvents[0]+anthropicEvents[6]+strings.Repeat(
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\""+
					strings.Repeat("z", 400)+"\"}}\n\n", 3))},
		{name: "row 24/openai quiet stream cut at its reservation by the clock", path: testResponsesPath, body: responsesStreamRequest,
			limits: budgetOf(100, 0), bubble: true, client: pipeClient(eventStream, &pipe),
			prepare: func(run *caseRun) {
				if err := run.tracker.Meter(recordAccount, api.EndpointBudget{MaxUnits: 100}).Spend(t.Context(), 95, 0); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "row 25/openai idle timeout", path: testResponsesPath, body: responsesStreamRequest,
			client: pipeClient(eventStream, &pipe),
			config: func(_ *caseRun, c *model.HandlerConfig) { c.IdleTimeout = 50 * time.Millisecond }},
		{name: "row 25/openai idle after the terminal event (provider usage)", path: testResponsesPath, body: responsesStreamRequest,
			bubble: true, client: pipeClient(eventStream, &pipe),
			config: func(_ *caseRun, c *model.HandlerConfig) { c.IdleTimeout = 50 * time.Millisecond },
			drive:  func(run *caseRun) { send(run, responsesStream) }},
		{name: "row 26/anthropic stream, workload left", anthropic: true, body: anthropicStreamRequest,
			client: pipeClient(eventStream, &pipe),
			drive: func(run *caseRun) {
				send(run, anthropicEvents[0], anthropicEvents[7])
				run.writer.waitFor(t, "I'll list it.")
				run.clock.Advance(2 * time.Second)
				run.cancel()
			}},
		{name: "row 26/anthropic whole JSON, workload left (provider usage)", anthropic: true, body: messagesBody,
			client: pipeClient("application/json", &pipe),
			drive: func(run *caseRun) {
				send(run, anthropicSuccess)
				run.writer.waitFor(t, `"end_turn"`)
				run.cancel()
			}},
		{name: "row 26/openai partial JSON, workload left (estimate)", path: chatPath, body: chatBody,
			client: pipeClient("application/json", &pipe),
			drive: func(run *caseRun) {
				send(run, `{"id":"chatcmpl-1",`)
				run.writer.waitFor(t, `"chatcmpl-1"`)
				run.cancel()
			}},
		{name: "row 26/openai terminal, then workload left (provider usage)", path: testResponsesPath, body: responsesStreamRequest,
			client: pipeClient(eventStream, &pipe),
			drive: func(run *caseRun) {
				send(run, responsesStream)
				run.writer.waitFor(t, "response.completed")
				run.cancel()
			}},
		{name: "row 26/anthropic stream, gateway stopping", anthropic: true, body: anthropicStreamRequest, useDrain: true,
			client: pipeClient(eventStream, &pipe),
			drive: func(run *caseRun) {
				send(run, anthropicEvents[0])
				run.writer.waitFor(t, "message_start")
				shutdown(run)
			}},
		{name: "row 26/anthropic stream, call deadline", anthropic: true, body: anthropicStreamRequest, grant: oneSecondCall,
			client: pipeClient(eventStream, &pipe),
			drive: func(run *caseRun) {
				send(run, anthropicEvents[0])
				run.writer.waitFor(t, "message_start")
			}},
		{name: "row 27/openai body read error after headers", path: chatPath, body: chatBody,
			client: respondFailing(http.StatusOK, `{"id":"chatcmpl-1",`)},
		{name: "row 27/openai terminal, then body read error (provider usage)", path: testResponsesPath,
			body: responsesStreamRequest, client: respondFailingAs(http.StatusOK, eventStream, responsesStream)},
		{name: "row 28/openai stream ended inside an event", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, responsesEvents[0]+"event: response.output_text.delta\ndata: {")},
		{name: "row 28/openai stream ended without its terminal event", path: testResponsesPath,
			body:   responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, strings.Join(responsesEvents[:len(responsesEvents)-1], ""))},
		{name: "row 29/openai JSON over the parse bound", path: chatPath, body: chatBody,
			client: respond(http.StatusOK, "application/json", largeJSON)},
		{name: "row 30/openai JSON completed", path: chatPath, body: chatBody},
		{name: "row 30/anthropic JSON completed", anthropic: true, body: messagesBody},
		{name: "row 30/openai JSON without usage (estimate)", path: chatPath, body: chatBody,
			client: respond(http.StatusOK, "application/json", `{"id":"chatcmpl-1"}`)},
		{name: "row 30/openai JSON usage above the reservation (exceeded)", path: chatPath,
			body: `{"model":"test","max_tokens":5,"messages":[]}`, limits: budgetOf(1000, 0),
			client: respond(http.StatusOK, "application/json", `{"usage":{"total_tokens":900,"prompt_tokens":5,"completion_tokens":895}}`)},

		// Settle modifiers.
		{name: "modifier/anthropic unpriced usage under a cost cap", anthropic: true, limits: budgetOf(10_000, 1_000_000),
			body: messagesBody,
			client: respond(http.StatusOK, "application/json", strings.Replace(anthropicSuccess, `"output_tokens":2`,
				`"output_tokens":2,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5}`, 1))},
		{name: "modifier/openai settlement refused (unsettled)", path: chatPath, body: chatBody,
			ledger: func() budget.Ledger {
				return settleFailingLedger{Ledger: budget.NewMemoryLedger(), err: errors.New("ledger down")}
			}},
		{name: "modifier/openai estimate refused (unsettled)", path: chatPath, body: chatBody,
			client: respond(http.StatusOK, "application/json", `{"id":"chatcmpl-1"}`),
			ledger: func() budget.Ledger {
				return settleFailingLedger{Ledger: budget.NewMemoryLedger(), err: errors.New("ledger down")}
			}},
		{name: "modifier/openai lease already recovered (recovered)", path: chatPath, body: chatBody,
			ledger: func() budget.Ledger {
				return settleFailingLedger{Ledger: budget.NewMemoryLedger(), err: budget.ErrReservationRecovered}
			}},
		{name: "modifier/openai forfeit refused (unsettled)", path: chatPath, body: chatBody,
			client: respond(http.StatusOK, "application/json", largeJSON),
			ledger: func() budget.Ledger {
				return settleFailingLedger{Ledger: budget.NewMemoryLedger(), err: errors.New("ledger down")}
			}},
		{name: "modifier/openai no budget, completed", path: chatPath, body: chatBody, noBudget: true},
		{name: "modifier/openai no budget, idle timeout (estimate recorded, nothing charged)", path: testResponsesPath,
			body: responsesStreamRequest, noBudget: true,
			client: pipeClient(eventStream, &pipe),
			config: func(_ *caseRun, c *model.HandlerConfig) { c.IdleTimeout = 50 * time.Millisecond }},
		{name: "modifier/openai no budget, over the parse bound", path: chatPath, body: chatBody, noBudget: true,
			client: respond(http.StatusOK, "application/json", largeJSON)},
	}
	lines := make([]string, 0, len(cases))
	for _, tc := range cases {
		lines = append(lines, runModelCase(t, tc))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}

// TestRelayOutcomesAreRecorded records what the sign-in and account relays
// reply and audit; a one-unit model budget shows they charge nothing.
func TestRelayOutcomesAreRecorded(t *testing.T) {
	const signIn, account = "https://platform.claude.com", "https://api.github.com"
	ok := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	lines := make([]string, 0, 12)
	for _, tc := range []struct {
		name, method, url, body string
		upstream                http.HandlerFunc
		closed                  bool
		relay                   func(upstream string, client *http.Client, logger audit.Writer) *model.Relay
	}{
		{name: "relay/sign-in served", method: http.MethodPost, url: signIn + "/v1/oauth/token", body: refreshBody,
			upstream: ok(http.StatusOK, `{"access_token":"sk-ant-oat01-new"}`)},
		{name: "relay/sign-in provider non-2xx", method: http.MethodPost, url: signIn + "/v1/oauth/token", body: refreshBody,
			upstream: ok(http.StatusBadRequest, `{"error":"invalid_grant"}`)},
		{name: "relay/sign-in wrong method", method: http.MethodGet, url: signIn + "/v1/oauth/token"},
		{name: "relay/sign-in unrecognized method", method: "BREW", url: signIn + "/v1/oauth/token"},
		{name: "relay/sign-in other path", method: http.MethodPost, url: signIn + "/v1/organizations", body: "{}"},
		{name: "relay/sign-in query refused", method: http.MethodPost, url: signIn + "/v1/oauth/token?code=secret-value", body: refreshBody},
		{name: "relay/sign-in request over the limit", method: http.MethodPost, url: signIn + "/v1/oauth/token",
			body: strings.Repeat("a", 65<<10)},
		{name: "relay/sign-in build request failed", method: http.MethodPost, url: signIn + "/v1/oauth/token", body: refreshBody,
			relay: func(_ string, client *http.Client, logger audit.Writer) *model.Relay {
				return model.NewAnthropicSignInHandler("http://relay.invalid/\x7f", client, logger)
			}},
		{name: "relay/sign-in provider unreachable", method: http.MethodPost, url: signIn + "/v1/oauth/token", body: refreshBody,
			closed: true},
		{name: "relay/sign-in response over the limit", method: http.MethodPost, url: signIn + "/v1/oauth/token", body: refreshBody,
			upstream: ok(http.StatusOK, strings.Repeat("r", 8<<20+1))},
		{name: "relay/account gateway-held token unavailable", method: http.MethodGet, url: account + "/copilot_internal/user",
			relay: func(upstream string, client *http.Client, logger audit.Writer) *model.Relay {
				provider, _ := model.ProviderFor(model.ProviderCopilot)
				relay := provider.AccountRelay(modelauth.Token{}, client, logger)
				relay.UpstreamURL = upstream
				return relay
			}},
		{name: "relay/account served with the harness's own sign-in", method: http.MethodGet, url: account + "/copilot_internal/user",
			upstream: ok(http.StatusOK, `{"login":"fixture"}`),
			relay: func(upstream string, client *http.Client, logger audit.Writer) *model.Relay {
				provider, _ := model.ProviderFor(model.ProviderCopilot)
				relay := provider.AccountRelay(modelauth.Workload{}, client, logger)
				relay.UpstreamURL = upstream
				return relay
			}},
	} {
		handler := tc.upstream
		if handler == nil {
			handler = ok(http.StatusOK, "{}")
		}
		server := httptest.NewServer(handler)
		if tc.closed {
			server.Close()
		}
		events := &recordedEvents{}
		build := tc.relay
		if build == nil {
			build = func(upstream string, client *http.Client, logger audit.Writer) *model.Relay {
				return model.NewAnthropicSignInHandler(upstream, client, logger)
			}
		}
		relay := build(server.URL, server.Client(), events)
		tracker := budget.NewTracker()
		identity := testExecutionGrant()
		identity.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 1}
		authority := "platform.claude.com:443"
		if strings.HasPrefix(tc.url, account) {
			authority = "api.github.com:443"
		}
		routes := gateway.Dispatcher{authority: {Capability: api.CapabilityModelInference, Tier: audit.TierSemantic,
			Handler: relay, Budgets: tracker, AuditLogger: events}}
		before := ledgerReading(tracker)
		req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer gho_harness_fixture")
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), identity)))
		server.Close()
		lines = append(lines, outcomeLine(t, tc.name, response.Code, response.Header(), response.Body.String(),
			before+" -> "+ledgerReading(tracker), events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "relay-outcomes.txt"), lines)
}
