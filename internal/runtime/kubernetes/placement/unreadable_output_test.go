package placement

import (
	"errors"
	"testing"
)

// A stop that finds the native container ended but cannot read its output
// cannot tell a graceful stop from an exit, so it records an exit.
func TestKubernetesStopThatCannotReadTheOutputRecordsAnExit(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	endNative(t, p, kube, l, 0)
	e.logsErr = errors.New("pods/log denied")
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Exit == nil {
		t.Fatalf("unreadable output: exit %+v: %v", stopped.Exit, err)
	}
}
