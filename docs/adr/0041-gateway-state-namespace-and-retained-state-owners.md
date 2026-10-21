# 0041: The gateway's state has its own namespace, and retained state has owners

Status: accepted
Date: 2026-10-21

Partly superseded: [ADR 0045](0045-budget-ledgers-are-a-custom-resource.md) replaces the `sproozi-budget` namespace with the `BudgetLedger` resource in `sproozi-system`; the gateway and the remote executor can write ledgers there but no ConfigMaps. Ledger retention, pruning and the executor's run namespace are unchanged.

## Problem and constraints

Two reviews on 2026-10-21 found three related gaps.

- **The gateway could rewrite its own configuration.** The budget ledger lived
  in `sproozi-system`, and the Role that let the gateway write ledgers granted
  `update` on every ConfigMap there. The egress profiles, MCP registry, model
  pricing and auth mode are ConfigMaps in the same namespace. A compromised
  gateway could change them, and the change would become trusted configuration
  on the next restart. ADR 0023 fixed the same shape for the controller.
- **Nothing removed budget ledgers.** One ledger per run and capability
  accumulated for ever. The sweep paged through all of them every minute.
- **Nothing finished a Docker removal.** `Stop` and `Abort` return success once
  a stop is durable and leave a failed removal for the next call (ADR 0020). A
  coordinator has no reason to make that call, so stopped containers and their
  volume could stay until someone ran `docker rm`.

Constraints: a ledger must never be removed while its usage could still
change, and removing it must not let a later call recreate it at zero (the
ledger refuses to recreate a deleted managed account; an ordinary run's account
could be recreated, but only by a request from that run, which cannot
authenticate once the run is gone). A durable stop must
never become an unknown stop because cleanup failed.

## Decision

- **A `sproozi-budget` namespace.** The install creates it with a Role and
  RoleBinding that let the gateway get, list, create, update and delete
  ConfigMaps there and nowhere else. `BUDGET_NAMESPACE` now defaults to
  `sproozi-budget`. The rotating ChatGPT credential stays in `sproozi-system`,
  read through `CREDENTIAL_NAMESPACE`, where the gateway's access is limited to
  the one named Secret (ADR 0026). A manifest test fails if any Role bound to
  the gateway can write ConfigMaps in `sproozi-system`.
- **The remote executor uses the same ledger namespace.** A managed session's
  accounts are initialized and sealed by the remote executor and charged by the
  gateway, so both must name one namespace. The executor's new
  `budgetNamespace` setting defaults to `sproozi-budget`, and its example Role
  there grants `get`, `create` and `update` on ConfigMaps. A manifest test fails
  if the gateway's `BUDGET_NAMESPACE` and the executor default diverge.
- **The gateway reads registrations, read-only.** The gateway reads each
  managed run's registration ConfigMap in the run's namespace on every
  connection. The old budget Role supplied that read by accident. A separate
  `sproozi-shared-gateway-registry` Role grants `get` on ConfigMaps only, bound
  in each namespace where managed runs live. (The first version bound it in
  `sproozi-system`; the second amendment below moved it.)
- **The gateway owns ledger retention.** New ledgers carry a
  `sproozi.com/run-uid` label and a `sproozi.com/written-at` annotation updated
  on every write. Every hour the gateway lists every AgentRun and deletes a
  ledger only when its run is absent from that list, it has nothing reserved,
  no pending count and no lease, and it has not been written for
  `BUDGET_LEDGER_RETENTION` (default 7 days). The delete carries the ledger's
  resource version, so a write after the list wins. A failed list deletes
  nothing. A run that no longer exists cannot authenticate at the gateway, so
  its usage is final, and stop receipts already copy the finalized usage.
- **The local service owns Docker removal retries.** `ReleaseStopped` walks the
  placement's records and finishes removal for every durably stopped or aborted
  visit not yet marked removed, under the same operation lock as `Stop`. The
  service runs it at start and every five minutes.
- **Session registry records keep no automatic expiry yet.** They live in run
  namespaces and are written by the remote executor. A coordinator may still
  retry a `Stop` for an execution after its AgentRun is gone, and the record is
  what answers that retry with the original receipt. Removing it safely needs a
  bound on how long such retries can arrive, which the coordinator does not
  define. Until it does, security hardening documents how an administrator
  lists and removes them.

### Amendment: managed sessions (2026-10-21)

