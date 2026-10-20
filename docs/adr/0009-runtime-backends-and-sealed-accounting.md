# 0009: Runtime backends and sealed accounting

Status: accepted
Date: 2026-10-10
Reconciled: 2026-10-12
Implementation: sealed accounting used by verified Codex and Claude round trips

Partly superseded by [ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md): charging the whole reservation after provider uncertainty now applies only when no estimate is possible, or at a budget cut. An interrupted model call is charged an estimate capped at its reservation.

## Problem and constraints

Docker and Kubernetes execution require different provisioning and stop evidence.
Kubernetes stores accounting in ConfigMaps, while standalone local execution must
not require a cluster. Keeping backend code in shared packages makes their trust
and lifecycle responsibilities difficult to trace. The user explicitly requested
backend subpackages.

A move also needs trusted accounting that cannot become zero after an API failure,
race with a new reservation, or refund usage twice. Existing ledger observations
returned zero for Kubernetes read/decode errors. Native state and worker output
cannot establish a placement's usage or process-stop receipt.

## Decision and reasons

Keep execution implementations under `internal/runtime/kubernetes` and
`internal/runtime/docker`. Kubernetes registration belongs in the nested
`internal/runtime/kubernetes/registry` package. Shared native state, PTY,
checkpoint and workspace behavior remains independent of these backends.
API types, controllers and webhooks retain Kubebuilder scaffold locations.

The same boundary applies to Kubernetes gateway authentication, policy evaluation,
model credential storage and task workflow Run CRUD. Their concrete implementations
live in `internal/gateway/kubernetes`, `internal/policy/kubernetes`,
`internal/modelauth/kubernetes` and `internal/tasks/kubernetes`. Shared grant types,
DTOs, task HTTP handlers and accounting contracts do not import those backends.
Command entry points compose the concrete adapters. Caller migration removes the
old implementations rather than retaining compatibility aliases.

Keep accounting rules in `internal/budget` and Kubernetes persistence in
`internal/budget/kubernetes`. One ledger operates on opaque records through atomic
storage updates. The memory, protected file and ConfigMap stores share reserve,
settle, validation and seal rules; the ConfigMap adapter owns resource-version retries.
The protected file store in `internal/budget/file` supplies local persistence
without Kubernetes dependencies.
Migrate callers to these packages and remove the former paths and constructors.

Ledger observation returns an error for unavailable or corrupt records. Ordinary
unused accounts can report zero after confirmed absence. Initialized placement
allocations must require their existing record for every reservation; losing it
denies usage and sealing. The placement must initialize accounts before activation
and carry that requirement through gateway meter selection. Managed Docker and
Kubernetes grants now do this; ordinary Run behavior remains distinct from
managed allocations.

Seal atomically rejects subsequent reservations and retains an immutable charge
of used plus outstanding reserved units and cost. Later settlement can update
ordinary observed counters but cannot change the retained receipt. Repeated seal
returns that same receipt. Track pending reservations, including zero-sized ones;
sealing refuses unresolved usage without finite bounds. Old records without
pending-reservation metadata remain usable for ordinary accounting but cannot
produce transfer receipts without explicit reconciliation.

Audit `tokensRemaining` is nullable: `null` means no successful observation and
zero means an observed exhausted allowance. A post-response observation failure
does not undo an already settled response or invent a balance.

## Alternatives and tradeoffs

- Leaving backend implementations in shared packages avoids import migration but
  mixes authority and lifecycle boundaries and contradicts the requested layout.
- Separate accounting algorithms per backend keep storage code locally simple
  but duplicate pending, seal and validation rules. A small atomic-record seam is
  justified by the two durable stores and their shared behavioral tests.
- Reading usage after revoking identity is insufficient: an already authorized
  request can race the read. Seal competes with reserve on the same durable record.
- Refunding outstanding reservations after provider uncertainty risks duplicate
  spending. Charging their bound preserves safety at the cost of unused capacity.
- Treating legacy omitted counters as zero would improve compatibility but lose
  the distinction between known idle state and unknown reservation history.

## Accepted costs, evidence and revisit conditions

The move coordinator and deployed admission now use these foundations. Their
Codex and Claude roundtrip evidence and remaining support limits are recorded in
[ADR 0010](0010-managed-session-authority-and-admission.md).
ConfigMap deletion and administrative mutation remain trusted administrative
actions; managed requests must use the initialized-account contract to fail closed.
Zero cost ceilings remain unbounded; exhausted finite budgets must never be
encoded as the budget API's unbounded zero.

Tests cover competing ledger replicas, reserve versus seal, immutable receipt
retries, corrupt/unavailable/deleted records, overflow and unchanged provider
accounting. A real Docker lifecycle fixture exercises immutable container identity,
execution binding, engine stop observation and refusal to reopen a terminal
container. That early fixture was a prerequisite. The later production Codex
extended proof exercises real gateway use and denials, exact checkpoint state,
a held remote move and forced local cancellation with no competing destination.
Its interrupted request retained a conservative charge for the unresolved bound;
the immutable receipt replay did not refund that charge or reopen spending.
Successful provider token counts alone therefore do not equal the sealed charge.
The production Claude round trip also conserved its original deadline and sealed
320 units across eight deterministic Messages requests. A source transcript did
not establish those balances; the existing placement-owned ledgers and immutable
receipts did. Final-source matched-image Codex verification passed after the
writer-fence finalizer and actual-termination corrections. Claude's full held
placement move/cancel matrix remains unassessed; its completed native prerequisite
and ordinary production round trip cannot substitute for those observations.
Broader failures and native compatibility must keep their separate evidence.

Revisit conservative receipts when trusted provider settlement can resolve an
unknown reservation without reopening spending. Add another storage adapter only
when an actual deployment needs it. Revisit the package seams if backend types
leak into the shared coordinator or callers have to orchestrate backend internals.
