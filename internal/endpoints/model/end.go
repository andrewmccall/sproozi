package model

import (
	"fmt"
	"net/http"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// callPhase is how far an admitted model call has got. The call moves only
// by callTransitions (ADR 0058), and its ending is charged and recorded only
// by end, from the phase it has reached.
type callPhase uint8

const (
	// callAdmitted: the reservation is held and the provider does not have
	// the whole request.
	callAdmitted callPhase = iota
	// callWritten: the provider has the whole request and has sent no
	// response headers.
	callWritten
	// callRelaying: the provider answered 2xx and the response is relayed.
	callRelaying
	// callSettled: the reservation is charged. Whatever follows a terminal
	// event is relayed unparsed and charges nothing more.
	callSettled
	// callEnded: the call is charged, recorded and answered.
	callEnded
)

var callPhaseNames = [...]string{"admitted", "written", "relaying", "settled", "ended"}

func (p callPhase) String() string { return callPhaseNames[p] }

// callEvent moves a call from one phase to the next.
type callEvent uint8

const (
	// eventWrote: the transport reported the whole request written.
	eventWrote callEvent = iota
	// eventAnswered: the provider sent 2xx response headers.
	eventAnswered
	// eventSettled: the reservation was released, settled or forfeited.
	eventSettled
	// eventEnded: the call was recorded and answered.
	eventEnded
)

var callEventNames = [...]string{"wrote", "answered", "settled", "ended"}

func (e callEvent) String() string { return callEventNames[e] }

// callTransitions is each event's (from → to) table. Nothing leaves
// callEnded, so a call is settled and recorded at most once.
var callTransitions = map[callEvent]map[callPhase]callPhase{
	eventWrote:    {callAdmitted: callWritten},
	eventAnswered: {callAdmitted: callRelaying, callWritten: callRelaying},
	eventSettled:  {callAdmitted: callSettled, callWritten: callSettled, callRelaying: callSettled},
	eventEnded:    {callSettled: callEnded},
}

// move applies event to the call's phase, or refuses an undeclared one and
// leaves the phase unchanged.
func (c *modelCall) move(event callEvent) error {
	to, ok := callTransitions[event][c.phase]
	if !ok {
		return fmt.Errorf("model call event %s is not declared from %s", event, c.phase)
	}
	c.phase = to
	return nil
}

// mark moves the call by event, logging a refusal, which only a programming
// error causes.
func (c *modelCall) mark(event callEvent) {
	if err := c.move(event); err != nil {
		c.h.logger().Error("Refused model call event", "runID", c.e.RunID, "runUID", c.e.RunUID,
			"error", err.Error())
	}
}

// noResponse ends a call whose provider sent no response headers. A request
// the provider had is charged from when it was written; one it never had
// is released, so a retry can use the reservation.
func (c *modelCall) noResponse() {
	if at := c.written.Load(); at != nil {
		c.mark(eventWrote)
		c.startedAt = *at
		c.observeFailure("provider call ended before its response headers")
	}
	c.end(ending{kind: endingNoResponse})
}

// endingKind is what ended a call.
type endingKind uint8

const (
	// endingUnsent: the provider request could not be built or authorized.
	endingUnsent endingKind = iota + 1
	// endingNoResponse: the provider call failed before response headers.
	endingNoResponse
	// endingRejected: the provider answered with a status other than 2xx.
	endingRejected
	// endingUnsupported: the provider answered a stream with a content type
	// the gateway cannot verify.
	endingUnsupported
	// endingInterrupted: the response ended before its usage did, for
	// ending.reason.
	endingInterrupted
	// endingUsage: the response reported its usage.
	endingUsage
	// endingCut: the running estimate reached the reservation.
	endingCut
	// endingRelayed: a settled response was relayed to its end.
	endingRelayed
)

var endingNames = [...]string{"", "unsent", "no-response", "rejected", "unsupported", "interrupted", "usage", "cut", "relayed"}

func (k endingKind) String() string { return endingNames[k] }

// ending is everything that decides how a call ends.
type ending struct {
	kind endingKind
	// reason is the end reason of an interrupted call or of reported usage.
	// end derives the rest.
	reason string
	// usage is the provider's reported usage, for endingUsage.
	usage providerUsage
	// reply is the error the workload receives, for endingUnsent.
	reply errorReply
	// response is the provider's answer, for endingRejected.
	response *http.Response
	// fallback marks the ending end chose for an undeclared one.
	fallback bool
}

// providerUsage is a response's reported usage, with Anthropic's own
// categories for pricing.
type providerUsage struct {
	usage     responseUsage
	anthropic anthropicUsage
}

// errorReply is an error the gateway writes instead of a provider response.
type errorReply struct {
	status                 int
	message, errType, code string
}

// The error type of a reply for a failure that is not the workload's, and
// the code of one the provider caused.
const (
	serverError       = "server_error"
	upstreamErrorCode = "upstream_error"
)

// chargeRule is how an ending is charged against the reservation.
type chargeRule uint8

const (
	// chargeNone: the call was charged when it settled.
	chargeNone chargeRule = iota
	// chargeRelease: the provider did no work for the call.
	chargeRelease
	// chargeEstimate: the estimate E capped at the reservation, or the whole
	// reservation when an event outgrew the parser or the input cannot be
	// sized (settleInterrupted).
	chargeEstimate
	// chargeProvider: the provider's usage, or the whole reservation when
	// pricing is unavailable (settleProvider).
	chargeProvider
	// chargeReservation: the whole reservation.
	chargeReservation
)

// callEndings declares, for each phase, the endings end accepts and how
// each is charged. end refuses any other pair.
var callEndings = map[callPhase]map[endingKind]chargeRule{
	callAdmitted: {endingUnsent: chargeRelease, endingNoResponse: chargeRelease, endingRejected: chargeRelease},
	callWritten:  {endingNoResponse: chargeEstimate, endingRejected: chargeRelease},
	callRelaying: {
		endingUnsupported: chargeEstimate, endingInterrupted: chargeEstimate,
		endingUsage: chargeProvider, endingCut: chargeReservation,
	},
	callSettled: {endingInterrupted: chargeNone, endingRelayed: chargeNone},
}

// settle charges the reservation for x and moves the call to callSettled
// without recording it, as a terminal event does before the rest of the
// response is relayed. It refuses an ending its phase does not declare or
// one already charged.
func (c *modelCall) settle(x ending) error {
	rule, declared := callEndings[c.phase][x.kind]
	if !declared || rule == chargeNone {
		return fmt.Errorf("model call cannot settle ending %s from %s", x.kind, c.phase)
	}
	c.charge(rule, x)
	return c.move(eventSettled)
}

// callFallbacks is the ending a call takes when end is given one its phase
// does not declare: the most conservative ending that phase already has.
// Nothing falls back from callEnded, so an ending after the end does
// nothing.
//
// ADR 0058 has a lifecycle record refuse an undeclared transition and save
// nothing, which is safe for a record: what it already holds stays true. A
// model call is different. By the time it can end it holds a reservation,
// and from callRelaying the workload has its response headers, so refusing
// and doing nothing would leave the reservation held until its lease is
// recovered, write no audit event and, before headers, let net/http answer
// an empty 200. So the refusal is logged and the call still ends: charged
// once through charge, recorded once and, if the workload was sent nothing,
// answered with the gateway's own internal-failure reply.
var callFallbacks = map[callPhase]endingKind{
	callAdmitted: endingUnsent,      // release
	callWritten:  endingNoResponse,  // the estimate
	callRelaying: endingInterrupted, // the estimate
	callSettled:  endingRelayed,     // already charged: record what is owed
}

// end charges, records and answers a call once, deriving its charge, end
// reason and audit fields from the phase it reached and how it ended. An
// ending its phase does not declare is logged and replaced by the phase's
// fallback; any ending after the first is logged and does nothing.
func (c *modelCall) end(x ending) {
	if err := c.endOnce(x); err != nil {
		c.h.logger().Error("Refused model call ending", "runID", c.e.RunID, "runUID", c.e.RunUID,
			"error", err.Error())
	}
}

// endOnce ends the call, and reports an undeclared ending even when it
// ended the call by the phase's fallback instead.
func (c *modelCall) endOnce(x ending) error {
	from := c.phase
	rule, declared := callEndings[from][x.kind]
	var refused error
	if !declared {
		refused = fmt.Errorf("model call ending %s is not declared from %s", x.kind, from)
		kind, ok := callFallbacks[from]
		if !ok {
			return refused
		}
		x, rule = ending{kind: kind, fallback: true}, callEndings[from][kind]
	}
	// Q1 keeps this copy: the event as it was before the charge.
	before := c.e
	switch {
	case x.kind == endingNoResponse && from == callWritten:
		// A request the provider had is charged by why it failed, so that
		// is read before the charge.
		x.reason = c.failureReason()
	case x.kind == endingUnsupported:
		x.reason = endUpstreamError
	case x.fallback && from == callRelaying:
		x.reason = c.failureReason()
	}
	if rule != chargeNone {
		c.charge(rule, x)
		if err := c.move(eventSettled); err != nil {
			return err
		}
	}
	if x.kind == endingNoResponse && from == callAdmitted {
		// A request the provider never had is released first and only then
		// asked why it failed: the workload may leave, or the gateway stop,
		// while the ledger releases.
		x.reason = c.unsentReason()
	}
	if err := c.move(eventEnded); err != nil {
		return err
	}
	if c.stopRelay != nil {
		// The relay stops between the charge and the record.
		c.stopRelay()
	}
	c.record(from, x, before)
	return refused
}

// charge releases, settles or forfeits the reservation, and is the only
// place that does. It is reached only through settle and end. Both look
// the ending up in callEndings for the call's phase, which admits a charge
// only before callSettled, and move the call to callSettled as soon as it
// returns; nothing moves a call back. So a reservation is charged at most
// once, with no flag to check or reset.
func (c *modelCall) charge(rule chargeRule, x ending) {
	switch rule {
	case chargeRelease:
		c.h.release(&c.e, c.reservation)
	case chargeEstimate:
		c.settleInterrupted(x.reason)
	case chargeProvider:
		c.settleProvider(x.usage.usage, x.usage.anthropic, x.reason)
	case chargeReservation:
		c.chargeReservation(endBudgetCut, reasonBudgetCut)
	}
}

// record writes the call's audit event and, where the workload is still
// owed one, its reply.
//
// Three rules here keep outcomes that look inconsistent. Each is pinned by a
// row of testdata/outcomes.txt and changes only in its own commit:
//
//   - Q1. Until the provider answers 2xx, a refusal written to the workload
//     records the event as it was before the charge, so a failed release's
//     usageSource is not recorded: an unsent request, a provider rejection,
//     and a request the provider never had that failed for any reason but
//     the workload leaving or the gateway stopping.
//   - Q2. A request the provider never had that failed at its deadline or
//     could not reach the provider records no endReason: unsentReason gives
//     it none, and Q1's event has none.
//   - Q3. allowed is false exactly when the gateway writes an error reply,
//     with two exceptions in relayWhole: a provider's non-2xx answer is
//     relayed with allowed true and its upstreamStatus on OpenAI, and
//     replaced with allowed false and no upstreamStatus on Anthropic. So a
//     call ended by a stopping gateway records allowed false here, before
//     its response headers, while refuseReservation records allowed true for
//     one stopped while it waited for its budget.
func (c *modelCall) record(from callPhase, x ending, before audit.Event) {
	ctx := c.r.Context()
	if x.fallback {
		// A refused ending is answered only if the workload has nothing yet.
		if from == callSettled || c.headersSent {
			c.finish()
			return
		}
		c.h.denyReply(ctx, c.w, c.e, internalFailureReply)
		return
	}
	switch x.kind {
	case endingUnsent:
		c.h.denyReply(ctx, c.w, before, x.reply) // Q1
	case endingRejected:
		c.h.relayWhole(c.w, c.r, before, x.response, c.diagnostic, c.credential, c.limits) // Q1, Q3
	case endingNoResponse:
		switch x.reason {
		case endClientCancelled:
			// The harness left; nothing failed and no one reads a reply.
			c.e.EndReason, c.e.Outcome = endClientCancelled, audit.Cancelled
			c.finish()
		case endGatewayRestart:
			c.e.EndReason = endGatewayRestart
			c.h.denyReply(ctx, c.w, c.e, gatewayStoppingReply)
		default:
			event := c.e
			if from == callAdmitted {
				event = before // Q1, Q2
			}
			c.h.denyReply(ctx, c.w, event, upstreamFailedReply)
		}
	case endingUnsupported:
		c.h.denyReply(ctx, c.w, c.e, errorReply{http.StatusBadGateway, "unsupported provider response", serverError, upstreamErrorCode})
	default:
		c.finish()
	}
}

// failureReason is why a provider call failed: the call's own end, or the
// provider's failure.
func (c *modelCall) failureReason() string {
	if c.ctx.Err() != nil {
		return c.cancelReason()
	}
	return endUpstreamError
}

// unsentReason is why a call failed before the provider had its request:
// the workload left, the gateway stopped, or, for anything else including
// the call's deadline, no reason (Q2). It reads the cause before asking
// whether the call ended, in that order.
func (c *modelCall) unsentReason() string {
	switch {
	case c.cancelReason() == endClientCancelled && c.ctx.Err() != nil:
		return endClientCancelled
	case c.cancelReason() == endGatewayRestart && c.ctx.Err() != nil:
		return endGatewayRestart
	}
	return ""
}

// Replies to a call that ended before its response headers.
var (
	// internalFailureReply is the gateway's reply when it fails itself.
	internalFailureReply = errorReply{http.StatusInternalServerError, "failed to build upstream request", serverError, upstreamErrorCode}
	gatewayStoppingReply = errorReply{http.StatusServiceUnavailable, gatewayStoppingMessage, serverError, gatewayStoppingCode}
	upstreamFailedReply  = errorReply{http.StatusBadGateway, "upstream request failed", serverError, upstreamErrorCode}
)
