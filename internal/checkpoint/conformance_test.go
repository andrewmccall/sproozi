package checkpoint

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// conversationContract is what a managed kind's conformance fixture must
// give.
type conversationContract struct {
	// fixture names its entry in stateFixtures.
	fixture string
	// text is the fixture's final text. It may not be empty: a fixture with
	// no answer cannot show that FinalText reads one.
	text string
	// evidence: the state keeps approved instructions, so ValidateContinuity
	// refuses a digest they do not match.
	evidence bool
}

// conversationContracts is the conformance fixture of every managed kind.
var conversationContracts = map[harness.NativeKind]conversationContract{
	harness.NativeCodex:   {fixture: "codex-instructed", text: fixtureReply, evidence: true},
	harness.NativeClaude:  {fixture: "claude-code", text: "CLAUDE_FIRST_COMPLETE"},
	harness.NativeCopilot: {fixture: "copilot", text: fixtureReply},
}

// Every managed kind's conversation goes through the whole checkpoint
// format: its registry capture, CaptureStopped, Inspect, Restore and a
// capture of what Restore published, FinalText and ValidateContinuity. A new
// managed harness fails here until its state type, artifact field and
// fixture all exist.
func TestEveryManagedKindConformsToTheCheckpointFormat(t *testing.T) {
	fixtures := stateFixtures()
	repository, _, _ := source(t)
	for _, kind := range harness.ManagedKinds() {
		t.Run(string(kind), func(t *testing.T) {
			contract, ok := conversationContracts[kind]
			i := slices.IndexFunc(fixtures, func(f stateFixture) bool { return f.name == contract.fixture && f.kind == kind })
			if !ok || i < 0 || contract.text == "" {
				t.Fatalf("managed harness %q has no checkpoint fixture with a final text; add its state to stateFixtures "+
					"(internal/checkpoint/outcomes_test.go) and its contract to conversationContracts "+
					"(internal/checkpoint/conformance_test.go)", kind)
			}
			h, err := harness.Managed(kind)
			if err != nil || h.CaptureState == nil {
				t.Fatalf("managed harness %q has no CaptureState in internal/harness/registry.go", kind)
			}
			state := fixtures[i].state(t, repository)
			if state.Kind() != kind {
				t.Fatalf("the fixture state of %q is a %q state", kind, state.Kind())
			}
			turn, err := state.CompletedTurn(checkpointLimit)
			if err != nil || turn == "" {
				t.Fatalf("fixture has no completed turn: %q, %v", turn, err)
			}
			home := t.TempDir()
			if err = state.Restore(home, repository, checkpointLimit); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			captured, err := h.CaptureState(home, repository, state.Thread(), checkpointLimit)
			if err != nil || !sameConversation(t, captured, state) {
				t.Fatalf("CaptureState does not return the state Restore published: %v", err)
			}

			data, meta, err := CaptureStopped(t.Context(), kind, home, repository, state.Thread(), nil, checkpointLimit)
			if err != nil {
				t.Fatalf("CaptureStopped: %v; add the state's field to nativeArtifact and its case to setState and "+
					"state in internal/checkpoint/artifact.go", err)
			}
			if meta.Harness != kind || meta.ThreadID != state.Thread() || meta.TurnID != turn {
				t.Fatalf("checkpoint metadata %+v, want thread %q turn %q", meta, state.Thread(), turn)
			}
			if inspected, err := Inspect(data, checkpointLimit); err != nil || inspected != meta {
				t.Fatalf("Inspect: %+v, %v", inspected, err)
			}

			target := filepath.Join(t.TempDir(), "placement")
			if restored, err := Restore(t.Context(), data, target, checkpointLimit); err != nil || restored != meta {
				t.Fatalf("Restore: %+v, %v", restored, err)
			}
			target, err = filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			again, err := h.CaptureState(filepath.Join(target, h.NativeDirectory), filepath.Join(target, "workspace"),
				state.Thread(), checkpointLimit)
			if err != nil || !sameConversation(t, again, state) {
				t.Fatalf("the restored placement does not hold the same conversation: %v", err)
			}

			if text, truncated, err := FinalText(data, checkpointLimit); err != nil || truncated || text != contract.text {
				t.Fatalf("FinalText %q (truncated %v), want %q: %v", text, truncated, contract.text, err)
			}

			checkContinuity(t, data, kind, contract.evidence)
		})
	}
}

// checkContinuity checks ValidateContinuity of a kind's artifact: the
// approved digest passes, an unapproved one fails only where the state keeps
// evidence, and a malformed digest or another kind's launch fails.
func checkContinuity(t *testing.T, data []byte, kind harness.NativeKind, evidence bool) {
	t.Helper()
	if err := ValidateContinuity(data, kind, fixtureDigest, checkpointLimit); err != nil {
		t.Fatalf("ValidateContinuity refused the approved instructions: %v", err)
	}
	err := ValidateContinuity(data, kind, strings.Repeat("0", 64), checkpointLimit)
	if evidence != (err != nil) {
		t.Fatalf("ValidateContinuity of an unapproved digest: %v; the contract says evidence=%v", err, evidence)
	}
	if ValidateContinuity(data, kind, "not-a-digest", checkpointLimit) == nil {
		t.Fatal("ValidateContinuity accepted a malformed digest")
	}
	if ValidateContinuity(data, "not-registered", fixtureDigest, checkpointLimit) == nil {
		t.Fatal("ValidateContinuity accepted another kind's launch")
	}
}

// sameConversation reports that got carries want's conversation: the same
// thread, size, completed turn and final text.
func sameConversation(t *testing.T, got, want harness.ConversationState) bool {
	t.Helper()
	if got == nil || got.Kind() != want.Kind() || got.Thread() != want.Thread() || got.Size() != want.Size() {
		return false
	}
	gotTurn, err := got.CompletedTurn(checkpointLimit)
	wantTurn, _ := want.CompletedTurn(checkpointLimit)
	if err != nil || gotTurn != wantTurn {
		return false
	}
	gotText, err := got.FinalText(checkpointLimit)
	wantText, _ := want.FinalText(checkpointLimit)
	return err == nil && gotText == wantText
}
