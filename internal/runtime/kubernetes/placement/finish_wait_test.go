package placement

import (
	"context"
	"testing"
	"time"
)

// The checkpoint helper's reply grace (finishReplyGrace in cmd/checkpoint,
// pinned at 10 seconds by TestFinishReplyGraceIsTenSeconds): the longest the
// helper outlives a finish whose client never closes.
const helperReplyGrace = 10 * time.Second

// kubeletReportMargin is the least a stop must allow beyond that for the
// kubelet to report the helper's end: its relist, about a second, and the
// status write.
const kubeletReportMargin = 2 * time.Second

// A stop whose finish failed waits for the helper's end long enough for a
// helper that waited its whole grace and a kubelet that reported it at its
// next relist, and no longer than 15 seconds of the stop's budget. Guards:
// the production bound, which the fixture shortens. setupKind checks that
// an opened placement takes it.
func TestTheHelperEndWaitOutlastsTheHelpersGrace(t *testing.T) {
	t.Parallel()
	if helperEndWait < helperReplyGrace+kubeletReportMargin {
		t.Fatalf("helperEndWait %s is shorter than the helper's grace %s and a kubelet report %s",
			helperEndWait, helperReplyGrace, kubeletReportMargin)
	}
	if helperEndWait != 15*time.Second {
		t.Fatalf("helperEndWait is %s, not the 15 seconds ADR 0068 and the reference state", helperEndWait)
	}
}

// The wait is the placement's own bound, not the package's: a placement
// told to wait 50 milliseconds gives up on a helper that keeps running long
// before the production 15 seconds. Guards: that the wait reads the field.
func TestALostFinishWaitsThePlacementsOwnBound(t *testing.T) {
	t.Parallel()
	p, _, e, l, data := setup(t)
	p.helperEndWait = 50 * time.Millisecond
	i := activeVisit(t, p, l, data)
	e.finishOutcome = func(context.Context, string, string) error { return errCutStream }
	started := time.Now()
	if _, err := p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("a lost finish with the helper running was accepted")
	}
	if waited := time.Since(started); waited >= helperEndWait/2 {
		t.Fatalf("the stop waited %s with a bound of %s", waited, p.helperEndWait)
	}
}
