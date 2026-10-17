# Further execution runtimes

**Status:** future extensions to implemented Kubernetes and controlled local Docker.
See the [session guide](../../guides/session-mobility.md) for the current narrow
Codex and Claude Code foreground workflows and limits.

Docker and Kubernetes have concrete placement adapters under
`internal/runtime/docker` and `internal/runtime/kubernetes/placement`. The
shared session coordinator tracks approved work, exclusive ownership, the
original deadline and the remaining reserved allowance. Backends retain their
actual admission, identity, storage, stop evidence and immutable usage receipts.
Ordinary Kubernetes Runs continue through the controller; local execution does
not fabricate an AgentRun. Both adapters use the same explicit selected native
kind and trusted launch contract. That implemented sharing does not establish
another runtime or native client's compatibility. [ADR
0009](../../adr/0009-runtime-backends-and-sealed-accounting.md) and [ADR
0010](../../adr/0010-managed-session-authority-and-admission.md) record these
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

## Testing requirements

See [runtime extension requirements](../../../test/acceptance/README.md#runtime-extensions)
for admission, isolation, stop, accounting and recovery checks.
