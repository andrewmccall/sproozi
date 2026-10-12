# Further execution runtimes

**Status:** future extensions to implemented Kubernetes and controlled local Docker.
See the [session guide](../../guides/session-mobility.md) for the current narrow
Codex 0.161.0 and Claude Code 2.1.27 foreground contracts and their proof limits.

Docker and Kubernetes have concrete placement adapters under
`internal/runtime/docker` and `internal/runtime/kubernetes/placement`. The neutral
session coordinator carries approved intent, exclusive ownership, original
deadline and remaining escrow. Backends retain their actual admission, identity,
storage, stop evidence and immutable usage receipts. Ordinary Kubernetes Runs
continue through the controller; local execution does not fabricate an AgentRun.
Both adapters use the same explicit selected native kind and trusted launch
contract. That implemented sharing does not establish another runtime or native
client's compatibility.
[ADR 0009](../../adr/0009-runtime-backends-and-sealed-accounting.md) and
[ADR 0010](../../adr/0010-managed-session-authority-and-admission.md) record these
boundaries and accepted costs.

## Remote Docker and other environments

Remote Docker is a later placement option. It needs authenticated engine access,
independent durable accounting, exact container ownership and the same admission,
network and storage guarantees. A developer profile must not expose an engine
socket or general infrastructure credentials to the worker.

Other concrete requirements may justify stronger container isolation, existing
sandbox providers or virtual machines. Reuse their supported lifecycle APIs when
appropriate. Reject a run if a backend cannot enforce its requested filesystem,
network, resource or lifetime restriction. Placement preference cannot weaken
policy to make a run start.

Do not build a generic runtime plugin registry or scheduler before an actual
additional backend requires it. Compare supported guarantees and observable
failure behavior with the two current adapters first.

## Required proof for another backend

Run the same pinned native session under equivalent approved authority. Exercise
allowed work, denied capability and direct access, cancellation, original deadline,
immutable accounting and restart recovery. Confirm the entire writer stopped,
rather than accepting a terminal detach or process-group exit as the fence.

Prepare restored state while execution remains inert. Admit fresh destination
identity and authority, reject stale ownership, and demonstrate that an unknown
source stop or usage cannot release escrow or activate a competing writer.
Workspace persistence is independent of sandbox cleanup. State the actual host
and engine limits instead of implying identical isolation across deployments.

[Session extensions](session-mobility.md#proofs-before-implementation-claims)
retain the broader proof matrix. The existing fixed profile is a deliberately
narrow trusted-administrator boundary, not general enterprise federation.
