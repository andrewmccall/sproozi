# 0004: Portable sessions across bounded executions

Status: accepted
Date: 2026-10-10
Reconciled: 2026-10-12
Implementation: controlled Codex and Claude Code round trips verified

## Problem and constraints

Developers should be able to continue a native coding conversation and workspace
between local Docker and an approved Kubernetes environment. They should not
need personal cloud accounts or a new agent UI. At the initial design, Docker,
session checkpoints and relocation were absent. AgentRun intent is immutable, worker identity
is disposable, and terminal Runs cannot resume. Persistent Hermes coordinator
state does not establish portable worker conversations.

This decision extends the boundaries in ADRs [0001](0001-mcp-capabilities-through-shared-gateway.md),
[0002](0002-native-cli-harnesses-and-model-protocols.md) and
[0003](0003-persistent-assistants-and-retained-results.md). It does not supersede
their implemented decisions. Logical session continuity remains separate from
the Kubernetes API identity of each bounded remote execution.

## Accepted outcome and initial scope

The 10 October 2026 design discussion selected the full local-to-Kubernetes-to-local
round trip. The same native conversation and unfinished workspace must continue
across it. Local execution, attachment and checkpoint proofs are prerequisites
within that destination; they do not replace it.

The user also accepted these independently revisitable scope decisions:

- [ADR 0005](0005-controlled-docker-launch-before-session-adoption.md): start
  through Sproozi in local Docker; defer existing-session adoption.
- [ADR 0006](0006-native-cli-and-terminal-before-gui-mobility.md): native CLI
  locally and terminal attachment remotely; defer native GUI/IDE integration.
- [ADR 0007](0007-codex-round-trip-before-harness-expansion.md): complete Codex's
  round trip first, then Claude Code before broader portability claims.

The user also selected a completed-turn checkpoint boundary: wait for the current
native turn and tools to finish rather than interrupting active work. Preserve
repository Git history, staged and unstaged changes, and untracked files. Ignore
ignored files unless the caller explicitly selects them. This accepts the delay
of a long turn and excludes live in-flight state transfer; revisit interruption
when a concrete workload cannot tolerate that delay and native safety is proved.

The direction was accepted before its move mechanisms were selected. The later
checkpoint, accounting and admission decisions are recorded in ADRs
[0008](0008-selected-native-and-git-checkpoints.md),
[0009](0009-runtime-backends-and-sealed-accounting.md) and
[0010](0010-managed-session-authority-and-admission.md). The two independently
verified native adapters are recorded in
[ADR 0011](0011-native-session-compatibility-and-completion.md); their acceptance
does not establish compatibility with other clients or native versions.

## Decision and reasons

A logical session tracks owner, native conversation identity, workspace continuity,
checkpoint references and execution location. It references successive bounded
executions. Each execution receives current admission and a fresh identity;
session continuity carries no standing capability grant.

[ADR 0012](0012-retained-work-and-fresh-session-continuations.md) extends retained
product sessions across independently authorized executions. Live moves within
one execution keep that execution's original deadline and aggregate allowance.
Later resume from retained state creates a new execution, deadline, allocation
and current grant while preserving the product session and native UUID. It does
not renew the previous execution's ledger or alter its receipts. The home-owned
live-move topology remains; newly submitted remote work starts in ADR 0012's
laptop-independent catalogue before an explicit one-way claim to the home.

Use authenticated attachment first, then checkpoint/restore at supported safe
boundaries. Preserve native harness state rather than inventing a shared
conversation format. Native GUI attachment remains a separate compatibility
proof. Initially launch through Sproozi; consider adopting existing native
sessions only after import and exclusive-writer control can be established.

Relocation must retain a recoverable checkpoint, prevent concurrent writers and
reconcile retries or uncertain activation before either side resumes. A staged
destination cannot run tools or obtain active external authority before source
revocation and exclusive ownership are established. Cancellation prevents pending
activation. Checkpoints transfer selected conversation/workspace state, not
provider credentials or whole user homes. Destination policy regenerates all
authority-bearing configuration.

Moves retain the original absolute deadline and subtract each immutable usage
receipt once. The home coordinator persists exact operation, epoch, allocation
and placement incarnation. Each placement owns admission, actual stop evidence
and its independent accounting. Unknown stop or usage keeps the allocation
encumbered and prevents another writer from activating. ADR 0010 records the
selected home-owned topology and fixed remote profile, including why a cluster
must not be required for standalone local launch.

