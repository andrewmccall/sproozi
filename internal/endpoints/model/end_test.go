package model

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

// endEvents keeps the audit events end writes.
type endEvents struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *endEvents) Log(e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// How a test call's context has ended when end asks why.
const (
	callLive     = ""
	callLeft     = "left"
	callStopping = "stopping"
	callDeadline = "deadline"
)

const endTestAccount = "end-test/model.inference"

// endTestCall is an admitted OpenAI call holding all of a 1000-unit budget
// for a 40-byte unbounded request, so it is charged 10 units for an
// interrupt (its input; no output and no elapsed time), 7 for its reported
// usage and 1000 for its whole reservation.
type endTestCall struct {
	call    *modelCall
	tracker *budget.Tracker
	events  *endEvents
	writer  *httptest.ResponseRecorder
}

func newEndTestCall(t *testing.T, phase callPhase, ended string) *endTestCall {
	t.Helper()
	tracker := budget.NewTracker()
	limits := api.EndpointBudget{MaxUnits: 1000}
	meter := tracker.Meter(endTestAccount, limits)
	events := &endEvents{}
	now := time.Unix(1_800_000_000, 0)
	h := &Handler{Config: HandlerConfig{AuditLogger: events, Logger: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return now }}}
	ctx := context.Background()
	switch ended {
	case callLeft:
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	case callStopping:
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		cancel(errGatewayStopping)
	case callDeadline:
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadlineCause(ctx, time.Now().Add(-time.Second), proxytransport.ErrModelCallDeadline)
		t.Cleanup(cancel)
	}
	r := httptest.NewRequestWithContext(budget.WithMeter(context.Background(), meter), http.MethodPost,
		chatCompletionsPath, nil)
	body := []byte(`{"model":"m","messages":[],"pad":"xxxx"}`)
	reservation, done, err := meter.ReserveRequest(context.Background(), 0, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := httptest.NewRecorder()
	call := &modelCall{h: h, ctx: ctx, w: writer, r: r, limits: limits, reservation: reservation,
		plan: reservationPlan{sizable: true}, body: body, phase: phase, startedAt: now,
		cancelUpstream: func() {}, checkOwner: newOwnerCheck()}
	t.Cleanup(done)
	return &endTestCall{call: call, tracker: tracker, events: events, writer: writer}
}

func (c *endTestCall) used(t *testing.T) (used, reserved int64) {
	t.Helper()
	state, err := c.tracker.State(endTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	return state.UnitsUsed, state.ReservedUnits
}

func reportedUsage() providerUsage {
	total, prompt, completion := int64(7), int64(5), int64(2)
	return providerUsage{usage: responseUsage{Usage: &tokenUsage{TotalTokens: &total, PromptTokens: &prompt,
		CompletionTokens: &completion}}}
}

func rejection() *http.Response {
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`))}
}

const chatCompletionsPath = "/v1/chat/completions"

// TestEndDerivesEachEndingFromItsPhase declares every (phase, ending) pair
// end accepts, as ADR 0058 lays out for lifecycle records: the charge it
// makes, the end reason and outcome it records, and what the workload is
// sent. A settled call was settled from its reported usage first.
func TestEndDerivesEachEndingFromItsPhase(t *testing.T) {
	usage := ending{kind: endingUsage, reason: endCompleted, usage: reportedUsage()}
	unsent := ending{kind: endingUnsent, reply: errorReply{http.StatusServiceUnavailable, "provider authentication is unavailable",
		serverError, "provider_auth_unavailable"}}
	cases := []struct {
		phase  callPhase
		x      ending
		ended  string
		charge chargeRule
		// used and reserved are the ledger's after end.
		used, reserved int64
		endReason      string
		outcome        audit.Outcome
		allowed        bool
		status         int
	}{
		{callAdmitted, unsent, callLive, chargeRelease, 0, 0, "", audit.Unavailable, false, http.StatusServiceUnavailable},
		// Q2: neither a provider failure nor the deadline is recorded as an end reason.
		{callAdmitted, ending{kind: endingNoResponse}, callLive, chargeRelease, 0, 0, "", audit.UpstreamFailed, false, http.StatusBadGateway},
		{callAdmitted, ending{kind: endingNoResponse}, callDeadline, chargeRelease, 0, 0, "", audit.UpstreamFailed, false, http.StatusBadGateway},
		{callAdmitted, ending{kind: endingNoResponse}, callLeft, chargeRelease, 0, 0, endClientCancelled, audit.Cancelled, true, 0},
		{callAdmitted, ending{kind: endingNoResponse}, callStopping, chargeRelease, 0, 0, endGatewayRestart, audit.Unavailable, false, http.StatusServiceUnavailable},
		// Q3: an OpenAI rejection is relayed and allowed.
		{callAdmitted, ending{kind: endingRejected, response: rejection()}, callLive, chargeRelease, 0, 0, "", audit.Served, true, http.StatusTooManyRequests},
		{callWritten, ending{kind: endingNoResponse}, callLive, chargeEstimate, 10, 0, endUpstreamError, audit.UpstreamFailed, false, http.StatusBadGateway},
		{callWritten, ending{kind: endingNoResponse}, callDeadline, chargeEstimate, 10, 0, endDeadline, audit.UpstreamFailed, false, http.StatusBadGateway},
		{callWritten, ending{kind: endingNoResponse}, callLeft, chargeEstimate, 10, 0, endClientCancelled, audit.Cancelled, true, 0},
		{callWritten, ending{kind: endingNoResponse}, callStopping, chargeEstimate, 10, 0, endGatewayRestart, audit.Unavailable, false, http.StatusServiceUnavailable},
		{callWritten, ending{kind: endingRejected, response: rejection()}, callLive, chargeRelease, 0, 0, "", audit.Served, true, http.StatusTooManyRequests},
		{callRelaying, ending{kind: endingUnsupported}, callLive, chargeEstimate, 10, 0, endUpstreamError, audit.UpstreamFailed, false, http.StatusBadGateway},
		{callRelaying, ending{kind: endingInterrupted, reason: endClientCancelled}, callLive, chargeEstimate, 10, 0, endClientCancelled, audit.Served, true, 0},
		{callRelaying, usage, callLive, chargeProvider, 7, 0, endCompleted, audit.Served, true, 0},
		{callRelaying, ending{kind: endingCut}, callLive, chargeReservation, 1000, 0, endBudgetCut, audit.Served, true, 0},
		{callSettled, ending{kind: endingInterrupted, reason: endClientCancelled}, callLive, chargeNone, 7, 0, endCompleted, audit.Served, true, 0},
		{callSettled, ending{kind: endingRelayed}, callLive, chargeNone, 7, 0, endCompleted, audit.Served, true, 0},
	}
	tested := map[callPhase]map[endingKind]bool{}
	for _, tc := range cases {
		name := tc.phase.String() + "/" + tc.x.kind.String() + "/" + tc.ended
		t.Run(name, func(t *testing.T) {
			if rule, declared := callEndings[tc.phase][tc.x.kind]; !declared || rule != tc.charge {
				t.Fatalf("callEndings[%s][%s] = %v, %v; want charge %v", tc.phase, tc.x.kind, rule, declared, tc.charge)
			}
			c := newEndTestCall(t, tc.phase, tc.ended)
			if tc.phase == callSettled {
				c.call.phase = callRelaying
				if err := c.call.settle(usage); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.call.endOnce(tc.x); err != nil {
				t.Fatal(err)
			}
			if c.call.phase != callEnded {
				t.Fatalf("phase = %s, want ended", c.call.phase)
			}
			if used, reserved := c.used(t); used != tc.used || reserved != tc.reserved {
				t.Errorf("ledger used=%d reserved=%d, want used=%d reserved=%d", used, reserved, tc.used, tc.reserved)
			}
			if len(c.events.events) != 1 {
				t.Fatalf("recorded %d events, want 1", len(c.events.events))
			}
			e := c.events.events[0]
			if e.EndReason != tc.endReason || e.Outcome != tc.outcome || e.Allowed != tc.allowed {
				t.Errorf("event endReason=%q outcome=%q allowed=%v, want %q %q %v",
					e.EndReason, e.Outcome, e.Allowed, tc.endReason, tc.outcome, tc.allowed)
			}
			status := 0
			if c.writer.Body.Len() > 0 {
				status = c.writer.Code
			}
			if status != tc.status {
				t.Errorf("reply status %d, want %d", status, tc.status)
			}
		})
		if tested[tc.phase] == nil {
			tested[tc.phase] = map[endingKind]bool{}
		}
		tested[tc.phase][tc.x.kind] = true
	}
	for phase, endings := range callEndings {
		for kind := range endings {
			if !tested[phase][kind] {
				t.Errorf("callEndings declares %s from %s, which this table does not test", kind, phase)
			}
		}
	}
}

// end refuses an ending its phase does not declare. The refusal is logged
// and the call falls back to its phase's own conservative ending
// (callFallbacks), so it is still charged once, recorded once and, when the
// workload has been sent nothing, answered with the gateway's own 500. An
// ending after the call ended stays a logged no-op. end refuses rather
// than panics: a panic mid-relay would cut the workload's response and
// skip the record.
func TestEndRefusesUndeclaredAndRepeatedEndings(t *testing.T) {
	kinds := []endingKind{endingUnsent, endingNoResponse, endingRejected, endingUnsupported,
		endingInterrupted, endingUsage, endingCut, endingRelayed}
	for _, phase := range []callPhase{callAdmitted, callWritten, callRelaying, callSettled, callEnded} {
		for _, kind := range kinds {
			if _, declared := callEndings[phase][kind]; declared {
				continue
			}
			t.Run(phase.String()+"/"+kind.String(), func(t *testing.T) { checkRefusedEnding(t, phase, kind, false) })
			if phase == callRelaying {
				t.Run(phase.String()+"/"+kind.String()+"/headers sent", func(t *testing.T) {
					checkRefusedEnding(t, phase, kind, true)
				})
			}
		}
	}

	t.Run("second ending", func(t *testing.T) {
		c := newEndTestCall(t, callRelaying, callLive)
		c.call.end(ending{kind: endingInterrupted, reason: endClientCancelled})
		for _, x := range []ending{
			{kind: endingInterrupted, reason: endClientCancelled},
			{kind: endingCut},
			{kind: endingUsage, reason: endCompleted, usage: reportedUsage()},
		} {
			if err := c.call.endOnce(x); err == nil {
				t.Fatalf("end accepted %s after the call ended", x.kind)
			}
		}
		if used, _ := c.used(t); used != 10 || len(c.events.events) != 1 {
			t.Fatalf("used=%d events=%d, want one estimate of 10 recorded once", used, len(c.events.events))
		}
	})

	t.Run("second settlement", func(t *testing.T) {
		c := newEndTestCall(t, callRelaying, callLive)
		if err := c.call.settle(ending{kind: endingUsage, reason: endCompleted, usage: reportedUsage()}); err != nil {
			t.Fatal(err)
		}
		for _, x := range []ending{
			{kind: endingUsage, reason: endCompleted, usage: reportedUsage()},
			{kind: endingInterrupted, reason: endUpstreamError},
			{kind: endingRelayed},
		} {
			if err := c.call.settle(x); err == nil {
				t.Fatalf("settle accepted %s after the call settled", x.kind)
			}
		}
		if used, _ := c.used(t); used != 7 || c.call.phase != callSettled || len(c.events.events) != 0 {
			t.Fatalf("used=%d phase=%s events=%d, want 7 settled once and nothing recorded",
				used, c.call.phase, len(c.events.events))
		}
	})
}

// A call moves only by callTransitions; an undeclared event is refused and
// leaves the phase.
func TestCallTransitionsAreDeclared(t *testing.T) {
	want := map[callEvent]map[callPhase]callPhase{
		eventWrote:    {callAdmitted: callWritten},
		eventAnswered: {callAdmitted: callRelaying, callWritten: callRelaying},
		eventSettled:  {callAdmitted: callSettled, callWritten: callSettled, callRelaying: callSettled},
		eventEnded:    {callSettled: callEnded},
	}
	for _, event := range []callEvent{eventWrote, eventAnswered, eventSettled, eventEnded} {
		for _, from := range []callPhase{callAdmitted, callWritten, callRelaying, callSettled, callEnded} {
			c := &modelCall{phase: from}
			err := c.move(event)
			to, declared := want[event][from]
			switch {
			case declared && (err != nil || c.phase != to):
				t.Errorf("%s from %s: phase=%s err=%v, want %s", event, from, c.phase, err, to)
			case !declared && (err == nil || c.phase != from):
				t.Errorf("%s from %s: phase=%s err=%v, want refused", event, from, c.phase, err)
			}
		}
	}
}

// refusedFallback is what a refused ending falls back to, by phase, before
// any response headers were sent.
var refusedFallback = map[callPhase]struct {
	used    int64
	allowed bool
	outcome audit.Outcome
	status  int
	reason  string
}{
	callAdmitted: {0, false, audit.Unavailable, http.StatusInternalServerError, ""},
	callWritten:  {10, false, audit.Unavailable, http.StatusInternalServerError, endUpstreamError},
	callRelaying: {10, false, audit.Unavailable, http.StatusInternalServerError, endUpstreamError},
	callSettled:  {7, true, audit.Served, 0, endCompleted},
}

// checkRefusedEnding ends a call in phase with an undeclared kind and checks
// that it fell back: settled once with nothing left reserved, recorded once
// and answered when nothing was sent; or, after the end, left unchanged.
func checkRefusedEnding(t *testing.T, phase callPhase, kind endingKind, headersSent bool) {
	t.Helper()
	c := newEndTestCall(t, callRelaying, callLive)
	switch phase {
	case callSettled:
		if err := c.call.settle(ending{kind: endingUsage, reason: endCompleted, usage: reportedUsage()}); err != nil {
			t.Fatal(err)
		}
	case callEnded:
		c.call.end(ending{kind: endingUsage, reason: endCompleted, usage: reportedUsage()})
	default:
		c.call.phase = phase
	}
	c.call.headersSent = headersSent
	eventsBefore := len(c.events.events)
	usedBefore, _ := c.used(t)
	x := ending{kind: kind, reason: endClientCancelled, usage: reportedUsage(), response: rejection()}
	if err := c.call.endOnce(x); err == nil {
		t.Fatal("end did not report an undeclared ending")
	}
	used, reserved := c.used(t)
	if phase == callEnded {
		if len(c.events.events) != eventsBefore || used != usedBefore || c.writer.Body.Len() != 0 {
			t.Fatalf("an ending after the end changed the call: events=%d used=%d reply=%q",
				len(c.events.events), used, c.writer.Body.String())
		}
		return
	}
	want := refusedFallback[phase]
	if headersSent {
		want.allowed, want.outcome, want.status = true, audit.Served, 0
	}
	if c.call.phase != callEnded || reserved != 0 || used != want.used {
		t.Fatalf("refused ending left phase=%s used=%d reserved=%d, want ended used=%d reserved=0",
			c.call.phase, used, reserved, want.used)
	}
	if len(c.events.events) != 1 {
		t.Fatalf("refused ending recorded %d events, want 1", len(c.events.events))
	}
	e := c.events.events[0]
	if e.Allowed != want.allowed || e.Outcome != want.outcome || e.EndReason != want.reason {
		t.Errorf("event allowed=%v outcome=%q endReason=%q, want %v %q %q",
			e.Allowed, e.Outcome, e.EndReason, want.allowed, want.outcome, want.reason)
	}
	status := 0
	if c.writer.Body.Len() > 0 {
		status = c.writer.Code
	}
	if status != want.status {
		t.Errorf("reply status %d (%q), want %d", status, c.writer.Body.String(), want.status)
	}
}
