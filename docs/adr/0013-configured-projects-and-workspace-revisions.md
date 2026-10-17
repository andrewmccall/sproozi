# 0013: Configured Projects and exact workspace revisions

Status: accepted
Date: 2026-10-13
Implementation: verified through stock Hermes, native Kubernetes and local Docker development workflow

## Problem and constraints

The execution roadmap's lightweight foundation stage
requires optional Projects, independent reusable templates, scoped capabilities
and memberships, separate session listing, and reproducible Git workspace revisions.
Standalone sessions must remain valid. A Project association cannot become a
portable grant, and a committed revision cannot replace an unfinished workspace
or native conversation checkpoint.

[ADR 0012](0012-retained-work-and-fresh-session-continuations.md) already separates
product identity, writer ownership and fresh execution authority. Each configured
executor binds one administrator-owned template and placement. Lightweight grouping
must preserve those boundaries, immutable replay, one-way claim, capacity admission
and cleanup after configuration changes.

## Decision and reasons

Define Projects in administrator configuration. Each has a stable identifier,
an explicit installation-principal member list, a reference to the executor's
independently managed template, an optional approved workspace repository and an
explicit capability subset. Numeric ceilings come from that executor's current
configuration and remain subject to live placement policy. Selecting a Project
can narrow authority; it cannot enlarge it. Different templates use separately
configured fixed executors. The first design adds no multi-template routing owner.

Use the authenticated installation principal as membership identity. Task/home
tokens retain their existing roles; membership does not represent individual bot
users or transfer writer ownership. Check current membership before submission,
listing, direct-ID observation, export or user-facing cancellation/recovery, including
replay. Keep admitted association and repository source immutable. A Project edit
must not reinterpret an existing request or reveal its retained metadata to a
revoked principal.

Revocation must not disable trusted writer cleanup. On owner restart, nonterminal
work whose Project was removed or membership revoked follows the existing monotonic
cancellation path with a deterministic retained operation. Internal fencing and
accounting recovery remain available independently of public membership checks.
Keep template, instruction and placement configuration compatible while old writers
remain unresolved. Configuration is loaded by its owning service; this decision
does not introduce a live distributed membership service.

Carry optional Project/template/repository provenance through authenticated
catalogue and export records, including a credential-free repository reference
captured from trusted configuration. Importing that association supplies history
only. Local resume requires independent current home approval qualified by
source and Project, narrowing the fresh local grant. Standalone work retains its
existing approval path. Every local preparation/activation path, including
interrupted resume replay, recovery and movement, rechecks the source-qualified
approval. A narrowed current ceiling cannot rewrite an admitted allocation;
cancellation precedes a fresh smaller grant. Stop-only and cancellation recovery
remain available independently of approval. Membership and Project labels never
establish artifact authority.

Expose the selected workspace's full Git HEAD, object format and complete workspace
archive digest separately from native UUID and aggregate checkpoint digest. Bounded
manifest inspection reports declared metadata; inert restoration verifies actual
Git objects. The full archive still carries the index, working changes and selected
files under [ADR 0008](0008-selected-native-and-git-checkpoints.md). A recorded
repository reference or worker fetch instruction does not prove upstream provenance.

For a new execution at a recorded revision, select one immutable source using its
session ID, checkpoint digest and exact full recorded revision. Require current
source/target access, compatible repository identity and retained stopped/sealed
evidence. Reconstruct a clean detached workspace from retained Git objects in private
staging, excluding dirty/untracked state and native conversation history. Persist
the normalized workspace input and digest before destination preparation so retries
reuse it. Start a fresh native conversation with independently admitted authority.
This path is separate from same-conversation resume, which restores the full
unfinished workspace. Neither path publishes commits or grants GitHub access.

## Alternatives and rejection reasons

- A Project CRD or second mutable catalogue would support central reconciliation
  and status. It adds another lifecycle and synchronization boundary for grouping
  already-owned sessions. Administrator definitions and immutable association are
  sufficient for the current single-principal topology.
- Multiple templates behind one owner endpoint would simplify some operator
  configuration. It adds retained backend selection and routing to every lifecycle
  operation. Existing separately configured executors meet the current requirement;
  revisit when same-endpoint template selection is an explicit need.
- Per-person membership would support independent bot-user permissions. The current
  authenticated identity is an installation. Individual attribution needs its own
  authentication and authority design; a display name cannot establish it.
- Copying Project capabilities into portable state would simplify local setup but
  let source history determine destination authority. Independent home approval
  accepts extra configuration while preserving fresh authorization.
