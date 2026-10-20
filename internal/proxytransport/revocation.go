package proxytransport

import (
	"log/slog"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

// executionLogWindow groups events for one execution's inspected connections.
// Revalidation ticks run per connection, so a revoked execution usually loses
// all of its connections within one tick of each other, and a client over its
// per-execution limit usually retries several connections at once.
const executionLogWindow = 10 * time.Second

// executionLog writes one line for the first event of an execution in a
// window, and one summary line counting the others when the window closes, so
// an execution with many connections does not flood the log. Lines carry the
// execution reference and one detail, never credentials.
type executionLog struct {
	logger  *slog.Logger
	window  time.Duration
	first   string
	further string
	key     string

	mu     sync.Mutex
	bursts map[gateway.Reference]*executionBurst
}

type executionBurst struct {
	more   int
	detail any
}

func newExecutionLog(logger *slog.Logger, first, further, key string) *executionLog {
	if logger == nil {
		logger = slog.Default()
	}
	return &executionLog{logger: logger, window: executionLogWindow, first: first, further: further, key: key,
		bursts: map[gateway.Reference]*executionBurst{}}
}

// newRevocationLog reports revoked inspected connections with their reason.
func newRevocationLog(logger *slog.Logger) *executionLog {
	return newExecutionLog(logger, "Revoked inspected connection", "Revoked further inspected connections", "reason")
}

// newLimitLog reports inspected connections refused with 429 because their
// execution already holds MaxSessionsPerExecution sessions.
func newLimitLog(logger *slog.Logger) *executionLog {
	return newExecutionLog(logger, "Rejected inspected connection at per-execution limit",
		"Rejected further inspected connections at per-execution limit", "limit")
}

// revoked reports one revoked inspected connection of execution.
func (l *executionLog) revoked(execution gateway.Reference, cause error) {
	reason := "revalidation failed"
	if cause != nil {
		reason = cause.Error()
	}
	l.record(execution, reason)
}

// record reports one event for execution with its detail.
func (l *executionLog) record(execution gateway.Reference, detail any) {
	l.mu.Lock()
	if burst, open := l.bursts[execution]; open {
		burst.more++
		burst.detail = detail
		l.mu.Unlock()
		return
	}
	l.bursts[execution] = &executionBurst{}
	l.mu.Unlock()
	l.logger.Info(l.first, append(executionAttrs(execution), l.key, detail)...)
	time.AfterFunc(l.window, func() { l.flush(execution) })
}

func (l *executionLog) flush(execution gateway.Reference) {
	l.mu.Lock()
	burst := l.bursts[execution]
	delete(l.bursts, execution)
	l.mu.Unlock()
	if burst != nil && burst.more > 0 {
		l.logger.Info(l.further, append(executionAttrs(execution), "count", burst.more, l.key, burst.detail)...)
	}
}

func executionAttrs(execution gateway.Reference) []any {
	return []any{"backend", execution.Backend, "namespace", execution.Namespace,
		"execution", execution.Name, "executionUID", execution.UID}
}
