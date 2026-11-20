package gateway

import (
	"context"
	"sync/atomic"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// decision is what one dispatched request has recorded: nothing yet, an
// audit event, or a declaration that it has none by design. Tool calls may
// run on other goroutines, so it is atomic.
type decision struct{ state atomic.Int32 }

const (
	undecided int32 = iota
	recorded
	declaredUnrecorded
)

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
// writes nothing and leaves the request undecided.
func Record(ctx context.Context, w audit.Writer, e audit.Event) {
	if w == nil {
		return
	}
	w.Log(e)
	if d := decisionFrom(ctx); d != nil {
		d.state.Store(recorded)
	}
}

// UnrecordedReason names a kind of request that has no audit record by
// design. Each is pending an owner decision on whether to audit it
// (ADR 0070). The list is closed, so every deliberate omission can be found.
type UnrecordedReason string

const (
	// UnrecordedMCPDiscovery: named MCP initialize, tools/list, ping,
	// server/discover and notifications.
	UnrecordedMCPDiscovery UnrecordedReason = "named MCP discovery"
	// UnrecordedKubernetesMCP: Kubernetes /mcp discovery, and the shape and
	// argument refusals that end before a pod list is authorized.
	UnrecordedKubernetesMCP UnrecordedReason = "Kubernetes MCP discovery and refusals before authorization"
	// UnrecordedControl: the control connection, its refusals and its
	// diagnose calls.
	UnrecordedControl UnrecordedReason = "control connection"
)

// UnrecordedReasons lists every reason a request may go unrecorded.
var UnrecordedReasons = []UnrecordedReason{UnrecordedMCPDiscovery, UnrecordedKubernetesMCP, UnrecordedControl}

// Unrecorded declares that the request in ctx has no audit record by design,
// so the dispatcher does not record it as an omission. A record written
// before or after still counts.
func Unrecorded(ctx context.Context, _ UnrecordedReason) {
	if d := decisionFrom(ctx); d != nil {
		d.state.CompareAndSwap(undecided, declaredUnrecorded)
	}
}

// recordOmission writes the route's event with outcome unrecorded when the
// handler neither recorded nor declared its request. The event names the
// route, never the request's path, query or headers.
func recordOmission(ctx context.Context, d *decision, route Route, key string) {
	if d.state.Load() != undecided {
		return
	}
	e := AuditEvent(ctx, key)
	e.Outcome = audit.Unrecorded
	Record(ctx, route.AuditLogger, e)
}
