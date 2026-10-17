# Runtime checks at run admission and bounded replay records

Status: accepted
Date: 2026-10-17

## Problem and constraints

`AgentRuntime` rules (digest-pinned images, container security, approved
volumes, an immutable certificate-only trust bundle) were first checked when
the controller created the sandbox Pod. By then the run had taken the single
active slot. Every Pod create failed, the reconcile retried, and the run held
the slot until its execution deadline, blocking every queued run behind it. No
admission webhook exists for `AgentRuntime`. The CRD schema cannot express all
of these rules, and the trust bundle is a separate ConfigMap that can change or
disappear after the runtime is created.

The signed inbound event receiver records each accepted event ID as an
immutable ConfigMap so that replicas and restarts share replay protection. An
expired record was deleted only when the same event ID arrived again. Event IDs
are normally unique, so records accumulated without bound.

## Decision

Check the runtime when the run is admitted. `SandboxClient.ValidateRuntime`
applies the template rules and reads the trust bundle. The controller calls it
from `resolveAdmission`, the single step that also resolves references and the
session binding and evaluates policy. Every reconcile of a `Queued`, `Admitted`
or `Running` run goes through this step. A runtime that cannot launch fails the
run with reason `RuntimeInvalid` before it takes the slot. The `Ready` condition
message names the runtime and the broken rule. A later edit that breaks the
runtime of an active run fails that run, as a policy tightening already does.
Errors wrapping `ErrInvalidRuntime` are permanent. Other errors, such as a
failed ConfigMap read, are retried and do not fail the run.

`Ensure` and `EnsureManaged` no longer repeat the check. Their only caller runs
in the same reconcile, directly after `resolveAdmission`.

The event receiver sweeps expired replay records once per replay window. It
lists records by label in pages and deletes expired ones with UID and
resource-version preconditions, so concurrent replicas do not conflict. A
record therefore outlives its window by at most one more window. A record with
a missing or unreadable expiry is kept and keeps blocking its event. The replay
Role gains `list` for the sweep. The delete-on-collision path remains, because
a replayed ID can arrive before the next sweep.

## Alternatives and tradeoffs

A validating admission webhook for `AgentRuntime` would reject bad runtimes
earlier. It needs serving certificates and webhook infrastructure the
installation does not otherwise use. It cannot see a later trust bundle change,
and the controller would still need its own check. CEL rules on the CRD
cannot read the ConfigMap and would duplicate the Go rules in a second
language. A separate `AgentRuntime` controller that publishes readiness would
add a reconciler and a status contract, and runs would still need to check it.
Checking at admission reuses the existing per-run re-evaluation and reports the
problem on the object the operator is watching. The cost is that a bad runtime
surfaces only when a run uses it.

Keeping a second check at Pod creation would cost little, but the two checks
could drift and readers would need to know which one is authoritative.

For replay retention, sweeping on each accepted event would add a List to the
request path. A first-page-only sweep would also miss expired records beyond
that page. A Kubernetes TTL mechanism does not exist for ConfigMaps. Keeping the
replay keys in memory would lose protection across restarts and replicas. The
periodic sweep adds one goroutine and one paged List per window per replica.

## Revisit conditions

Revisit runtime checks if Sproozi adds admission webhooks for other resources,
or if operators need a runtime's status before any run uses it. Revisit replay
retention if event volume makes a full paged List per window expensive, or if
replay state moves out of ConfigMaps.

## Links

- [AgentRuntime reference](../reference/agentruntime.md)
- [AgentRun lifecycle](../reference/agentrun.md#lifecycle)
- [Webhook contract](../reference/webhook-contract.md)
- `internal/controller/reconciler.go` (`resolveAdmission`)
- `internal/runtime/kubernetes/sandbox.go` (`ValidateRuntime`)
- `internal/webhook/replay.go` (`SweepExpired`)
