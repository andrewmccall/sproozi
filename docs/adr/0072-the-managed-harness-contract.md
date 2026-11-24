# 0072: A managed harness is registry facts and two contracts

Status: accepted
Date: 2026-11-21

Extends [ADR 0039](0039-one-harness-registry-and-the-mcp-requirement.md), and
changes one sentence of its 15 November correction: that behaviour (completion
observers, checkpoint formats and the parsing of Codex's `-c` overrides)
stays in code.

## Problem and constraints

ADR 0039 put every per-harness fact in one registry
(`internal/harness/registry.go`), and its 15 November correction made each
managed fact required, so a new managed harness never takes Codex's value.
It left behaviour in code: "completion observers, checkpoint formats, and the
parsing of Codex's `-c` overrides". `TestNoHarnessFactOutsideTheRegistry`
allowed eight scopes to branch on a kind as that behaviour.

Code with no shape let a new harness fall through to another's behaviour.
Read against the code before this change, a fourth managed harness that
passed every guard would have hit eight fall-throughs, none of them tested:

1. **Wait text.** `waitingFor` gave any kind it did not name Codex's journal
   check on Codex's home, so every graceful stop reported "to start its
   first turn", however far the turn had gone.
2. **Task baseline.** `activate` took Codex's completed-turn baseline for any
   task launch that resumed a conversation.
3. **Final text.** `checkpoint.FinalText` read only the three known state
   fields and returned an empty result, without an error, for any other.
4. **Continuity.** Every state but Codex's got Claude Code's rule, a check of
   the digest's form only, whatever the harness keeps.
5. **Model option.** `NativeArgsSelectModel` assumed `--model`; a client
   spelled otherwise would get the configured model beside the user's.
6. **Override syntax.** Only Codex's `-c` was parsed; a client with its own
   configuration override would pass a controlled key unrefused.
7. **Partial adapter.** A supervisor adapter with a function left unset
   compiled and passed the guards, then panicked at activation or stop.
8. **Restore and state check.** The artifact's restore and its check of
   which state it holds refused an unknown kind, but no guard required
   either; only capture was guarded.

Two more sat outside the managed packages: local setup gave any provider
name it did not know OpenAI's URL (`localsetup.ProviderFor`), and
`ModelProtocol.InspectionHost`, which only tests called, gave any protocol
but Anthropic OpenAI's host.

Three constraints shape any contract:

- **Package boundaries.** The supervisor runs in `cmd/native-session`, whose
  adapter functions take its `*server` and `internal/nativepty` types (which
  link creack/pty). The controller, gateway, CLI and remote executor link
  `internal/harness` and `internal/checkpoint` but never `nativepty`, and
  methods on `*server` cannot leave `package main`. `internal/nativeconfig`
  is imported by `api/v1alpha1`, which `internal/harness` imports, so it
  cannot read the registry.
- **Frozen bytes.** A checkpoint artifact's SHA-256 is its identity in
  journals and receipts. Its JSON is `version, harness, codex | claude |
  copilot, workspace`; a retained checkpoint must decode, and a new capture
  must encode as before.
- **Test seams.** The supervisor's lifecycle record replaces single adapter
  functions (`withAdapter`), and other tests replace one function at a time.

## Decision

A managed harness is a registry entry, which holds its facts and one capture
function, and two pieces of behaviour, each behind its own contract and
guard.

1. **The registry holds facts, including the syntax of its arguments, and
   the capture of its conversation.**
   - `ModelFlag`, required: the option that chooses the model (`--model`;
     Codex also `-m`).
   - `ConfigOverride{Flag, ControlledKeys, ModelKey}`: the option that sets
     one configuration key, with the keys Sproozi sets (Codex's
     `-c`/`--config`, read as [ADR 0073](0073-codex-harness-arguments-fail-closed.md)
     describes). It is data; one reader in `native_args.go` parses every
     declared override. A managed harness without one needs an entry with
     its reason in `configOverrideExempt` (`internal/harness/facts_test.go`).
   - `CaptureState func(home, workspace, threadID string, maxBytes int64)
     (ConversationState, error)`, required for a managed entry and refused on
     an AgentRun-only one.

   `ValidateNativeArgs` and `NativeArgsSelectModel` branch on no kind.
   `managedFactProblems` requires each fact.

2. **The conversation state contract** is the `harness.ConversationState`
   interface (`internal/harness/conversation_state.go`): `Kind`, `Thread`
   (the harness conversation ID), `CompletedTurn`, `Size`,
   `Restore(home, workspace, maxBytes)`, `FinalText` and
   `ValidateContinuity(instructionDigest, maxBytes)`. `CodexState`,
   `ClaudeState` and `CopilotState` implement it. `ValidateContinuity` is
   required, so a harness whose conversation holds no evidence of the
   approved instructions says so in its own method: Claude Code's
   transcript is no proof of them
   ([ADR 0011](0011-native-session-compatibility-and-completion.md)), and
   Copilot's state returns nil (see open questions).
   `checkpoint.ValidateContinuity` then checks the digest's form for every
   kind. `internal/checkpoint` keeps one JSON field per kind, so an
   artifact's bytes do not change: `setState` stores a state by its type,
   and `state` returns the artifact's one state, refusing none, two, or one
   whose `Kind` is not the artifact's harness. `CaptureStopped`, `Inspect`,
   `Restore`, `FinalText` and `ValidateContinuity` go through the interface.
   This keeps ADR 0008's rules for the conversation state (one selected file, an explicit
   kind, exactly one matching state, a version 1 decoder for Codex, no
   configuration copied).

