package session

// Kept is what a stopped visit leaves its session (ADR 0062).
type Kept int

const (
	// KeptCapture is a new checkpoint captured from the stopped visit.
	KeptCapture Kept = iota + 1
	// KeptInput is the checkpoint a resumed visit started from: nothing the
	// visit did is kept.
	KeptInput
	// KeptWorkspace is no checkpoint at all: a first visit leaves the
	// workspace it started from, which the execution retains.
	KeptWorkspace
)

// VisitEnd is what a placement knows, once a visit's writer is fenced, about
// how the visit ended.
type VisitEnd struct {
	// First is a first visit, started from a workspace rather than a
	// checkpoint.
	First bool
	// Turnless is a first visit whose stop reported no conversation (ADR
	// 0032).
	Turnless bool
	// Exited is a harness that ended by itself or at its deadline before the
	// stop's graceful handoff (ADR 0038).
	Exited bool
	// ProvesCompletion is a harness whose own state shows that its last turn
	// completed, so a stopped container may still be captured (ADR 0011).
	ProvesCompletion bool
	// Conversation is a native conversation the visit is known to have: the
	// resumed one, or the one a first visit bound.
	Conversation bool
}

// StopKeeps decides what a stopped visit keeps, the same way for every
// placement. keep is what the stop keeps before any capture. When keep is
// KeptCapture, onCaptureFailure is what the stop keeps instead if that
// capture fails, or zero when the failure fails the stop, so its retry
// captures again.
//
// A graceful stop proved its turn complete, so it captures, and a capture
// failure is the stop's failure. A harness that exited proved nothing by
// stopping: its visit is captured only when its own state proves completion
// and it had a conversation, and even then a capture that fails keeps what
// the visit started from rather than failing the stop for ever.
func StopKeeps(e VisitEnd) (keep, onCaptureFailure Kept) {
	start := KeptInput
	if e.First {
		start = KeptWorkspace
	}
	switch {
	case e.Turnless:
		return start, 0
	case e.Exited && (!e.ProvesCompletion || !e.Conversation):
		return start, 0
	case e.Exited:
		return KeptCapture, start
	}
	return KeptCapture, 0
}
