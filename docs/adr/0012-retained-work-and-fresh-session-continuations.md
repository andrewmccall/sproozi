# 0012: Retained remote work and local session resume with fresh permissions

Status: accepted
Date: 2026-10-12
Reconciled: 2026-10-13
Implementation: retained Codex work and local resume verified; stock Hermes dispatch verified

Partly superseded: [ADR 0015](0015-default-clients-and-native-tool-schemas.md) replaces the pinned Codex task harness. Managed tasks still use Codex, installed at the upstream default release. Other decisions in this record remain in effect.

Superseded in part: [ADR 0018](0018-coordinator-owns-every-visit.md) replaces
reusing the initial home journal for a resumed execution's first visit. The
coordinator now owns that visit. Other decisions in this record remain in effect.

## Problem and constraints

Hermes must start useful native work in Kubernetes, observe its answer, and leave
the conversation and Git workspace available for a human to continue locally.
The original execution can finish before the laptop connects. An expired worker
deadline must not make retained conversation data unusable, and a later resume
must not reopen its grants or erase its consumption.

The existing home coordinator implements live moves within one bounded
execution. Its original deadline and remaining aggregate allowances are immutable.
Native completion, exact container termination, sealed usage and durable state
are separate facts. Those safety requirements remain unchanged.

## Decision and reasons

Co-host a remote work catalogue with the administrator's existing fixed placement
service. Its authenticated MCP tools accept an approved repository, harness,
executor and task. The catalogue retains immutable submission identity, execution
intent, bounded untrusted answers and receipt-bound native/workspace checkpoints.
New submission does not require a running laptop daemon. One protected directory
and process lock own this catalogue; it is not a new Kubernetes API resource.

The initial supported task harness is pinned Codex. A real positional task enters
its native TUI. Fresh native configuration marks only the admitted isolated
workspace trusted, avoiding an unattended folder prompt. Completion must belong
to the newly submitted task and a new native turn. Restored old completion cannot
satisfy it. Existing client-specific quiescence, exact writer fencing, account
sealing and checkpoint validation then retain the result before storage helpers
are released. Claude's existing interactive mobility remains separate from this
Codex task contract.

Use an explicit one-way home claim for completed work. Pull stores an inert
validated checkpoint first, then obtains and durably retains the authenticated
owner's claim acknowledgement. The remote catalogue retains a home-owned
tombstone and cannot reopen that product session. Repeating the same transfer
operation returns the same acknowledgement. Disappearance, expiry or a lost
reply never reclaims ownership. The registered principal represents one trusted
home installation; sharing its credential between independent homes is outside
this ownership model.

Local resume starts a new execution with its own identity, allocation and
deadline. It uses a current administrator-approved grant. The product session ID
and genuine native UUID remain stable. Retain prior execution records and
accounting receipts unchanged. Reuse the initial home journal with a checkpoint
input; only that new execution's real first stop enters the existing move
coordinator. No synthetic receipt, renewed old ledger or compatibility layer is
needed.

Bind each product resume, move, stop and cancellation operation to its original
internal execution and intent. A delayed request against the stable product ID
cannot acquire a later execution. Even an idle stop acknowledgement retains its
operation binding, so replay cannot stop a writer created afterward.

The claimed home can request later Kubernetes visits through the existing fixed
profile. Its live-move coordinator owns those visits; the remote task catalogue
does not regain task ownership. Stop-to-retained uses the coordinator's existing
validated stop path without preparing a destination.

Keep one private developer configuration for local launch, named remote profiles
and external capabilities. Git, shell and compilers are runtime contents.
The optional engineering image layers a pinned Go toolchain and public dependency
cache over the native image. Its build context admits only authored manifests
and the command wrapper; it does not copy a host home or credentials. Build
scratch space remains ephemeral and cached modules are not portable history.
GitHub, a specific Kubernetes API and named external MCP services remain gateway
boundaries. Reuse the existing semantic handlers in Docker, with gateway-owned
credential files and scoped policy. Regenerate placeholder Kubernetes client
configuration and approved Codex MCP connections on each launch. They grant no
access independently and are excluded from portable native state.

## Alternatives and rejection reasons

A laptop-owned catalogue reuses more home orchestration and preserves completely
standalone local operation. Exposing it to Hermes would make new remote dispatch
depend on laptop reachability. The selected remote owner meets unattended dispatch
without adding a publicly reachable laptop service. Existing standalone local
start and live move remain available.

