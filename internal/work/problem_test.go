package work

import (
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Only tasks are retried by the executor; an interactive session's problem
// must not promise a retry that never comes.
func TestExecutionProblemNamesWhoRetries(t *testing.T) {
	for _, phase := range []session.Phase{session.PhaseUnknownStop, session.PhaseUnknownUsage} {
		task := executionProblem(session.Record{Phase: phase}, false)
		interactive := executionProblem(session.Record{Phase: phase}, true)
		if !strings.Contains(task, "the executor retries") {
			t.Errorf("%s task problem %q does not say the executor retries", phase, task)
		}
		if strings.Contains(interactive, "the executor retries") || !strings.Contains(interactive, `run "recover"`) {
			t.Errorf("%s interactive problem %q", phase, interactive)
		}
	}
	if p := executionProblem(session.Record{Phase: session.PhaseActive}, true); p != "" {
		t.Errorf("active problem %q", p)
	}
}