- Using only a branch name, URL or current repository alias is convenient but
  mutable and can exclude unpublished commits. An exact checkpoint/revision binding
  reproduces retained objects without requiring upstream fetch or a new Git service.
- Reusing full native resume for every revision launch preserves more state but
  conflates a clean committed checkout with unfinished work and conversation
  resume. Separate fresh workspace-only execution preserves both meanings.

## Accepted costs and consequences

Project membership is installation-level and configuration-based. Definitions and
home approvals require operator coordination. Moving between Projects or templates
does not bypass pinned instruction compatibility. Removed membership hides retained
data from public callers while trusted services still finish writer cleanup.

Unpublished commits remain reproducible while their retained objects are available;
this does not establish that another remote can fetch them. Revision normalization
needs private quota-backed staging and process limits, as ordinary workspace restore
does. It consumes storage and must respect existing retention admission before
creating a writer. Old receipts and source checkpoints remain immutable.

Shared homes, multiplayer, collaboration, HA, automatic publication and bespoke
Git/memory services remain separate directions. Project grouping does not relax
exclusive writer ownership or introduce shared workspace execution.

## Evidence and implementation reconciliation

Retained execution-roadmap grounding traced owner, checkpoint and home contracts.
Two designs compared fixed executors with same-endpoint template routing. The
independent cross-judge favored fixed executors; synthesis incorporated the other
design's digest-bound source, durable normalized input, metadata recomputation and
explicit staging ownership. Review also identified direct-ID access bypass and
membership revocation stranding cleanup as requirements to address. These are
design/review records, not end-to-end runtime acceptance.

| Material choice | Public record |
| --- | --- |
| Configured Projects and fixed reusable templates | This ADR's configuration decision |
| Installation membership, direct-ID/replay checks and safe revocation | This ADR's access and cleanup decision |
| Portable association and independent home approval | This ADR's provenance decision; ADR 0012's fresh authority |
| Exact revision versus dirty/native state | This ADR's workspace decision; ADR 0008's full checkpoints |
| Immutable source, durable input and staging bounds | This ADR's fresh revision execution decision |
| Exclusive claim, fencing, accounting and retention | ADRs 0009–0012, preserved here |
| Multi-template routing and a new CRD/store | Alternatives rejected above |
| Multiplayer, HA and automatic publication | Deferred with revisit conditions below |

The current implementation is in [Project
configuration](../../internal/project/project.go), [owner
selection](../../internal/work/project.go), [revision source
binding](../../internal/work/revision.go), [workspace revision
reconstruction](../../internal/workspace/revision.go) and [checkpoint
metadata](../../internal/checkpoint/codex.go). Local resume belongs to the
[existing home catalogue](../../internal/session/home/catalog.go). Current
configuration and callers are documented in the [Project/revision
reference](../reference/projects-and-revisions.md), with the [deployment
guide](../guides/retained-work-kubernetes.md) and repeatable [fixture
workflow](../../hack/verify/portable-workflows/README.md#development-and-governed-home-ops-projects).

The stock Hermes/native development proof passed actual Sproozi package tests in
Kubernetes and after same-conversation local Docker resume. It checked Project
filtering/association and launched another independently admitted native conversation
at the exact source checkpoint's recorded Git revision. The source checkpoint and
ownership remained unchanged. [Sanitized evidence](../../test/acceptance/reports/execution-roadmap-evidence.json)
records the public source snapshot and selected image identities. Deterministic
models establish execution/continuity behavior, not autonomous contribution quality.

Three independent reviewers reproduced an interrupted local approval bypass and
an interrupted legacy-claim upgrade failure. The fixes put current approval checks
on all activation paths and separate validated derived metadata from immutable
source acknowledgement. Independent overlay regressions and the home suite/race
checks pass. Stop-only Finalize and cancellation recovery remain available after
approval removal. These later fixes preserve the selected ownership/grant design;
they do not add a new authority mechanism.

## Revisit conditions

Add same-endpoint template selection when an actual workflow requires it. Revisit
configuration ownership when live membership changes, multiple principals or central
administration require a service or resource. Add individual identity only with
authenticated attribution and separately scoped authority. Broaden cross-repository
or cross-installation revision transfer with explicit provenance, retention and
access contracts. Revisit staging limits when a supported runtime supplies stronger
enforceable bounds. Collaboration or publication requires a separate decision rather
than interpreting Project membership as permission to share or publish a writer.
