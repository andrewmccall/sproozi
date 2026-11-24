package model_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
)

// endOrder watches one call from outside: whether the provider request was
// still open when the ledger charged the reservation, and whether the
// reservation was charged and the provider request cancelled when the audit
// event was written.
type endOrder struct {
	t  *testing.T
	mu sync.Mutex
	// upstream is the provider request's context.
	upstream context.Context
	// openAtCharge is whether the provider request was open at each charge.
	openAtCharge []bool
	// audited is how many events were written.
	audited int
}

func (o *endOrder) charged() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.openAtCharge = append(o.openAtCharge, o.upstream != nil && o.upstream.Err() == nil)
}

type orderLedger struct {
	budget.Ledger
	order *endOrder
}

func (l orderLedger) Settle(ctx context.Context, r *budget.LedgerReservation, u budget.TrustedUsage) error {
	l.order.charged()
	return l.Ledger.Settle(ctx, r, u)
}

func (l orderLedger) Release(ctx context.Context, r *budget.LedgerReservation) error {
	l.order.charged()
	return l.Ledger.Release(ctx, r)
}

type orderAudit struct {
	order *endOrder
	next  audit.Writer
}

func (a orderAudit) Log(e audit.Event) {
	o := a.order
	o.mu.Lock()
	o.audited++
	charges, cancelled := len(o.openAtCharge), o.upstream != nil && o.upstream.Err() != nil
	o.mu.Unlock()
	if charges != 1 {
		o.t.Errorf("the audit event was written after %d charges, want 1", charges)
	}
	if !cancelled {
		o.t.Error("the audit event was written while the provider request was still open")
	}
	a.next.Log(e)
}

// A relayed call is charged, then its provider request is cancelled, then
// its audit event is written, whichever way it ends.
func TestRelayExitsChargeThenCancelThenRecord(t *testing.T) {
	responsesStream := strings.Join(fixtureEvents(t, "responses-reasoning.sse"), "")
	var pipe *io.PipeWriter
	cases := []modelCase{
		{name: "end of body on a completed stream", path: testResponsesPath, body: responsesStreamRequest,
			client: respond(http.StatusOK, eventStream, responsesStream)},
		{name: "provider read error mid-stream", path: chatPath, body: chatBody,
			client: respondFailing(http.StatusOK, `{"id":"chatcmpl-1",`)},
		{name: "idle cut", path: testResponsesPath, body: responsesStreamRequest,
			client: pipeClient(eventStream, &pipe),
			config: func(_ *caseRun, c *model.HandlerConfig) { c.IdleTimeout = 50 * time.Millisecond }},
		{name: "workload write failure", path: testResponsesPath, body: responsesStreamRequest,
			failWrite: true, client: respond(http.StatusOK, eventStream, responsesStream)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order := &endOrder{t: t}
			tc.ledger = func() budget.Ledger { return orderLedger{Ledger: budget.NewMemoryLedger(), order: order} }
			wrap := tc.config
			tc.config = func(run *caseRun, c *model.HandlerConfig) {
				if wrap != nil {
					wrap(run, c)
				}
				next := c.HTTPClient.Transport
				c.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					order.mu.Lock()
					order.upstream = r.Context()
					order.mu.Unlock()
					return next.RoundTrip(r)
				})}
				c.AuditLogger = orderAudit{order: order, next: c.AuditLogger}
			}
			runModelCase(t, tc)
			if order.audited != 1 || len(order.openAtCharge) != 1 {
				t.Fatalf("audit events %d, charges %d, want one of each", order.audited, len(order.openAtCharge))
			}
			if !order.openAtCharge[0] {
				t.Error("the provider request was cancelled before the reservation was charged")
			}
		})
	}
}
