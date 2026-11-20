package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

// checkpointOutcomesFile records what the checkpoint format does with each
// managed kind's fixture state and with an unregistered kind: the artifact's
// bytes, what Inspect, Workspace, Restore, FinalText and ValidateContinuity
// return for it and for broken variants of it, and what CaptureStopped
// captures. A change that only moves where the format's code lives leaves it
// byte-identical; regenerate it with SPROOZI_UPDATE_OUTCOMES=1 only for a
// change that means to alter an outcome, and show the difference in that
// change.
const checkpointOutcomesFile = "testdata/checkpoint-outcomes.json"

// checkpointArtifactDir holds each fixture's artifact as the format encoded
// it when the record was taken. The read outcomes are computed from these
// files, so a format change that can no longer read an artifact written
// before it fails here.
const checkpointArtifactDir = "testdata/checkpoint-artifacts"

// checkpointWorkspaceFile is a workspace archive captured once from the
// source fixture. workspace.Capture is not repeatable (its index commit
// carries the capture's time), so every recorded artifact reuses this one.
const checkpointWorkspaceFile = "testdata/checkpoint-workspace.tar"

// fixtureWorkspace is the workspace path the encoded fixture states name.
const fixtureWorkspace = harness.NativeSessionRoot + "/workspace"

// fixtureInstructions are the approved instructions of the Codex fixture
// that carries them; fixtureDigest is their digest.
const fixtureInstructions = "Approved fixture instructions."

var fixtureDigest = func() string {
	sum := sha256.Sum256([]byte(fixtureInstructions))
	return hex.EncodeToString(sum[:])
}()

// fixtureReply is the final text of the Codex and Copilot fixtures.
const fixtureReply = "Fixture reply."

const copilotFixtureID = "0a7c5e1e-3b2d-4c4f-9a8b-1f2e3d4c5b6a"

