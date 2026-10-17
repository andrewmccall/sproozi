# 0010: Managed session authority and fresh admission

Status: accepted
Date: 2026-10-11
Implementation: verified for the controlled single-owner native session scope
Live Codex and Claude roundtrips: passed; scope recorded on 2026-10-12

Current compatibility policy: [ADR 0015](0015-default-clients-and-native-tool-schemas.md)
replaces exact client-version selection and the combined MCP discovery schema.
Other decisions in this record remain in effect.

## Problem and constraints

The accepted [portable-session
outcome](0004-portable-sessions-and-bounded-executions.md) requires a native
local terminal, remote work and return to local execution. Local launch must
work without a cluster. A native checkpoint carries conversation and workspace
continuity, but cannot carry permission to spend, destination policy, provider
credentials or proof that its source stopped. The developer must reach the
remote placement without Kubernetes credentials.

The retained design synthesis selected a home coordinator with placement-owned
admission and accounting. It rejected the alternative that required cluster
enrollment before every local launch. The implementation now needs one gateway
authorization model for both backends, without manufacturing an AgentRun for local
execution or granting authority from native history.

## Decision and reasons

Use a backend-neutral `ExecutionGrant` for resolved gateway authorization. It
contains the exact execution reference, immutable copies of policy/template
snapshots, approved capabilities and managed allocation account selections.
Authentication and revalidation remain backend-owned. Snapshot cloning prevents
callers from mutating retained authorization through shared maps or slices; it
does not make policy permanently exempt from revalidation. Ordinary Kubernetes
Runs keep their existing Run UID accounts and live checks. Managed grants require
explicit initialized accounts and cannot fall back to ordinary account creation.

A trusted local gateway owner retains each launch, allocation, exact Docker
container identity and token in a protected registry. It initializes all required
ledger records before activation and validates fixed local policy ceilings.
Missing, corrupt or replaced registry/account records deny managed access. A
first local launch starts with no invented native UUID; the trusted placement
binds the genuine observed UUID once. Returning placements use new containers,
allocations and fresh access while retaining the same logical session/native UUID.

Keep the home coordinator, local gateway and Docker adapters in one trusted home
daemon. Its gateway uses TLS and retains provider/MCP credentials outside worker
state. The worker receives a per-execution proxy token, placeholder provider
credentials and public trust certificates. The local Docker administrator and
engine are trusted. Worker socket replies, repository contents and native history
remain untrusted for authorization and accounting.

Remote access uses a fixed authenticated HTTPS profile with a per-principal token.
The service owns its Kubernetes credentials, namespace, approved template and
admission ceilings; the developer token cannot select Pods, images, environment
or Kubernetes credentials. Each destination creates a fresh AgentRun and follows
controller admission. A service-owned managed registry binds principal, operation,
epoch, checkpoint/native identity, original deadline, narrowed allowances and
actual Run/Pod UIDs. Restoration is inert. Activation requires that exact binding
and current admission; terminal Runs cannot reopen. Home authority cannot enlarge
remote policy. Direct authenticated service operations supply receipts without a
new signing or enrollment protocol.

The first native compatibility contract is stock Codex 0.161.0. Activation observes
the actual fixed executable version; checkpoint validation separately checks the
native UUID/version, completed final turn, supported resolved tools and actual
initial developer instruction digest. Native resume retained its original
developer instructions when a new launch override was attempted. Consequently,
placements must approve the same initial instruction digest. Administrators can
supply fresh model/native options, proxy configuration and public trust separately
from portable state. Those settings are launch configuration, not continuity
authority. They must not replace Sproozi-owned instructions or the update gate.
Changed public trust uses a new immutable ConfigMap identity. The fixture names
include the CA hash and change both runtime and gateway mounts together. A live
fresh-CA proof found that recreating the old name left an older mounted CA while
the API held the new bytes. Distinct names remove that cache ambiguity; process
trust still requires an observed new launch. Old fixture trust objects remain
until the dedicated cluster is removed.

