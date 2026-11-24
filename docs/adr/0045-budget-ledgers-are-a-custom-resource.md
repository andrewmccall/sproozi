# 0045: Budget ledgers are a custom resource, and the gateway and the executor each write only their own state

Status: accepted
Date: 2026-10-21

History: this record holds ADR 0041's decisions as well (2026-10-21), which it
began by replacing in part: the `sproozi-budget` namespace is gone, and the
rest of ADR 0041 moved here when that record was removed. ADR 0041's Docker
removal retry moved to [ADR 0020](0020-docker-placement-removes-stopped-visits.md).

## Problem and constraints

The gateway keeps one durable ledger per run and capability, and the remote
executor initializes and seals the ledgers of its managed sessions. Both were
ConfigMaps. Kubernetes RBAC grants ConfigMap access per namespace, not per
purpose. Two reviews on 2026-10-21 found these gaps:

- **The gateway could rewrite its own configuration.** The ledger lived in
  `sproozi-system`, and the Role that let the gateway write ledgers there
  granted `update` on every ConfigMap. The egress profiles, MCP registry, model
  pricing and auth mode are ConfigMaps in the same namespace. A compromised
  gateway could change them, and the change would become trusted configuration
  on the next restart. [ADR 0023](0023-controller-writes-only-in-sproozi-agents.md)
  fixed the same shape for the controller.
- **The executor had the same reach.** The remote executor example ran in
  `sproozi-system` with `get`, `create` and `update` on every ConfigMap there,
  so a compromised executor could rewrite the gateway's MCP registry and
  receive its bearer tokens after the next restart.
- **Nothing removed budget ledgers.** One ledger per run and capability
  accumulated for ever, and the sweep paged through all of them every minute.
- **Nothing expires session registrations.** Each managed session has a
  registration ConfigMap written by the executor in the run's namespace.

A first answer, ADR 0041, put ledgers in a separate `sproozi-budget`
namespace. That namespace held nothing else. It added an object to install and
remove, an extra RBAC surface for the executor (`update` on every ConfigMap
there), and ledgers that `kubectl` showed only as opaque JSON.

Constraints: a ledger must be updated atomically, as every reservation,
settlement and seal is one compare-and-swap write retried on conflict. A ledger
must never be removed while its usage could still change, and removing it must
not let a later call recreate it at zero. A durable stop must never become an
unknown stop because cleanup failed. No installation exists to migrate: the
only release was a test release and has been deleted.

## Decision

### Ledgers

- **A `BudgetLedger` resource** (`budgetledgers.sproozi.com`, `v1alpha1`,
  namespaced). Its `spec` holds the account key, run UID, capability, the
  encoded ledger state and display copies of units used, cost and whether the
  account is finalized, plus the last write time. The state encoding is
  unchanged, so the ledger rules in `internal/budget` are unchanged.
- **State in `spec`, no status subresource**, like a coordination `Lease`. The
  gateway and executor write the whole object in one compare-and-swap update;
  a status subresource would need two writes per charge. The display fields
  are computed from the state in the same write, so they cannot diverge.
- **Ledgers live in `sproozi-system`.** The gateway's Role there grants get,
  list, create, update and delete on `budgetledgers` and nothing else; the
  executor example's Role grants get, create and update. Neither can write a
  ConfigMap there. `BUDGET_NAMESPACE` and the executor's `budgetNamespace`
  default to `sproozi-system`, and the two must name the same namespace, as the
  executor initializes and seals the accounts the gateway charges. A manifest
  test fails if the gateway's `BUDGET_NAMESPACE` and the executor default
  diverge.
- **The gateway owns ledger retention.** Ledgers carry a `sproozi.com/run-uid`
  label and `spec.writtenAt`, updated on every write. Every hour the gateway
  lists every AgentRun and deletes a ledger only when its run is absent from
  that list, it has nothing reserved, no pending count and no lease, and it has
  not been written for `BUDGET_LEDGER_RETENTION` (default 7 days, at least one
  hour). The delete carries the ledger's UID and resource version, so a write
  after the list wins. A failed list deletes nothing. A run that no longer
  exists cannot authenticate at the gateway, so its usage is final, and stop
  receipts already copy the finalized usage. A ledger with no run UID label is
  kept. A minute sweep recovers expired leases.
- **Settling and recovery never recreate a deleted ledger.** An update that
  settles or recovers an account requires it to exist (`Existing` in
  `internal/budget/ledger.go`), so pruning cannot be undone by a late call.

### Namespaces and Roles

- **The gateway reads registrations, read-only.** It reads each managed run's
  registration ConfigMap in the run's namespace on every connection. A
  separate `sproozi-shared-gateway-registry` Role grants `get` on ConfigMaps
  only, bound in each namespace where managed runs live. The install binds it
  nowhere; the executor example binds it in `sproozi-sessions`.
- **The executor has its own namespace.** The executor, its ServiceAccount and
  Roles, its managed runs and their registrations live in `sproozi-sessions`,
  and CLI access defaults to that namespace. The executor's Role there grants
  ConfigMap `get`, `create` and `update`, which the administrator limits to
  registrations by creating the executor's profile and the CLI's trust anchor
  immutable. The executor's startup check refuses the gateway's namespace as
  its run namespace, where it writes ConfigMaps, but not as its budget
  namespace.
