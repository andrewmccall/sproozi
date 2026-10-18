# 0007: Prove the Codex round trip before expanding harness support

Status: accepted
Date: 2026-10-10
Reconciled: 2026-10-12
Implementation: Codex core/extended and Claude Code production round trips passed
Sequencing: fulfilled; retained as historical rationale

Partly superseded: [ADR 0015](0015-default-clients-and-native-tool-schemas.md) replaces the use of pinned harness versions. Selected native state and the shared execution and permission boundaries remain in effect.

## Problem and constraints

Existing Codex, Claude Code, OpenCode and Hermes launch support did not establish
portable native state for any of them. Simultaneously implementing every resume
adapter could multiply compatibility work before a reliable round trip exists.
Conversely, a successful Codex move cannot justify universal harness portability.

## Decision

Prove the complete Codex local Docker-to-Kubernetes-to-local experience first.
Then repeat it with Claude Code before claiming portability across harnesses.
Codex was the first complete supported proof; Claude Code followed as an
independently verified extension. This sequencing did not make both a prerequisite
for the first Codex delivery. The second proof now establishes shared placement
boundaries with two explicit native adapters, not universal harness portability.

Use pinned harness versions and selected native state. Reuse execution and
authority boundaries while allowing different native resume adapters. Do not
build a universal conversation format or generic harness registry ahead of
evidence from the two implementations.

Codex completion requires conversation facts and unfinished workspace changes to
survive both moves, plus attachment, fresh authority, exclusive ownership and
failure recovery. A fresh task, one-way restore or successful remote command is
insufficient. Concrete pause behavior, transfer scope and recovery mechanisms
now have concrete implementations and controlled Codex evidence.

## Alternatives and tradeoffs

Claude Code first would be a reasonable sequence if its native export/resume
contract better met the intended developer workflow. Codex is the selected first
proof, subject to native compatibility evidence rather than an assumption that
its state is inherently more portable.

Requiring both in the first release would demonstrate the shared boundaries
earlier, but would couple delivery to two independent state-format and client
compatibility problems. Sequential complete proofs expose those differences
without reducing the first proof to a partial move.

Codex-only portability indefinitely would reduce maintenance but leave the
cross-harness design untested. Claude Code is retained as the second proof;
OpenCode and Hermes mobility remain later candidates rather than inherited
support from their existing bounded workers.

## Accepted costs and consequences

The sequence deferred Claude work until the first Codex proof. Early implementation
choices were reassessed during the second adapter rather than presenting Codex
state as a universal contract. Every supported combination needs explicit harness,
version, runtime and client evidence, with unsupported combinations visible.

## Evidence and reconciliation

The user accepted Codex's complete round trip first, followed by Claude Code,
during the 10 October 2026 grilling. Existing stock launch support is documented
in the [harness reference](../reference/harnesses.md).

Official [Codex app-server](https://learn.chatgpt.com/docs/app-server) and
[Claude session documentation](https://code.claude.com/docs/en/sessions) provide
native resume candidates. Claude also documents launch options that must be
provided again on resume. The inspected sources did not establish our cross-host
checkpoint contract; that requires empirical evidence for each pinned version.

Codex 0.161.0 now has a production three-turn Docker/Kubernetes/Docker core proof
with terminal interaction, selected native/Git state, unchanged deadline and
sealed usage. The extended proof passed actual native tools, a held remote move,
capability/direct-access denials in both placements, forced held-turn cancellation
and immutable accounting replay. The final-source matched-image rerun passed
after the writer-fence finalizer and actual-termination corrections. The
[verified demo](../../test/acceptance/reports/verified-session-mobility.md) records its exact scope.

Claude Code 2.1.27 separately completed three production terminal turns across
Docker, Kubernetes and Docker, including actual foreground Bash. The proof retained
the same UUID, cumulative conversation, Git history/index/working changes,
untracked and selected ignored files, the original deadline and immutable usage
receipts. Eight deterministic Messages requests accounted for 320 sealed units;
the public [Claude mobility driver](../../hack/verify/session-mobility/claude.py)
used no paid Anthropic inference. Its selected transcript and trusted
launch/completion path are recorded in
[ADR 0011](0011-native-session-compatibility-and-completion.md). The proof covers
short foreground histories and both-placement gateway/direct-access denials.
Claude's native held-turn prerequisite does not establish a complete held placement
move or forced-cancellation scenario; that matrix remains unassessed. Compaction,
background tasks and broader failures need separate evidence.

The second adapter requires an explicit checkpoint/launch discriminator and
Claude-specific completion observation. Sharing placement and authority contracts
does not turn Codex's completion marker or instruction checks into a universal
native contract. OpenCode and Hermes have no mobility claim from this work.

Related design: [mobility acceptance criteria](../../test/acceptance/README.md#session-movement-extensions)
and [harness adapters](../explanations/future-architecture/harness-portability.md).

## Revisit conditions

The Codex-first, Claude-second sequence is complete. Claude's different transcript
and completion requirements are reconciled in ADR 0011; neither proof establishes
universal portability. Revisit the shared adapter shape when another native
contract exposes a concrete difference. Add another harness only with its own
continuity, authority and failure evidence. GUI priority remains independently
revisitable under [ADR 0006](0006-native-cli-and-terminal-before-gui-mobility.md).
