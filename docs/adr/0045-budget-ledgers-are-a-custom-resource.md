# 0045: Budget ledgers are a custom resource beside the gateway

Status: accepted
Date: 2026-10-21

Partly supersedes [ADR 0041](0041-gateway-state-namespace-and-retained-state-owners.md):
its `sproozi-budget` namespace is removed.

## Problem and constraints

The gateway keeps one durable ledger per run and capability, and the remote
executor initializes and seals the ledgers of its managed sessions. Both were
ConfigMaps. Kubernetes RBAC grants ConfigMap access per namespace, not per
purpose, so a Role that let the gateway write ledgers in `sproozi-system`
would also let it rewrite its own egress profiles, MCP registry, pricing and
auth mode. ADR 0041 solved that with a separate `sproozi-budget` namespace.

That namespace held nothing else. It added an object to install and remove,
an extra RBAC surface for the executor (`update` on every ConfigMap there),
and ledgers that `kubectl` showed only as opaque JSON. A ledger must still be
updated atomically: every reservation, settlement and seal is one
compare-and-swap write, retried on conflict.

No installation exists to migrate: the only release was a test release and
has been deleted.

## Decision

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
  ConfigMap there, which is the property ADR 0041 needed. `BUDGET_NAMESPACE`
  and the executor's `budgetNamespace` default to `sproozi-system`.
- **Retention and recovery are unchanged.** The hourly prune, keyed by the
  `sproozi.com/run-uid` label and `spec.writtenAt`, and the minute sweep of
  expired leases work as ADR 0041 describes.
- **The executor's startup check** now verifies `budgetledgers` access for
  itself and the gateway. It still refuses the gateway's namespace as its run
  namespace, where it writes ConfigMaps, but not as its budget namespace.

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

## Consequences

- One namespace fewer in every install; `kubectl get budgetledgers -n
  sproozi-system` shows each account's run, capability, units and whether it
  is finalized.
- The gateway and executor hold write access to a resource type that only
  they use, rather than to every ConfigMap in a namespace.
- The API gains a fifth CRD under the alpha policy (ADR 0044). Users do not
  create ledgers; the reference page says so.
- Each charge is still one API write, as before.

## Revisit when

- Charge rates make one API write per settlement a measurable load on the API
  server.
- A ledger needs fields other components must read without decoding `state`.

## Links

- `api/v1alpha1/budgetledger_types.go`, `internal/budget/kubernetes/ledger.go`
- `config/rbac/shared_gateway_budget_role.yaml`,
  `examples/portable-workflows/kubernetes/owner.yaml`
- [BudgetLedger reference](../reference/budgetledger.md)
