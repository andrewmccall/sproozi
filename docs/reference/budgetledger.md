# BudgetLedger

`BudgetLedger` (`budgetledgers.sproozi.com`, `sproozi.com/v1alpha1`,
namespaced) holds one [budget ledger](../../GLOSSARY.md#budget-ledger) on
Kubernetes: the durable usage of one run for one capability. The gateway and
the remote executor create and update these resources. Users and
administrators do not create them; read them to see what a run has used.

The local service keeps its ledgers in files, not in this resource.

## Who writes it

| Writer | Namespace | Verbs | What it does |
| --- | --- | --- | --- |
| Shared gateway, through the `sproozi-shared-gateway-budget` Role | `BUDGET_NAMESPACE`, default `sproozi-system` | `get`, `list`, `create`, `update`, `delete` | Reserves and settles each charge, sweeps expired [leases](../../GLOSSARY.md#lease) and prunes settled ledgers of deleted runs. |
| Remote executor, through the example's `sproozi-work-budget` Role | `budgetNamespace`, default `sproozi-system` | `get`, `create`, `update` | Initializes the accounts of its managed sessions with their allowance, and seals them when an execution ends. |

Neither Role grants any ConfigMap access in `sproozi-system`, so neither
writer can change the gateway's configuration there. The gateway and the
executor must use the same namespace; the executor checks this at
[startup](../../cmd/session-profile/README.md#startup-checks).

There is no status subresource. Like a coordination `Lease`, the whole object
is written in one compare-and-swap update: every reservation, settlement,
sweep and seal reads the ledger, changes it and updates it with its
`resourceVersion`, retrying on conflict for up to 30 seconds.

## Key fields

```yaml
apiVersion: sproozi.com/v1alpha1
kind: BudgetLedger
metadata:
  name: sproozi-budget-3f1c9a0b7d2e4c6a8b10
  namespace: sproozi-system
  labels:
    sproozi.com/run-uid: 6d0f3c52-8a4e-4f0e-9a51-2b7f0c1d9e33
spec:
  account: 6d0f3c52-8a4e-4f0e-9a51-2b7f0c1d9e33/model.inference
  runUID: 6d0f3c52-8a4e-4f0e-9a51-2b7f0c1d9e33
  capability: model.inference
  state: "<encoded ledger state>"
  unitsUsed: 48211
  costMicros: 0
  finalized: false
  writtenAt: "2026-10-21T09:14:02Z"
```

| Field | Meaning | Limits |
| --- | --- | --- |
| `metadata.name` | `sproozi-budget-` and the first 20 hex digits of the SHA-256 of `spec.account`. | Set by the writer. |
| `metadata.labels["sproozi.com/run-uid"]` | The run UID. Pruning lists only ledgers with this label. | Absent when the account key has no valid run UID. |
| `spec.account` | The account key: the run UID and capability joined by `/`. | 1 to 512 characters. Immutable. |
| `spec.runUID` | The run whose usage the account holds. | At most 63 characters. Optional. |
| `spec.capability` | The account's capability, such as `model.inference`. | At most 256 characters. Optional. |
| `spec.state` | The encoded counters, reservations, leases and seal. Only the gateway and the remote executor interpret it. | 1 to 1048576 characters. |
| `spec.unitsUsed` | Settled units, copied from `state` in the same write. | At least 0. |
| `spec.costMicros` | Settled cost in micro-units, copied from `state` in the same write. | At least 0. |
| `spec.finalized` | `true` once the account is sealed and its usage can no longer change: its [finalized usage](../../GLOSSARY.md#finalized-usage). | |
| `spec.writtenAt` | When the ledger was last written, to the second. Retention counts from it. | Required. |

`unitsUsed`, `costMicros` and `finalized` are for display. They are computed
from `state` in the write that changes it, so they cannot disagree with it.
They don't include reservations still pending.

## Inspect ledgers

```sh
kubectl get budgetledgers -n sproozi-system
```

The printer columns are:

| Column | Field |
| --- | --- |
| `Run` | `spec.runUID` |
| `Capability` | `spec.capability` |
| `Units` | `spec.unitsUsed` |
| `Finalized` | `spec.finalized` |
| `Written` | `spec.writtenAt` |

To list one run's accounts:

```sh
kubectl get budgetledgers -n sproozi-system -l sproozi.com/run-uid=RUN_UID
```

Get `RUN_UID` with
`kubectl get agentrun NAME -n NAMESPACE -o jsonpath='{.metadata.uid}'`.

## Retention

Once an hour the gateway deletes a ledger when all of these hold:

- it has the `sproozi.com/run-uid` label, and no AgentRun with that UID exists;
- nothing is reserved on it;
- `spec.writtenAt` is older than `BUDGET_LEDGER_RETENTION` (a Go duration,
  default `168h`, at least `1h`).

A run that no longer exists cannot authenticate to the gateway, so its usage
is final. The delete carries the ledger's UID and `resourceVersion` as
preconditions, so a ledger written after the gateway listed it is kept. A
ledger without the label is never pruned.

Every minute, and when it starts, the gateway also sweeps every ledger in its
namespace and settles expired leases at their recovery charge. A ledger it
can't decode is skipped and logged as `Skipped corrupt BudgetLedger`. See
[security hardening](security-hardening.md#cleanup-and-state-retention).

## Don't edit or delete ledgers

Editing `spec.state` changes what the run has used. Deleting the ledger of a
run that still exists resets that account: an AgentRun's next charge creates
an empty ledger, and a managed session's next charge is refused because its
initialized account is gone. Restrict write access to `budgetledgers` to the
gateway and the remote executor. The install includes the scaffolded
`sproozi-budgetledger-editor-role` and `sproozi-budgetledger-admin-role`
ClusterRoles, bound to no one; don't bind them to anyone who is not an
administrator.

## Related

- [AgentPolicy budgets](agentpolicy.md) set the limits these ledgers count against.
- [Required secrets and config](required-secrets-and-config.md#state-and-deployment-controls)
  covers `BUDGET_NAMESPACE` and `BUDGET_LEDGER_RETENTION`.
- [ADR 0045](../adr/0045-budget-ledgers-are-a-custom-resource.md) records why
  ledgers are a custom resource.
