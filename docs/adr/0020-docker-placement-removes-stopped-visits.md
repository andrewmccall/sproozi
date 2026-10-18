# The Docker placement removes a stopped visit's resources

Status: accepted
Date: 2026-10-18

## Problem and constraints

Each Docker visit creates an agent, a checkpoint helper and a network anchor
container, named `spz-<hash>-agent|helper|anchor`, and a tmpfs workspace volume
named `spz-<hash>-workspace`. The placement stopped these containers but never
removed them or the volume. A developer machine had 114 exited containers
after a week of sessions. The shared bridge network is configured by the
administrator and is not created per visit, so it does not leak.

The Kubernetes placement deletes the exact Pod after it records the stop. Its
`WriterFenced` record lets it treat a missing Pod as one it removed, and the
writer fence finalizer stops the Pod vanishing without its knowledge.

The Docker placement has no finalizer. A container can disappear because
someone else removed it, possibly while it was still running. So `Abort`
re-observes the exact agent container even after a stop is recorded: a
retained receipt is not current evidence that the writer stopped. Removal must
not weaken that rule. A missing container is evidence of a stop only when the
placement's own record says the placement removed it.

## Decision

The placement removes a visit's containers and workspace volume itself, inside
`Stop`, `Abort` and `AbortPreparation`, once the stop is durable. Removal does
not wait for the coordinator.

A visit is removable when its record is in phase `stopped` or `aborted` with
the writer fenced and a sealed receipt retained, or in phase
`preparation-aborting` with every created container stopped. A `stopped` record
already holds its retained checkpoint. A visit whose usage is still unknown
keeps its containers, so a later `Abort` can retry sealing against the same
container.

Removal has two record markers in the placement's private record:

1. The placement stops each recorded container again, confirming that Docker
   reports it exited, then saves `removing`.
2. It removes the agent, helper and anchor by their exact container IDs, then
   the volume after checking its recorded identity, then saves `removed`.

With `removing` saved, a container that is absent by name, or whose name now
holds a different container ID, is one this placement removed. A container that
is still present must be stopped before removal, so a container restarted by
someone else is stopped and removed rather than trusted. Each step is
idempotent, and a retry continues from the last saved marker.

After `removed`, `Stop`, `Abort` and `Inspect` answer from the retained record
without calling Docker. Before `removing`, nothing changes: `Abort` and
`Inspect` still require the exact container, and a missing one is an error.

A removal failure never turns a durable stop into an unknown stop. `Stop` and
the first `Abort` return the recorded result and leave removal for a retry. A
replayed `Abort` still fails if the exact agent container is present and cannot
be confirmed stopped.

## Alternatives

- **Remove after the coordinator finalizes the visit.** This needs a new
  placement method and coordinator calls on every stop, move, cancel and
  recovery path, plus recovery for a crash between finalizing and removing.
  The placement already knows when its own evidence is durable, as the
  Kubernetes placement does.
- **Remove without a record.** A later call could not tell this placement's
  removal from someone else's, so it would either trust absence, breaking the
  writer-fence rule, or fail forever after a successful removal.
- **Remove before usage is sealed.** The home gateway seals from its ledger and
  does not need the container, but a visit with unknown usage is the case an
  operator investigates. Keeping its containers costs little and keeps the
  physical fence observable for the sealing retry.
- **Sweep labelled containers on startup.** A sweep needs the same record to
  decide which containers are safe to remove, and it adds Docker work to
  `OpenPlacement`. It may still be added for removals that never get a retry.

## Tradeoffs and consequences

- A retried `Stop` that previously returned without engine calls now retries
  removal when `removed` is not yet recorded. Its result is unchanged.
- If removal fails after the coordinator has accepted the stop, and nothing
  calls `Stop` or `Abort` for that visit again, the containers and volume stay
  until removed by hand. They carry the `sproozi.com/execution` label.
- An aborted visit with unknown usage keeps its containers until an `Abort`
  retry seals its usage.
- Records gain two optional fields, `removing` and `removed`. The record
  version stays 2. Records written before this change have neither field and
  are removed on their next `Stop` or `Abort`.

## Revisit when

- Removal failures leave containers often enough to need a startup sweep.
- The coordinator gains a step after finalization that every placement must
  implement.
- Docker offers a way to stop a container being removed without the
  placement's knowledge, comparable to a Kubernetes finalizer.

## Links

- [ADR 0018](0018-coordinator-owns-every-visit.md) defines the stop outcomes
  this decision relies on.
- [ADR 0009](0009-runtime-backends-and-sealed-accounting.md) requires unknown
  stop or usage to keep the allowance held.
- Implementation: [Docker placement](../../internal/runtime/docker/placement.go),
  [engine](../../internal/runtime/docker/engine.go) and
  [record](../../internal/runtime/docker/placement_store.go).
- Tests: `internal/runtime/docker/cleanup_test.go` covers removal after stop,
  abort and cancelled preparation, replay from the record, a missing container
  without the record, and convergence after an engine failure. Live Docker
  removal was not run for this change.
- [Docker placement README](../../internal/runtime/docker/README.md) and
  [session mobility guide](../guides/session-mobility.md#recover-or-cancel).