The first version of this record moved only the gateway's ledger and removed
the old Role without replacing its registration read. Managed Kubernetes
sessions then failed authentication, and would have failed accounting, because
the executor still wrote accounts in its run namespace. A cross-model review
found it; the Kind e2e did not then drive a managed session. The two bullets
above on the executor and on registration reads are the fix. The Kind e2e now
runs one managed session through the shipped executor example under the
installed identities (`hack/verify/orchestration-kind/managed.py`).

### Amendment: the executor's own namespace (2026-10-21)

A later cross-model review found that the remote executor example still ran in
`sproozi-system` with `get`, `create` and `update` on every ConfigMap there, so
a compromised executor could rewrite the gateway's MCP registry and receive its
bearer tokens after the next restart: the threat this record removed from the
gateway. The CLI's port-forward Role in `sproozi-system` also reached the
gateway and controller. Now:

- The executor, its ServiceAccount, state, Service, managed runs and their
  registrations live in `sproozi-sessions`. The example binds the gateway's
  read-only registry Role there, and CLI access defaults to that namespace.
  The install binds no registry Role in `sproozi-system`, where no managed run
  lives.
- A manifest test resolves every RoleBinding and ClusterRoleBinding and fails
  if the gateway, the executor or the CLI can write ConfigMaps or port-forward
  in `sproozi-system`, and checks each API call on a managed session's path.
- The executor checks its own and the gateway's permissions at startup with
  access reviews and refuses to start with the missing one named.
- The gateway warns at startup if any Role still lets it write ConfigMaps in
  `sproozi-system`.
- A completed session stop no longer reads as a binding conflict: the run ends
  `Succeeded` with reason `SessionStopped` once its sandbox has finished, so the
  controller no longer releases the sandbox while the executor is capturing.
  This holds when the controller first sees the stop after the deadline.

### Amendment: cancelled sessions (2026-10-21)

An abort cancels the session registration before it sets `spec.cancel` on the
run. A reconcile between the two read the cancelled registration as a binding
conflict and ended the run `Failed` with `SessionBindingRejected`. The
controller now ends a run whose own registration is cancelled as `Cancelled`
with reason `SessionCancelled`. The gateway still refuses it, as before.

## Alternatives

- **Scope the Role with `resourceNames`.** Ledger names are derived from run
  UIDs and not known in advance.
- **Owner references from ledgers to AgentRuns.** Garbage collection would
  delete a ledger the moment its run is deleted, without checking for pending
  reservations, and ledgers live in another namespace, where owner references
  across namespaces are not allowed.
- **Delete ledgers in the controller when it deletes a run.** The controller
  would need write access to the gateway's state, and the controller cannot see
  in-flight reservations.
- **A periodic Docker sweep with no lock.** It could race a concurrent `Stop` for
  the same visit. Reusing the operation lock and the existing idempotent
  `release` avoids a second removal path.

## Consequences

- The gateway's ConfigMap write access no longer reaches administrator-owned
  configuration.
- The executor's Roles are not limited to its own runs. In `sproozi-agents` it
  can get, patch and delete any Pod and exec into it, and in `sproozi-budget` it
  can update any ledger. A compromised executor therefore holds the authority
  of every Kubernetes run, not only managed ones: it can use a sandbox's
  gateway token from inside the Pod, or reset a ledger. Keep the executor to
  clusters where all runs share one trust domain until managed sandboxes and
  ledgers have their own namespaces.
- The gateway lists every AgentRun once an hour. Its ClusterRole already grants
  that.
- Docker cleanup converges within five minutes of a transient engine failure.
- Registry records still grow with managed runs.

## Revisit when

- The coordinator bounds how long it retries a stop, so registry records can be
  expired after that bound.
- Ledger volume makes an hourly full list of AgentRuns expensive.
- More than one gateway replica writes ledgers (ADR 0026 keeps one).
- Runs from different trust domains share a cluster with a remote executor:
  give managed sandboxes and ledgers their own namespaces, so the executor's
  Roles cover only its runs.

## Links

- `config/rbac/shared_gateway_budget_role.yaml` (in config/budget-state until ADR 0045), `cmd/gateway/main.go` (`pruneLedgers`),
  `internal/budget/kubernetes/ledger.go` (`Prune`),
  `internal/runtime/docker/release_sweep.go`, `internal/session/home/release.go`
- [Security hardening](../reference/security-hardening.md),
  [required Secrets and configuration](../reference/required-secrets-and-config.md)
- [ADR 0020](0020-docker-placement-removes-stopped-visits.md),
  [ADR 0023](0023-controller-writes-only-in-sproozi-agents.md),
  [ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md)