3. **The supervisor adapter contract** is `nativeAdapter`
   (`cmd/native-session/launch.go`), a struct of function values with a
   guard, one entry per managed kind in `nativeAdapters`:
   - required: `prepare`, `observe`, `turnless`, `waiting`, `finish` and
     `settle`;
   - optional: `completion` (lifecycle diagnostics), with what leaving it
     unset means in `optionalAdapterFields`
     (`cmd/native-session/turnless_test.go`);
   - facts: `executable` (the registry's `Executable`), `quits`,
     `namesSession`, and `observesHooks`, which Claude Code's alone sets:
     `claudeHook` asks it instead of comparing the kind, and refuses Claude
     Code's hook command for every other kind.

   `TestEveryAdapterSetsEveryRequiredFunction` fails for a managed kind
   whose adapter leaves a required function unset; a function field added
   later is required unless it is named in `optionalAdapterFields`.
   `waitingFor` returns the adapter's text, and none for a kind without an
   adapter, which then never reaches a graceful stop. The adapter has no
   task baseline: `NativeLaunch.Validate` refuses a task launch that resumes
   a conversation, so a task's completed turn is always its own, and the
   supervisor's activation no longer takes Codex's baseline, which only that
   refused launch reached.
   Claude Code's and Copilot's adapter code is in `claude.go` and
   `copilot.go`; Codex's observers are in `codex.go` and `observer.go`, and
   its launch (`prepareCodexLaunch`, `codexArguments`) in `launch.go`.

4. **They are two because the packages are two.** The conversation state is
   read by every binary that handles checkpoints and holds no terminal code,
   so it lives in `internal/harness` beside the registry that captures it.
   The adapter reads the supervisor's own state, so it stays in
   `cmd/native-session`. Neither package can hold both.

5. **The adapter is a guarded struct of functions, not an interface.** Every
   method would take `*server`, because each harness's observer reads
   different fields of it, so an interface would add a type per harness and
   nothing the function values lack. The lifecycle record and other tests
   replace one function of one adapter; with an interface each would need a
   wrapper type. The guard fails in the same `go test` the contributor runs,
   so what the compiler would catch is caught one step later, by name.

6. **What remains a switch says so.** `behaviourSites`
   (`internal/harness/sites_test.go`) now lists only `nativeAdapters`, and
   `TestEveryBehaviourSiteNamesEveryManagedKind` requires each scope it
   lists to name every managed kind's constant. `nativeconfig.ValidateHarness`
   stays a string switch, guarded by `TestManagedKindsAreWired`.
   `localsetup.ProviderFor` refuses a provider it has no default URL for,
   and `InspectionHost` is removed.

The eight fall-throughs are each closed or named:

| # | Now | Fails in |
| --- | --- | --- |
| 1 | `waiting` is a required adapter function | `TestEveryAdapterSetsEveryRequiredFunction` |
| 2 | `NativeLaunch.Validate` refuses a task launch that resumes a conversation, for every kind ([ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md)), so no baseline is taken | `TestATaskLaunchResumesNoConversation` |
| 3 | `FinalText` is a required method; the fixture needs a final text | compilation; `TestEveryManagedKindConformsToTheCheckpointFormat` |
| 4 | `ValidateContinuity` is a required method; the fixture's evidence flag is tested, or a reason for none is required | compilation; the conformance test |
| 5 | `ModelFlag` is a required fact | `TestManagedKindsSupplyEveryFact` |
| 6 | `ConfigOverride` or an exemption with a reason | `TestManagedKindsSupplyEveryFact` |
| 7 | every adapter function is required unless named optional | `TestEveryAdapterSetsEveryRequiredFunction` |
| 8 | `Restore`, `CompletedTurn` and `Size` are required methods; a field without its `setState` case or `state` line fails the round trip | compilation; the conformance test |

For a new managed harness, what is missing fails here:

| Missing | Fails in |
| --- | --- |
| A registry entry, or its CRD enum value | `TestRegistryMatchesTheCRDEnum` |
| A fact, including `ModelFlag`, `ConfigOverride` and `CaptureState` | `TestManagedKindsSupplyEveryFact` |
| A state type's method | compilation, at its `CaptureState` (`capturedState[S ConversationState]`) |
| The artifact field, a fixture, or a state that does not round-trip | `TestEveryManagedKindConformsToTheCheckpointFormat` |
| An adapter, or a required function | `TestEveryManagedKindHasAnAdapter`, `TestEveryAdapterSetsEveryRequiredFunction`, and the entry in `nativeAdapters` (`TestEveryBehaviourSiteNamesEveryManagedKind`) |
| The options check | `TestManagedKindsAreWired` |

Its rows then appear in the records, each regenerated only in its own
commit: `internal/harness/testdata/registry-outcomes.json`,
`cmd/sproozi/testdata/wizard-outcomes.json`,
`internal/checkpoint/testdata/checkpoint-outcomes.json` (with the artifacts
in `checkpoint-artifacts/`), and in `cmd/native-session/testdata`
`waiting-outcomes.json`, `launch-arguments.json` and
`supervisor-lifecycle.txt`. A fake fourth kind added by the contributor
checklist, in a scratch copy of the tree, met each of these guards in turn
and passed every unit test in the harness, supervisor, checkpoint, CLI,
native configuration and local setup packages at the end.

## Alternatives

- **One interface spanning both packages**, a `ManagedHarness` with
  executable, home, prepare, observe, finish, capture and restore, one file
  per harness. It cannot live in one package: its supervisor methods take
  `*server` and `nativepty` types, and putting them in `internal/harness`
  would link the terminal code into the controller and gateway, while
  `*server` cannot leave `package main`. Per-visit state such as Claude
  Code's hook lifecycle would move into an adapter instance under the same
  mutex, and the lifecycle record could no longer replace single
  functions.
- **Exhaustive switches alone**: keep every switch, make each refuse a kind
  it does not name, and require each allowed scope to name every managed
  kind. Cheap, and decision 6 keeps it as a safety net for what remains. On
  its own it misses final text and continuity, which branched on which
  state pointer was set rather than on kind; the task baseline and Codex's
  home under a generic name, which named no kind; the `--model`
  assumption; and a partial adapter. It forces `case` lines that state
  nothing, and it proves no behaviour, which a fixture round trip does.
- **The review's original shape with a single launch builder**: the
  interface above plus one builder of the native launch for Docker and
  Kubernetes. The parts the two placements share already have one
  implementation (`NativeBaseEnvironment`, `NativeClients`,
  `NativeLaunch.Validate`); the two launch literals differ by design (local
  trust, profile and arguments against projected ones), so a builder would
  merge code that must differ. Deferred, not rejected, until a harness
  needs a launch field both placements set alike.
- **A generic artifact** (`{"kind": …, "state": …}`). A new harness would
  need no artifact field, but the encoding would change and every retained
  checkpoint would need another decoder; the field costs one line per
  harness.
- **Registry function values for each state operation instead of an
  interface.** Equivalent, but six function fields on every entry, unused
  by AgentRun-only harnesses, read worse than one `CaptureState` returning a
  typed state.

## Tradeoffs and consequences

- Besides each harness's MCP `Entry` renderer, which it already held, the
  registry now holds one more function, each managed harness's
  `CaptureState`, and Codex's override syntax as data; ADR 0039's 15
  November correction had kept both in code.
- A contributor writes a state type in `internal/harness` and an adapter in
  `cmd/native-session`; the [checklist](../contributing/add-a-harness.md)
  and the glossary's managed wiring name both.
- A missing adapter function is caught by a test, not by the compiler.
- The checkpoint encoding still lists one field per harness; a new harness
  edits `nativeArtifact`, `setState` and `state`.
- The restructuring changed no behaviour of the three harnesses: each
  record was written from the code before it and stayed byte-identical
  through it. The behaviour changes made alongside each changed its record
  only in its own commit: a kind without an adapter has no wait text
  instead of Codex's, a task launch that resumes a conversation is refused,
  Codex's arguments fail closed (ADR 0073), and `inspectionHost` left
  `registry-outcomes.json`.
- `StateProvesCompletion` stays optional: left unset, a stopped or exited
  process is not evidence of a completed turn and the stop needs the
  supervisor's acknowledgement, which is the conservative rule.

## Revisit conditions

- A harness's completion cannot be observed through `*server` and its
  journal, for example over a protocol connection instead of a terminal.
- A harness's conversation is more than one selected file, or restore
  needs more than a home and a workspace.
- `internal/nativeconfig` stops being imported by `api/v1alpha1`; its switch
  can then read the registry.
- The checkpoint format version changes for another reason; a generic state
  field can come with it.
- Quiesce polling measurably costs CPU or latency on large journals: each
  100 ms poll re-reads the whole journal, for at most two minutes.
- The source scan's known gaps, listed in its doc comment, hide a branch on
  a kind that a review finds.

## Open questions

These are recorded, not decided:

- Copilot's "no instruction evidence" rule may be wrong. ADR 0048 says
  Copilot adds `copilot-instructions.md` to its system prompt, and the
  fixture journals record a `system.message` with the whole system prompt at
  the first turn, but they were captured without an instructions file, so
  they cannot show whether approved instructions appear there.
- Must every managed harness declare its completion evidence
  (`StateProvesCompletion`), or is the conservative default enough?
- Login sync has two implementations: Go for managed sessions
  (`cmd/native-session/signin.go`) and Python for Claude Code AgentRuns
  (`claude-launch.py`).
- Codex has no settled-input wait before a stop: unlike Claude Code's and
  Copilot's observers, its observer does not consult `inputSettled`.
- Deferred until evidence asks for them: a single launch builder, an
  incremental journal fold, a shared Python helper module for the AgentRun
  launch files, a Copilot turn enum in place of its observer's booleans, and
  one conversation-ID pattern in place of three.

## Links

- Registry and arguments: `internal/harness/registry.go`,
  `internal/harness/native_args.go`
- Conversation state: `internal/harness/conversation_state.go`,
  `internal/harness/codex_state.go`, `claude_state.go`, `copilot_state.go`,
  `internal/checkpoint/artifact.go`, `internal/checkpoint/result.go`
- Supervisor adapter: `cmd/native-session/launch.go`,
  `cmd/native-session/codex.go`, `claude.go`, `copilot.go`,
  `cmd/native-session/homes.go`
- Guards: `internal/harness/registry_test.go`,
  `internal/harness/facts_test.go`, `internal/harness/sites_test.go`,
  `internal/checkpoint/conformance_test.go`,
  `internal/checkpoint/registry_test.go`,
  `cmd/native-session/turnless_test.go`
- Checklist: [add a harness](../contributing/add-a-harness.md)
- Related: [ADR 0008](0008-selected-native-and-git-checkpoints.md),
  [ADR 0011](0011-native-session-compatibility-and-completion.md),
  [ADR 0033](0033-harness-arguments-from-the-command-line.md),
  [ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md),
  [ADR 0073](0073-codex-harness-arguments-fail-closed.md)