## Alternatives and tradeoffs

- One long-lived AgentRun per session would reuse an existing API identity, but
  conflicts with immutable request intent, terminal cleanup and bounded worker
  admission. It would also retain authority while the session is inactive.
  Separate session continuity keeps these execution boundaries explicit.
- Reconnect-only operation avoids state transfer and is the first useful proof.
  It cannot move execution off a laptop or bring cluster work home, so it is a
  distinct experience rather than the final relocation design.
- Live process migration could preserve in-flight state more completely. Process,
  kernel and connection compatibility, credential-bearing memory and external
  side effects make it unsuitable for the initial portable-session boundary.
  Safe native checkpoint/restore accepts interruption and unsupported states.
- A universal Sproozi transcript would reduce format differences but requires
  owning context compaction, tool history and native client behavior. Native
  state keeps harness ownership intact at the cost of per-version compatibility.
- Copying user homes and credentials would simplify some resumes, but exports
  ambient authority into another trust domain. Selected state plus reissued
  authority requires stricter export/import checks and can reject existing sessions.
- Adopting arbitrary existing sessions immediately improves the developer
  experience, but cannot assume clean state or control of the original writer.
  Controlled launches are a narrower first proof; adoption remains a later goal.

## Accepted costs and consequences

This direction needs durable session and operation records, protected checkpoint
storage and ownership recovery beyond current disposable workers. Sensitive native
history changes storage and transfer responsibilities. Some harness versions,
workspace paths, tools and client surfaces will be non-movable or unsupported.

Stopping before destination activation causes an availability gap. A partitioned
source may block a move until writer exclusivity is established. A fresh Run alone
cannot provide aggregate accounting or safe replay of external side effects.
These costs remain part of the implemented contract. The controlled implementation
uses protected atomic journals and fixed administrator profiles; broader owner,
retention and enrollment requirements remain future extensions.

## Evidence and reconciliation

The [shared design conversation](https://chatgpt.com/share/6aca663c-73a0-83eb-896c-0471749672be)
requested a new session-mobility direction alongside revised Docker and harness
designs. Its visible reasoning separates reconnect, safe checkpoint/restore and
live migration, and separates portable sessions from bounded executions. The
attached ZIP could not be downloaded; these documents reconstruct the proposal
from the visible conversation.

The implementation now separates the [home service](../../internal/session/home/service.go),
[durable coordinator](../../internal/session/coordinator.go),
[Docker placement](../../internal/runtime/docker/placement.go) and
[Kubernetes placement](../../internal/runtime/kubernetes/placement/placement.go).
Terminal Runs remain terminal. Managed remote visits use fresh ordinary Runs and
independent service-owned registration. Hermes state still belongs to its separate
coordinator and supplies no worker-session portability evidence.

The deterministic Codex 0.161.0 core proof completed three genuine terminal turns
across Docker, Kubernetes and Docker. It retained the same native UUID, exact Git
state, selected ignored file, original deadline and sealed consumption. The later
extended proof passed actual native tools, held remote move, both placements'
capability/direct-access denials, forced cancellation and immutable receipt replay.
The final-source matched-image Codex rerun passed after the writer-fence finalizer
and actual-termination corrections. The production Claude Code 2.1.27 proof completed three genuine terminal
turns across Docker, Kubernetes and Docker, including foreground Bash execution.
It preserved the native UUID, Git/index/working state, untracked and selected
ignored files, the original deadline and 320 sealed units from eight deterministic
provider requests. Claude's both-placement denial probes passed separately from
its native held-turn prerequisite. Its held placement move/cancel matrix remains
unassessed. These short controlled histories do not establish compaction,
background tasks or every failure case. See the
[current guide](../guides/session-mobility.md) for support limits and drivers.

Related future docs: [session mobility](../explanations/future-architecture/session-mobility.md),
[execution runtimes](../explanations/future-architecture/execution-runtimes.md) and
[harness session adapters](../explanations/future-architecture/harness-portability.md).

## Revisit conditions

Revisit the session/execution split if a proven native protocol cannot preserve
conversation continuity through successive bounded executions. Revisit native
state ownership if supported cross-host export/resume cannot be established.
Consider process migration only when a concrete workload requires it and its
authority and side effects can be bounded. Expand adoption or GUI integration
after client acceptance tests prove it. Revisit the selected storage, fencing and
accounting mechanisms under ADRs 0008-0010 when deployment requirements exceed
their single-owner and trusted-administrator assumptions.