- **The executor's startup check** verifies its own permissions and the
  gateway's with access reviews (run-namespace ConfigMap `create` and
  `update`, `agentruns` `create`, `budgetledgers` `create` and `update`; the
  gateway's registration `get` and ledger `update`) and refuses to start
  naming the missing one.
- **The gateway warns at startup** if any Role still lets it write ConfigMaps in
  `sproozi-system`. It changes nothing; the administrator removes the binding.
- **Manifest tests** resolve every RoleBinding and ClusterRoleBinding and fail
  if the gateway, the executor or the CLI can write ConfigMaps or port-forward
  in `sproozi-system`, and check each API call on a managed session's path.

### Managed session ends

Separating the namespaces left two controller rules to settle, so a managed
session ends cleanly. `hack/verify/orchestration-kind/managed.py` drives a
managed session through the shipped executor example under the installed
identities.

- **A completed session stop is a normal end.** The run ends `Succeeded` with
  reason `SessionStopped` once its sandbox has finished, not as a binding
  conflict, so the controller does not release the sandbox while the executor
  is capturing. This holds when the controller first sees the stop after the
  execution's deadline (`resolveAdmission`, `finishStoppedSession`).
- **A cancelled registration ends the run `Cancelled`**, with reason
  `SessionCancelled`. An abort cancels the registration before it sets
  `spec.cancel`, and a reconcile between the two used to read the cancelled
  registration as a binding conflict, `SessionBindingRejected`. The gateway
  still refuses such a run.

### Retained records

- **Session registrations keep no automatic expiry yet.** They live in run
  namespaces and are written by the remote executor, which gives them no owner
  reference. A coordinator may still retry a `Stop` for an execution after its
  AgentRun is gone, and the record is what answers that retry with the original
  receipt. Removing it safely needs a bound on how long such retries can
  arrive, which the coordinator does not define. Until it does,
  [security hardening](../reference/security-hardening.md) documents how an
  administrator lists and removes them.

## Alternatives

- **Keep ConfigMaps in `sproozi-budget`.** Works, but keeps a namespace whose
  only reason is an RBAC limitation, and a broader executor grant.
- **ConfigMaps in `sproozi-system` with `resourceNames`.** Ledger names derive
  from run UIDs and are not known in advance.
- **A status subresource.** Conventional for observed state, but each charge
  would become a spec and a status write, and a ledger has no desired state.
- **An external store such as Redis or Postgres.** Higher write throughput, but
  a new dependency and a new trusted service, which current charge rates do
  not need.
- **Owner references from ledgers to AgentRuns.** Garbage collection would
  delete a ledger the moment its run is deleted, without checking for pending
  reservations.
- **Delete ledgers in the controller when it deletes a run.** The controller
  would need write access to the gateway's state, and it cannot see in-flight
  reservations.

## Consequences

- One namespace fewer in every install; `kubectl get budgetledgers -n
  sproozi-system` shows each account's run, capability, units and whether it
  is finalized.
- The gateway and executor hold write access to a resource type that only
  they use, rather than to every ConfigMap in a namespace. The gateway's
  ConfigMap write access no longer reaches administrator-owned configuration.
- The API gains a fifth CRD under the alpha policy (ADR 0044). Users do not
  create ledgers; the reference page says so.
- Each charge is still one API write, as before.
- The gateway lists every AgentRun once an hour. Its ClusterRole already grants
  that.
- The executor's Roles are not limited to its own runs. In `sproozi-agents` it
  can get, patch and delete any Pod, exec into it and read its logs, and in
  `sproozi-system` it can update any BudgetLedger. A compromised executor
  therefore holds the authority of every Kubernetes run, not only managed
  ones: it can use a sandbox's gateway token from inside the Pod, or reset a
  ledger. Keep the executor to clusters where all runs share one trust domain
  until managed sandboxes and ledgers have their own namespaces.
- Registration records still grow with managed runs.

## Revisit when

- Charge rates make one API write per settlement a measurable load on the API
  server, or an hourly full list of AgentRuns becomes expensive.
- A ledger needs fields other components must read without decoding `state`.
- The coordinator bounds how long it retries a stop, so registration records
  can be expired after that bound.
- More than one gateway replica writes ledgers (ADR 0026 keeps one).
- Runs from different trust domains share a cluster with a remote executor:
  give managed sandboxes and ledgers their own namespaces, so the executor's
  Roles cover only its runs.

## Links

- `api/v1alpha1/budgetledger_types.go`, `internal/budget/kubernetes/ledger.go`
  (`Prune`), `cmd/gateway/main.go` (`pruneLedgers`, `warnSystemConfigWrites`)
- `config/rbac/shared_gateway_budget_role.yaml`,
  `examples/portable-workflows/kubernetes/owner.yaml`,
  `cmd/session-profile/preflight.go`, `internal/controller/session.go`,
  `internal/controller/reconciler.go` (`finishStoppedSession`),
  `test/manifests/rbac_test.go`
- [BudgetLedger reference](../reference/budgetledger.md),
  [Security hardening](../reference/security-hardening.md),
  [required Secrets and configuration](../reference/required-secrets-and-config.md)
- [ADR 0023](0023-controller-writes-only-in-sproozi-agents.md),
  [ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md),
  [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md)
  (which also holds a cancelled registration's end)
