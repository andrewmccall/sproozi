package gateway

import (
	"context"
	"sync"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// decision is what one dispatched request has recorded: nothing yet, an
// audit event, or a declaration that it has none by design. Tool calls may
// run on other goroutines, so it is guarded.
type decision struct {
	mu       sync.Mutex
	recorded bool
	declared UnrecordedReason
}

type decisionContextKey struct{}

// withDecision starts the request's decision.
func withDecision(ctx context.Context) (context.Context, *decision) {
	d := &decision{}
	return context.WithValue(ctx, decisionContextKey{}, d), d
}

func decisionFrom(ctx context.Context) *decision {
	d, _ := ctx.Value(decisionContextKey{}).(*decision)
	return d
}

// Record writes e through w, the handler's audit writer, and marks the
// request in ctx decided. Every handler records each reply this way. A nil w
// writes nothing and leaves the request undecided, so the dispatcher records
// the omission.
func Record(ctx context.Context, w audit.Writer, e audit.Event) {
	if w == nil {
		return
	}
	w.Log(e)
	if d := decisionFrom(ctx); d != nil {
		d.mu.Lock()
		d.recorded = true
		d.mu.Unlock()
	}
}

// UnrecordedReason names a kind of request that has no audit record by
// design. Each is pending an owner decision on whether to audit it
// (ADR 0070). Code outside this package can neither construct nor alter one:
// it can only take the values the functions below return, and the zero value
// declares nothing.
type UnrecordedReason struct{ name string }

func (r UnrecordedReason) String() string { return r.name }

var (
	unrecordedMCPDiscovery  = UnrecordedReason{"named MCP discovery"}
	unrecordedKubernetesMCP = UnrecordedReason{"Kubernetes MCP discovery and refusals before authorization"}
	unrecordedControl       = UnrecordedReason{"control connection"}
)

// UnrecordedMCPDiscovery: named MCP initialize, tools/list, ping,
// server/discover and notifications.
func UnrecordedMCPDiscovery() UnrecordedReason { return unrecordedMCPDiscovery }

// UnrecordedKubernetesMCP: Kubernetes /mcp discovery, and the shape and
// argument refusals that end before a pod list is authorized.
func UnrecordedKubernetesMCP() UnrecordedReason { return unrecordedKubernetesMCP }

// UnrecordedControl: the control connection, its refusals and its diagnose
// calls.
func UnrecordedControl() UnrecordedReason { return unrecordedControl }

// UnrecordedReasons lists every reason a request may go unrecorded.
func UnrecordedReasons() []UnrecordedReason {
	return []UnrecordedReason{unrecordedMCPDiscovery, unrecordedKubernetesMCP, unrecordedControl}
}

// Unrecorded declares that the request in ctx has no audit record by design,
// so the dispatcher does not record it as an omission. A record written
// before or after still counts. The first reason declared is kept.
func Unrecorded(ctx context.Context, reason UnrecordedReason) {
	d := decisionFrom(ctx)
	if d == nil || reason == (UnrecordedReason{}) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.declared == (UnrecordedReason{}) {
		d.declared = reason
	}
}

// DeclaredUnrecorded reports the reason the request in ctx was declared
// unrecorded with, if any, and whether it has recorded an event.
func DeclaredUnrecorded(ctx context.Context) (reason UnrecordedReason, declared, recorded bool) {
	d := decisionFrom(ctx)
	if d == nil {
		return UnrecordedReason{}, false, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.declared, d.declared != (UnrecordedReason{}), d.recorded
}

// recordOmission writes the route's event with outcome unrecorded when the
// handler neither recorded nor declared its request. The event names the
// route, never the request's path, query or headers.
func recordOmission(ctx context.Context, d *decision, route Route, key string) {
	d.mu.Lock()
	omitted := !d.recorded && d.declared == (UnrecordedReason{})
	d.mu.Unlock()
	if !omitted {
		return
	}
	e := AuditEvent(ctx, key)
	e.Outcome = audit.Unrecorded
	Record(ctx, route.AuditLogger, e)
}
