# 0042: Idempotent webhook delivery and one response before verification

Status: superseded by [ADR 0043](0043-remove-the-event-webhook.md)
Date: 2026-10-21

## Problem and constraints

Two reviews on 2026-10-21 found four problems in the webhook receiver.

- **A failed creation lost the event.** The receiver recorded the event ID, then
  created the AgentRun. If creation failed, a retry with the same ID was refused
  as a replay. A sender could only retry with a new ID, which duplicates work
  when the first creation had in fact succeeded behind a lost response.
- **Store errors looked like replays.** Any replay-store failure, including an
  API outage, returned `409` with the internal error in the body.
- **Expired-record cleanup could recurse without bound.** When an expired record
  could not be deleted, the store ignored the error and called itself again.
- **Unauthenticated callers could map receivers.** The key belongs to the
  receiver, so the template and Secret must be read before the MAC can be
  checked. Lookups returned `404`, `409` or `500` before verification, telling
  any caller which receivers existed and how they were configured.

Constraints: replay protection must still refuse a delivered event within its
window, across replicas and restarts. A sender cannot be trusted to choose run
names.

## Decision

- **The run name comes from the event.** The AgentRun is named
  `<template>-<16 hex>` from the replay key of namespace, receiver and event ID,
  with the template part cut so the name fits a 63-character label value. The
  run carries the replay key in `sproozi.com/webhook-event`.
- **The replay record has a delivery state.** A claim creates the record as
  `pending`. After the AgentRun exists, the receiver marks it `delivered` with
  the run's name. A claim that finds `delivered` is a replay (`409`). A claim
  that finds `pending` finishes the delivery: creating the run again either
  succeeds or finds the existing run by name, checks its event annotation and
  returns it with `200`. A failed mark leaves the record pending, which the next
  delivery repairs. Records from before this change, with no state, count as
  delivered.
- **Errors have their own statuses.** Store failures return `503`, creation
  failures `500`; bodies are fixed messages and causes go to the audit record.
- **The claim loop is bounded.** It tries at most three times and returns a
  delete failure instead of retrying it.
- **One answer before verification.** Every failure before the MAC is confirmed
  returns `401`: missing or ambiguous receiver, missing Secret annotation,
  missing or short key, and a bad signature. Only a transient API failure
  returns `503`. The audit record names the real cause.
- **Event context is bounded at the door.** More than 64 keys or 16 KiB of JSON
  returns `400` before a record is claimed, matching the agent contract and the
  new CRD limit.

The webhook gains `get` on AgentRuns and `update` on replay ConfigMaps.

## Alternatives

- **Delete the replay record when creation fails.** A crash between the failure
  and the delete loses the event the same way, and a creation that succeeded
  behind a lost response would be repeated.
- **Use `generateName` for runs.** Safe against long template names but gives
  every delivery a new run, so retries duplicate work.
- **Verify the MAC before any lookup with a cached key.** Saves API reads for
  forged requests, but a cache adds staleness to key rotation. The uniform
  `401` removes the information leak; rate limiting at ingress bounds the cost.

## Consequences

- Senders can retry any `5xx` with the same event ID and get exactly one run,
  with one exception: if marking the record delivered failed and the run was
  then deleted within the replay window, a retry creates the run again. That
  needs two failures within ten minutes by default.
- A run the API server rejects as invalid returns `422` and leaves the record
  pending, so the event can still be delivered after the template is fixed.
- An administrator debugging a receiver reads the audit record, not the HTTP
  response.
- Run names change format. Anything that parsed the old nanosecond suffix must
  read the `sproozi.com/webhook-event` annotation instead.

## Revisit when

- Senders need to learn delivery status without retrying.
- Forged-request volume makes pre-verification API reads a cost worth caching.

## Links

The implementation below was never released; the links show the code it
replaced. The reworked code is described in
[event webhooks](../explanations/future-architecture/event-webhooks.md).


- [internal/webhook/handler.go](https://github.com/andrewmccall/sproozi/blob/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/internal/webhook/handler.go), [internal/webhook/replay.go](https://github.com/andrewmccall/sproozi/blob/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/internal/webhook/replay.go),
  [internal/webhook/agentrun.go](https://github.com/andrewmccall/sproozi/blob/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/internal/webhook/agentrun.go)
- [Webhook contract](https://github.com/andrewmccall/sproozi/blob/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/docs/reference/webhook-contract.md)
- [ADR 0017](0017-runtime-admission-and-replay-retention.md),
  [ADR 0022](0022-webhook-hmac-secrets-in-own-namespace.md)