A renewable deadline or allowance inside the old session record would reduce the
number of records. It would make every replay and accounting invariant depend on
renewal history and risk reopening consumed authority. Independent executions
keep the existing accounting proof intact.

An AgentSession CRD would provide Kubernetes reconciliation and CAS. It would
still need authenticated native operations and durable artifact transfer, and
would add another public API for this single-owner workflow. Protected owner
files reuse the current placement topology with less new machinery.

A permanent cluster lease for every local activation would avoid handoff, but
would make local resume depend on cluster reachability. Timeout-based lease
recovery cannot establish physical writer termination. Explicit one-way transfer
supports offline local execution and preserves conservative unknown states.

Scraping an ordinary completed worker's home would reuse the older task service.
Its container cleanup can remove state before capture, and its retained text
answer is not a native conversation or a writer/accounting receipt. Use the
managed placement pipeline rather than give callers two incompatible lifecycles.

## Accepted costs and consequences

The remote owner needs private durable storage and a single running process.
Catalogue and ownership history have bounded retention and require operator
management; deleting those journals is an administrative reset, not recovery.
The home catalogue bounds retained artifacts and journals by `retainedBytes` and
file count under one storage lock. New executions also reserve existing
initial-execution storage before preparation. Capacity rejection does not claim
a source or admit a new writer; recovery retries the original operation after
the operator restores capacity. One-way claim is sufficient for the first
workflow. Returning ownership, shared homes, federation, HA, session forks and
multiplayer remain separate work.

Native state retains its original instruction compatibility requirement. New
authorization changes grants and environment, not a pinned Codex conversation's
original trusted instructions. Unsupported histories and background tools still
fail closed. Unknown writer stop or missing usage prevents export and new writer
admission. Interrupted preparation can also retain conservative accounting even
when no final answer was produced.

GitHub branches remain owned by the current execution UID. A resumed local
session can preserve the old branch as history and create a new publishing
branch under `sproozi/$SPROOZI_EXECUTION_UID/`. Retaining a branch does not
grant permission to overwrite another execution's published branch. Kubernetes
remains read-only; reviewable GitOps changes use separately approved GitHub
access.

## Evidence and reconciliation

The product direction is the [shared delegation discussion](https://chatgpt.com/share/6acd0540-b080-83ed-bb95-31ecd3f3540a).
Two independent designs compared home-owned and remote-owned catalogues; a
separate judge favored the latter for laptop-independent dispatch. The selected
design grafted the home candidate's independent initial execution model and
explicit new-turn completion barrier. This narrows the owner topology for newly
submitted remote work without replacing ADR0004/0009's live-move accounting.

Implementation is in [the remote work owner](../../internal/work/owner.go),
[home pull and resume](../../internal/session/home/catalog.go),
[remote initial preparation](../../internal/runtime/kubernetes/placement/placement.go),
[native task observation](../../cmd/native-session/observer.go), and
[finalization without a destination](../../internal/session/finalize.go).
The [workflow guide](../guides/portable-workflows.md) states setup and support
limits. The [verification driver](../../hack/verify/portable-workflows/verify.py)
records actual native evidence and its fixture limitations.

The subsequent stock Hermes fixture discovered and invoked the actual
retained-work MCP tool, retrieved the completed session after its sandbox
stopped, and passed one-way pull and Docker resume with the same native UUID
under a fresh execution and deadline. A real stock native tool wrote and read
the controlled workspace file. This resolves the earlier absence of a fresh
Hermes harness proof. The [recorded
acceptance](../../test/acceptance/reports/verified-portable-workflows.md) separates this
deterministic-provider evidence from paid inference, real engineering
contribution and personal home-ops acceptance. It does not expand native-history
compatibility.

Live verification exposed an initial helper argument mismatch, default-branch
workspace restoration, native folder trust, blocking submission replay and
lost-preparation cancellation identity. Correct those at their owning contracts;
retain the failed roots and original allocations. Unit tests alone do not prove
Hermes usage, a real contribution, paid inference or a personal home-ops diagnosis.

## Revisit conditions

Revisit storage and ownership if multiple principals, independent homes, HA,
cross-installation transfers or return/fork workflows become requirements.
Revisit native task delivery when another pinned harness earns equivalent task,
foreground-tool, completion and same-conversation resume evidence. Revisit
configuration rendering when duplicate operator setup prevents a fresh
installation from reproducing the documented workflow.
