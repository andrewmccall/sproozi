package harness

// ConversationState is one managed harness's conversation as a checkpoint
// carries it: the selected native files, never configuration or authority.
// Each managed kind's state type implements it, and the kind's registry entry
// captures it (Harness.CaptureState). internal/checkpoint reaches every
// format through these methods; docs/contributing/add-a-harness.md is the
// checklist for a new one. (NativeState is the supervisor's lifecycle state.)
type ConversationState interface {
	// Kind is the managed kind whose state this is.
	Kind() NativeKind
	// Thread is the harness conversation ID (`threadId`).
	Thread() string
	// CompletedTurn validates the state and returns its last completed turn,
	// or the candidate settled leaf where the harness writes no completion
	// event (Claude Code). It is not evidence that the writer stopped.
	CompletedTurn(maxBytes int64) (string, error)
	// Size is the number of native bytes the state carries.
	Size() int
	// Restore publishes the state in an owned, prepared native home, for a
	// destination workspace at workspace. It never replaces an existing
	// conversation file.
	Restore(home, workspace string, maxBytes int64) error
	// FinalText is the conversation's last answer, untrusted workload
	// output; empty when the harness recorded none. It validates the state
	// as CompletedTurn does first, and returns that error for a state with
	// no completed turn.
	FinalText(maxBytes int64) (string, error)
	// ValidateContinuity compares the state's native evidence of approved
	// instructions with the trusted launch's digest. A harness whose state
	// keeps no such evidence returns nil and says why in its method. The
	// digest's form is checked by checkpoint.ValidateContinuity for every
	// kind, not here.
	ValidateContinuity(instructionDigest string, maxBytes int64) error
}

// capturedState returns a captured state as a ConversationState, and no
// state with an error. Its constraint makes a state type that lacks a method
// fail to compile at its registry capture.
func capturedState[S ConversationState](state S, err error) (ConversationState, error) {
	if err != nil {
		return nil, err
	}
	return state, nil
}