A graceful move gates new terminal input and lets the admitted native turn and
tools finish while source gateway access remains available. Revoking first can
break a held model/tool request and prevent reaching the completed-turn boundary.
After native quiet, the placement revokes source authority and establishes the
actual runtime fence before capture: Docker reports the exact container stopped
with `Running=false` and PID zero, or Kubernetes reports the exact agent container
terminated. The separate trusted helper retains the workspace/native volume
through that fence and capture. Native `/quit`, journal completion and PTY
process-group exit cannot account for descendants that escape the group.
Cancellation remains a separate revoke-first forced stop.

Managed Kubernetes Pods carry the `sessions.sproozi.com/writer-fence` finalizer.
The profile persists termination observed for the exact Pod UID and native
container before releasing that finalizer. Graceful stop also retains the
checkpoint and receipt and finishes the capture helper first. Cancellation uses
normal UID-preconditioned deletion and waits for observed termination. A missing
Pod without previously retained termination evidence remains unknown: API
deletion does not establish that kubelet stopped its processes. See the
[Kubernetes Pod lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/).
Receipt replay checks the exact incarnation and uses that retained evidence if
the Pod has since disappeared. Ordinary execution Pods have no session finalizer.
The termination record must include its matching container ID and finish time;
Kubernetes' synthetic `ContainerStatusUnknown` termination does not qualify.
The [pinned kubelet conversion](https://github.com/kubernetes/kubernetes/blob/v1.33.1/pkg/kubelet/kubelet_pods.go#L1854-L1897)
explicitly distinguishes that inferred state from a CRI-reported exit.

Forced cancellation must interrupt an in-flight graceful wait before waiting for
the serialized journal lock. A pending cancellation ticket prevents a new move
from starting between that interruption and persisted cancellation. Disconnecting
a terminal or move client still leaves its durable operation running. Cancellation
is explicit, and a stopped or interrupted move never activates a destination
after that ticket has been accepted.

Usage is sealed atomically against new reservations on each required account.
Receipts retain used plus unresolved reserved bounds and replay immutably.
Forced stop seals already initialized accounts only after the exact container
or Pod is fenced. It retains a cancellation receipt without a checkpoint digest;
cancellation need not leave a transferable conversation. Missing or corrupt
accounts remain unknown and are never initialized as zero during recovery.
Receipt replay rechecks the exact physical fence before releasing escrow.

The placement supplies actual stop evidence; the gateway/accounting owner supplies
trusted usage. The coordinator subtracts each visit receipt once and carries the
original absolute deadline forward. Unknown stop or usage keeps the allocation
encumbered and blocks destination activation. A canceled graceful wait does not
reopen gated input or silently refund capacity. Lost activation replies recover
the retained exact intent through current admission, not a replacement allocation.

## Alternatives and accepted tradeoffs

- A cluster-owned session ledger simplifies central administration but makes
  standalone local execution depend on cluster availability. Placement-owned
  bounded allowances support disconnected remote work, at the cost of durable
  move/recovery records and conservative uncertainty handling.
- Requiring AgentRun-shaped local grants would reuse Kubernetes assumptions but
  invent a resource with no Kubernetes owner. A neutral resolved grant reuses
  capability enforcement while keeping actual lifecycle evidence backend-owned.
- Copying whole homes or allowing imported native settings would simplify resume
  but transfer credentials and ambient configuration. Selected checkpoints plus
  separate trusted launch configuration require native pinning and instruction
  compatibility checks. Rewriting opaque history or replacing the native thread
  with a summary would weaken the selected continuity contract.
- Immediate revocation is appropriate for cancellation, but interrupts graceful
  turn completion. Graceful move retains source access only within the existing
  allowance/deadline while input is gated; native/tool failure can still require
  abort and leave no transferable checkpoint.
- Reading counters after revocation avoids a seal API but races already admitted
  reservations. Atomic seals charge unresolved bounds conservatively, accepting
  unused capacity loss rather than permitting duplicate spend.
- Treating Pod absence as a fence simplifies cleanup but can admit another writer
  while the first still runs after forced deletion or loss of node contact.
  Retaining observable termination accepts stuck terminating Pods when the profile
  is unavailable. Restarting the same profile and recovering the retained operation
  releases its finalizer after evidence is durable. Administrator removal without
  that evidence leaves the allocation unknown; it cannot authorize a refund.
- SQLite, OIDC, application encryption, signed portable tokens and federation PKI
  can serve broader deployments. The selected direct trusted services use bounded
  protected atomic files and existing Kubernetes storage. These simpler mechanisms
  accept a single home owner and trusted administrators; they do not promise host
  compromise resistance or general multi-tenant federation.

## Evidence, limits and revisit conditions

Unit and race tests exercise immutable snapshots, managed registry/account loss,
reservation/seal races, receipt replay, same-incarnation activation recovery and
gated graceful completion versus forced cancellation. The stock native TUI fixture
exercises production PTY detach/reconnect, held-turn input rejection, completed
native quit and same-UUID fresh-home continuity. The dedicated enforcing-CNI Kind
fixture completed three genuine Codex turns across Docker, Kubernetes and Docker,
retained exact Git state and the original deadline, and sealed the fixture's
90 units across visits. Its final cancellation initially fenced the Pod but lacked
a usage receipt; recovery with the cancellation seal fix passed against that same
retained session. The public driver is
[`session-mobility/verify.py`](../../hack/verify/session-mobility/verify.py).
The later matched-image Codex proof passed real tool execution, held-turn
handoff, both-placement capability/direct-HTTP denials, forced cancellation and
immutable conservative receipt replay. Claude separately passed its complete
TUI/Bash/Git roundtrip and both-placement denials with exact usage conservation.
[Recorded acceptance](../../test/acceptance/reports/verified-session-mobility.md) retains their scope.
The fixtures do not establish arbitrary tools, compaction, all failure modes or
paid-provider compatibility.

Revisit the instruction restriction when a supported native mechanism can change
instructions without rewriting opaque history. Revalidate the native pin when
upgrading the client, including tool journals and terminal behavior. Broaden native
options only with tests that preserve the separately owned instruction/update
boundary. Revisit conservative charges when trusted provider settlement can
resolve uncertainty without reopening spending. Revisit storage and authenticated
profiles when actual multi-owner, enterprise identity or federation requirements
justify their additional coordination and trust costs.
Revisit finalizer retention if a backend can provide independent durable process
termination evidence across node partitions and administrator force deletion.

This decision refines the mechanisms in [ADR 0004](0004-portable-sessions-and-bounded-executions.md),
[checkpoint ADR 0008](0008-selected-native-and-git-checkpoints.md) and
[accounting ADR 0009](0009-runtime-backends-and-sealed-accounting.md).
Current boundaries live in [gateway grants](../../internal/gateway/grant.go),
[local authority](../../internal/gateway/docker/owner.go),
[Docker placement](../../internal/runtime/docker/README.md),
[remote registry](../../internal/runtime/kubernetes/session/registry.go),
[remote placement](../../internal/runtime/kubernetes/placement/placement.go),
[home composition](../../internal/session/home/compose.go),
[profile transport](../../internal/session/profile/server.go),
[native launch](../../cmd/native-session/README.md) and
[runtime client configuration](../reference/agentruntime.md).

## Subsequent decision

[ADR 0012](0012-retained-work-and-fresh-session-continuations.md) adds retained
remote work and later fresh executions under a stable product session ID. This
ADR's original deadline and aggregate allowance apply within each execution's
live moves. A new execution has current admission, new accounts and a new
deadline; it cannot reopen an old allocation or modify sealed usage. Exact
writer fencing and conservative unknown-state handling remain required in both
paths.