// codexInstructedRollout is a completed Codex turn that carries approved
// instructions and a final answer.
const codexInstructedRollout = `{"type":"session_meta","payload":{"id":"01a126e8-288d-7350-9b52-627e16048343","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + fixtureInstructions + `"}]}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture-turn"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Say the fixture reply."}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Fixture reply."}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture-turn","last_agent_message":"Fixture reply."}}
`

// stateFixture is one managed kind's recorded native state, built for the
// workspace path it names.
type stateFixture struct {
	name  string
	kind  harness.NativeKind
	state func(t *testing.T, root string) harness.ConversationState
}

func codexFixture(rollout string) func(*testing.T, string) harness.ConversationState {
	return func(*testing.T, string) harness.ConversationState {
		return harness.CodexState{ThreadID: fixtureThread,
			Path:    "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + fixtureThread + ".jsonl",
			Rollout: []byte(rollout)}
	}
}

func stateFixtures() []stateFixture {
	return []stateFixture{
		{"codex", harness.NativeCodex, codexFixture(codexFixtureRollout)},
		{"codex-instructed", harness.NativeCodex, codexFixture(codexInstructedRollout)},
		{"claude-code", harness.NativeClaude, func(t *testing.T, root string) harness.ConversationState {
			const id = "26386254-894d-4504-9bac-23ab929077cd"
			path, err := harness.ClaudeProjectPath(root, id)
			if err != nil {
				t.Fatal(err)
			}
			return harness.ClaudeState{ThreadID: id, Workspace: root, Path: path, Transcript: []byte(claudeCheckpointTranscript)}
		}},
		{"copilot", harness.NativeCopilot, func(t *testing.T, _ string) harness.ConversationState {
			events, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "copilot", "resumed-tool.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			return harness.CopilotState{ThreadID: copilotFixtureID, Events: events}
		}},
	}
}

// fixtureArtifact builds the artifact CaptureStopped would encode for state
// and the workspace archive.
func fixtureArtifact(t *testing.T, kind harness.NativeKind, state harness.ConversationState, archive []byte) nativeArtifact {
	t.Helper()
	a := nativeArtifact{Version: formatVersion, Harness: kind, Workspace: archive}
	if err := a.setState(state); err != nil {
		t.Fatalf("no artifact field for %T: %v", state, err)
	}
	return a
}

// restoreHome publishes state in home, for a workspace at root, as the
// harness would have written it.
func restoreHome(t *testing.T, home, root string, state harness.ConversationState) {
	t.Helper()
	if err := state.Restore(home, root, checkpointLimit); err != nil {
		t.Fatal(err)
	}
}

// Keys of the recorded outcomes.
const (
	keyError    = "error"
	keyMetadata = "metadata"
	keyText     = "text"
)

func outcomeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func digestOf(data []byte) map[string]any {
	sum := sha256.Sum256(data)
	return map[string]any{"sha256": hex.EncodeToString(sum[:]), "bytes": len(data)}
}

// continuityKinds are the kinds ValidateContinuity is asked about: every
// managed kind and an unregistered one.
func continuityKinds() []harness.NativeKind {
	return append(harness.ManagedKinds(), "not-registered")
}

var projectCharacters = regexp.MustCompile(`[^a-zA-Z0-9]`)

// restoredTree lists every path under target with its mode and, for a file,
// its digest. Claude Code's project directory, which encodes the restored
// workspace's path, is shown as -WORKSPACE-. Git's own directory is listed
// without its contents or mode: its index records file times.
func restoredTree(t *testing.T, target string) []string {
	t.Helper()
	project := projectCharacters.ReplaceAllString(filepath.Join(target, "workspace"), "-")
	var tree []string
	err := filepath.WalkDir(target, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(target, name)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		line := strings.ReplaceAll(filepath.ToSlash(relative), project, "-WORKSPACE-") + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:])
		}
		if relative == filepath.Join("workspace", ".git") {
			// Git creates it under the process umask.
			tree = append(tree, "workspace/.git")
			return filepath.SkipDir
		}
		tree = append(tree, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// readOutcomes is what every reader of an artifact returns for data.
func readOutcomes(t *testing.T, data []byte) map[string]any {
	t.Helper()
	o := map[string]any{}
	meta, err := Inspect(data, checkpointLimit)
	o["inspect"] = map[string]any{keyMetadata: meta, keyError: outcomeError(err)}
	archive, workspaceMeta, err := Workspace(data, checkpointLimit)
	o["workspace"] = map[string]any{"archive": digestOf(archive), keyMetadata: workspaceMeta, keyError: outcomeError(err)}
	text, truncated, err := FinalText(data, checkpointLimit)
	o["finalText"] = map[string]any{keyText: text, "truncated": truncated, keyError: outcomeError(err)}
	continuity := map[string]any{}
	for _, kind := range continuityKinds() {
		continuity[string(kind)] = map[string]string{
			"right":     outcomeError(ValidateContinuity(data, kind, fixtureDigest, checkpointLimit)),
			"wrong":     outcomeError(ValidateContinuity(data, kind, strings.Repeat("0", 64), checkpointLimit)),
			"malformed": outcomeError(ValidateContinuity(data, kind, "not-a-digest", checkpointLimit)),
		}
	}
	o["continuity"] = continuity
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "placement")
	restored, err := Restore(t.Context(), data, target, checkpointLimit)
	restore := map[string]any{keyMetadata: restored, keyError: outcomeError(err)}
	if err == nil {
		restore["tree"] = restoredTree(t, target)
	}
	o["restore"] = restore
	return o
}

// artifactVariants are broken or older encodings of a fixture's artifact.
func artifactVariants(t *testing.T, a nativeArtifact) map[string][]byte {
	t.Helper()
	encode := func(v any) []byte {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	variants := map[string][]byte{}
	for name, kind := range map[string]harness.NativeKind{"harness-unregistered": "not-registered", "harness-empty": ""} {
		changed := a
		changed.Harness = kind
		variants[name] = encode(changed)
	}
	managed := harness.ManagedKinds()
	other := a
	other.Harness = managed[(slices.Index(managed, a.Harness)+1)%len(managed)]
	variants["harness-other-managed"] = encode(other)
	two := a
	if two.Codex == nil {
		two.Codex = &harness.CodexState{}
	} else {
		two.Claude = &harness.ClaudeState{}
	}
	variants["two-states"] = encode(two)
	none := a
	none.Codex, none.Claude, none.Copilot = nil, nil, nil
	variants["no-state"] = encode(none)
	newer := a
	newer.Version = formatVersion + 1
	variants["version-3"] = encode(newer)
	older := a
	older.Version = 1
	variants["version-1-with-harness"] = encode(older)
	return variants
}

// fixtureArchive is the recorded workspace archive, captured from the
// source fixture the first time the record is taken.
func fixtureArchive(t *testing.T, update bool) []byte {
	t.Helper()
	archive, err := os.ReadFile(checkpointWorkspaceFile)
	if err == nil || !update || !os.IsNotExist(err) {
		if err != nil {
			t.Fatalf("%v; record it with SPROOZI_UPDATE_OUTCOMES=1", err)
		}
		return archive
	}
	repository, _, _ := source(t)
	if archive, err = workspace.Capture(t.Context(), repository, nil, checkpointLimit); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(checkpointWorkspaceFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(checkpointWorkspaceFile, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	return archive
}

// captureOutcomes is what CaptureStopped captures from a home holding the
// fixture's state, and what it refuses.
func captureOutcomes(t *testing.T, repository string, f stateFixture) map[string]any {
	t.Helper()
	state := f.state(t, repository)
	home := t.TempDir()
	restoreHome(t, home, repository, state)
	data, meta, err := CaptureStopped(t.Context(), f.kind, home, repository, state.Thread(), nil, checkpointLimit)
	o := map[string]any{"harness": meta.Harness, "threadId": meta.ThreadID, "turnId": meta.TurnID, keyError: outcomeError(err)}
	if err == nil {
		var a nativeArtifact
		if err = json.Unmarshal(data, &a); err != nil {
			t.Fatal(err)
		}
		o["native"] = digestOf(journalOf(t, a))
	}
	return o
}

// journalOf is the conversation bytes an artifact carries.
func journalOf(t *testing.T, a nativeArtifact) []byte {
	t.Helper()
	switch {
	case a.Codex != nil:
		return a.Codex.Rollout
	case a.Claude != nil:
		return a.Claude.Transcript
	case a.Copilot != nil:
		return a.Copilot.Events
	}
	t.Fatal("artifact carries no native state")
	return nil
}

// refusedCaptures are CaptureStopped's refusals of an empty home for each
// managed kind and of an unregistered kind.
func refusedCaptures(t *testing.T, repository string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kind := range continuityKinds() {
		home := t.TempDir()
		_, _, err := CaptureStopped(t.Context(), kind, home, repository, fixtureThread, nil, checkpointLimit)
		text := strings.ReplaceAll(outcomeError(err), home, "<home>")
		out[string(kind)] = strings.ReplaceAll(text, repository, "<repository>")
	}
	return out
}

// The checkpoint format's readers and capture return what they returned when
// the record was taken, for artifacts the format wrote then. A refactor of
// the format leaves the record, and the recorded artifacts, unchanged.
func TestCheckpointOutcomesAreUnchanged(t *testing.T) {
	update := os.Getenv("SPROOZI_UPDATE_OUTCOMES") == "1"
	archive := fixtureArchive(t, update)
	repository, _, _ := source(t)
	fixtures := map[string]any{}
	for _, f := range stateFixtures() {
		a := fixtureArtifact(t, f.kind, f.state(t, fixtureWorkspace), archive)
		encoded, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		// The version 1 format carried only Codex's state.
		recorded := map[string][]byte{"version-2": encoded}
		if a.Codex != nil {
			if recorded["version-1"], err = json.Marshal(codexArtifact{Version: 1, Native: *a.Codex,
				Workspace: a.Workspace}); err != nil {
				t.Fatal(err)
			}
		}
		o := map[string]any{"kind": f.kind, "capture": captureOutcomes(t, repository, f)}
		for version, data := range recorded {
			name := f.name + "-" + version
			file := filepath.Join(checkpointArtifactDir, name+".json")
			if update {
				if err = os.MkdirAll(checkpointArtifactDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(file, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			old, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v; record it with SPROOZI_UPDATE_OUTCOMES=1", err)
			}
			if !slices.Equal(old, data) {
				t.Errorf("%s: the format no longer encodes this state as it did; %s is the encoding when the record "+
					"was taken", name, file)
			}
			o[version] = map[string]any{"artifact": digestOf(old), "read": readOutcomes(t, old)}
		}
		variants := map[string]any{}
		for name, data := range artifactVariants(t, a) {
			variants[name] = map[string]any{"artifact": digestOf(data), "read": readOutcomes(t, data)}
		}
		o["variants"] = variants
		fixtures[f.name] = o
	}
	got, err := json.MarshalIndent(map[string]any{
		"fixtures":        fixtures,
		"refusedCaptures": refusedCaptures(t, repository),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if update {
		if err = os.WriteFile(checkpointOutcomesFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(checkpointOutcomesFile)
	if err != nil {
		t.Fatalf("%v; record the outcomes with SPROOZI_UPDATE_OUTCOMES=1", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a checkpoint outcome changed; %s records what each reader returned. If the change is meant, "+
			"regenerate it with SPROOZI_UPDATE_OUTCOMES=1 and show the difference in its own commit", checkpointOutcomesFile)
	}
}
