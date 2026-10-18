# 0008: Selected native and Git checkpoints

Status: accepted
Date: 2026-10-10
Reconciled: 2026-10-12
Implementation: selected Codex and Claude checkpoints verified through both placements

Current compatibility policy: [ADR 0015](0015-default-clients-and-native-tool-schemas.md)
replaces exact client-version selection and the combined MCP discovery schema.
Other decisions in this record remain in effect.

## Problem and constraints

The accepted [session outcome](0004-portable-sessions-and-bounded-executions.md)
requires native conversation continuity and an unfinished repository, including
Git history, staged and unstaged changes, and untracked files. Ignored files
transfer only when explicitly selected. A whole home or Git metadata directory
would also transfer configuration, hooks and possible credentials. A commit alone
would lose the distinction between staged and working changes.

The source must finish its current native turn and stop every workspace/native
writer before capture. A native completion event alone is not a process fence.
The checkpoint module does not own a Docker launcher, terminal attachment, move
coordinator or Kubernetes restoration. Those are now implemented separately,
under [ADR 0010](0010-managed-session-authority-and-admission.md).

## Decision and reasons

Keep native history opaque and select only the exact supported conversation file.
Codex 0.161.0 uses its thread rollout JSONL: validate UUID, recorded client version,
bounded file/path and final completed-turn event, rejecting native errors and
aborted or pending turns. Claude Code 2.1.27 uses its exact primary transcript:
validate UUID, version, primary chain, paired foreground Bash results and settled
assistant leaf. Its transcript supplies neither reliable turn completion nor
approved instructions. The supervisor separately observes the selected lifecycle
hooks, and graceful capture requires a persisted quiescence acknowledgement.
Lost acknowledgement can therefore block a Claude move even after the process
has stopped; forced cancellation remains available.

Version 2 artifacts require an explicit native kind and exactly one matching
state component. Version 1 decoding recognizes the former Codex artifact; new
launches never default an absent kind. Do not copy configuration, authentication,
hooks, plugins, caches or index databases. Destination configuration is supplied
separately. The placement must independently confirm all writers stopped.
These client-specific mechanisms and their alternatives are recorded in
[ADR 0011](0011-native-session-compatibility-and-completion.md).

A real three-home experiment using the stock CLI preserved the same native UUID
and cumulative conversation under different absolute workspace paths, transferring
one rollout file through the production adapter. Fresh provider ports and
credential-variable bindings were applied. Changing `developer_instructions`
through `exec resume` retained the original instructions; the fixture rejected
that attempted override. Require an unchanged trusted instruction digest for this
CLI path until an explicit native reconfiguration mechanism is proved. Claude's
approved instruction digest is retained in trusted launch intent and the same
instructions are supplied fresh on every visit, independently of transcript bytes.

Native history can contain developer-role records and remembered permissions.
It remains sensitive, agent-writable data. Resume does not semantically downgrade
those records to user text. Runtime containment and the destination's live gateway
policy must remain the authority; historical records cannot grant external access.

For the workspace, retain reachable Git history and refs in a Git bundle. Capture
the index as a private synthetic commit created in sanitized scratch state, while
recording the original HEAD separately. Transfer selected working files as bounded
raw bytes. Restore the original HEAD and captured index, then overlay the working
files. Do not change the original repository or copy its configuration/hooks.
Use rebuilt Git subprocess environments and disable ambient hooks, filters and
external diff behavior at the capture/restore seam.

Validate a versioned archive manifest, paths, sizes, digests and Git objects before
publishing a new destination. Restore into private staging and publish only to an
absent target. Initial unsupported layouts fail explicitly, including conflicts,
unborn/shallow/partial repositories, submodules, linked worktrees and selected
symlinks. Ignored paths outside the selection do not become checkpoint contents.

## Alternatives and tradeoffs

- Copying whole native homes or `.git` directories simplifies restoration, but
  transfers ambient authority and metadata unrelated to continuity. Selected
  state requires harness-specific compatibility and sanitized Git reconstruction.
- A common transcript avoids native file coupling, but would make Sproozi own
  context and native conversation semantics. Keeping the genuine rollout accepts
  version pinning and native historical roles instead.
- A new native thread with an imported textual summary avoids resume quirks, but
  does not satisfy the accepted same-conversation outcome.
- Staged and unstaged binary patches are compact and passed the first fixture
  recipe. A synthetic index plus raw working bytes makes restored file-size bounds
  clearer and avoids treating binary patch expansion as bounded archive data.
- A single snapshot commit is simpler but loses the index/working distinction.
- Rewriting native instructions in the rollout might appear to fix the override,
  but edits an opaque internal format and can change conversation semantics.
  Reject incompatible instruction templates until native support is proved.

## Accepted costs and limits

Native compatibility is tied to explicit client pins and supported foreground
tool records. Long or compacted histories, background tasks and subagents require
separate proofs. The native CLI may make auxiliary requests,
including title generation; those calls need ordinary gateway accounting too.

Workspace byte limits cover the encoded archive, selected files and inspected Git
object sizes. Git unpacking precedes object-size inspection. Untrusted restore
therefore requires quota-backed staging storage and process limits; an archive
size limit alone is not a hard disk-consumption bound. Initial layout exclusions
reduce compatibility and must remain visible.

Checkpoint helpers do not establish source revocation, exclusive ownership,
transport access control, budget/deadline continuity or crash recovery. Those
remain move-coordinator and runtime obligations.

## Evidence and reconciliation

Implementation: [native Codex state](../../internal/harness/codex_state.go),
[native Claude state](../../internal/harness/claude_state.go),
[aggregate checkpoints](../../internal/checkpoint/artifact.go),
[workspace capture](../../internal/workspace/workspace.go) and
[workspace restore](../../internal/workspace/restore.go). Pair restoration uses
private staging and publishes neither component after a failed restore. Native
adapter tests cover identity/version/path, supported records and incomplete state.
`make verify-native-session` exercises actual
Codex through the production capture/restore helper and local deterministic
providers. It does not contact paid providers or prove deployed gateway/CNI
behavior. The workspace tests reconstruct real fixture Git repositories.

Production Codex core and extended proofs restore exact Git history, HEAD/index,
staged/unstaged changes, untracked files and the explicitly selected ignored
binary. Excluded ignored files remain absent. The extended proof also preserves
an actual native tool's file effect and completed history across a held remote
move. The production Claude Code 2.1.27 round trip independently preserves its
exact selected transcript and unfinished workspace, including foreground Bash
output and explicit ignored selection. These controlled fixtures do not
establish every Git layout, arbitrary tools or long-history compatibility.

See [session mobility](../explanations/future-architecture/session-mobility.md)
and [harness portability](../explanations/future-architecture/harness-portability.md)
for the remaining delivery gates.

## Revisit conditions

Revisit the native pin/state allowlist when another client version has retained
same-ID resume evidence. Revisit the instruction constraint after native TUI or
explicit thread-resume reconfiguration proves replacement of destination trusted
instructions. Expand filesystem layouts only with observable unfinished-state
round-trip tests. Replace the scratch/storage bound when a real runtime can
supply a smaller, enforceable restore limit. Reconsider native ownership if long
histories or safe-turn shutdown cannot preserve the accepted conversation.
