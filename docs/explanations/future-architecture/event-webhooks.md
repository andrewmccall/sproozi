# Event webhooks

Status: removed; may return once a supported sender exists.

Sproozi shipped a signed HTTP receiver, `sproozi-webhook`, that turned an
external event into one AgentRun. No demo, guide or verifier sent it an event,
and its only planned sender, the
[home-ops Alertmanager integration](../../guides/home-ops-playbook.md), was
never built. [ADR 0043](../../adr/0043-remove-the-event-webhook.md) removed it.
This note keeps the design and what two review rounds learned about it, so a
future receiver starts from there.

The original code is at commit `01bac1b` ([cmd/webhook](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/cmd/webhook),
[internal/webhook](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/internal/webhook), [config/webhook-secrets](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/config/webhook-secrets), [config/webhook-state-rbac](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/config/webhook-state-rbac)). The
later idempotent-delivery rework described below was never released.

## What it did

- **Endpoint.** `POST /webhook/<namespace>/<receiver>` on port 8083, cluster
  internal. A NetworkPolicy admitted only namespaces labelled
  `sproozi.com/webhook-client: enabled`.
- **Receiver.** Exactly one AgentTemplate in `<namespace>` labelled
  `sproozi.com/webhook-receiver=<receiver>` and annotated
  `sproozi.com/webhook-secret=<secret>`.
- **Authentication.** `X-Sproozi-Signature: sha256=<hex>` over
  `timestamp || rawBody`, with `X-Sproozi-Timestamp` in Unix seconds within a
  five-minute window. The key was the `hmac-key` entry of a Secret of at least
  32 bytes, read only from `sproozi-webhooks`
  ([ADR 0022](../../adr/0022-webhook-hmac-secrets-in-own-namespace.md)).
- **Body.** `{"id": "...", "eventContext": {"k": "v"}}`, at most 512 KiB.
- **Result.** An AgentRun with the fixed task `Process incoming webhook event`,
  the template's runtime and policy, every capability the policy allowed, and
  the event context as untrusted input.
- **Replay protection.** One ConfigMap per accepted event in
  `sproozi-webhook-state`, kept for two replay windows after the event's
  timestamp and swept once per window
  ([ADR 0017](../../adr/0017-runtime-admission-and-replay-retention.md)).
- **Limits.** Headers 64 KiB read within 10 s, whole request 45 s, response
  45 s, idle keep-alive 60 s.

## Gotchas a new receiver must handle

These are the defects reviews found, in the order they bite.

1. **The key belongs to the receiver, so lookups happen before the MAC is
   checked.** A forged request with well-formed headers costs a body read, a
   template list and policy and Secret reads. Answer every failure before the
   MAC is confirmed with the same `401` (missing or ambiguous receiver, missing
   or short key, and a `Forbidden` Secret read), or an unauthenticated caller
   can map receivers. Return `503` only for transient API errors. Rate-limit at
   ingress, or cache keys with a short lifetime.
2. **Recording the event before creating the run loses events.** The first
   version wrote the replay record, then created the run; a failed create made
   every retry a "replay". The rework recorded a `pending` claim, created the
   run, then marked the record `delivered` with the run's name. A pending claim
   let a retry finish the delivery; only a delivered record refused it. Records
   from before the state existed counted as delivered.
3. **Retries must name the same run.** Name the run from the event, not the
   clock, and put the event key in an annotation, so a retry after a lost
   response finds the run (`AlreadyExists`, annotation matches, answer `200`)
   instead of creating a second one. Refuse a same-named run whose annotation
   belongs to another event (`409`).
4. **Do not put mutable routing in the run name.** The rework named runs
   `<template>-<16 hex of the event key>`. Reassigning a receiver to another
   template between two deliveries of one event produced two names and two
   runs. Derive the name from the event key alone, or record the intended run
   name in the claim and make every later delivery use it.
5. **A replay record outlives its run.** If marking `delivered` fails and the
   run is deleted within the replay window, a retry creates the run again.
   Either accept that, or make `MarkDelivered` retry on conflict.
6. **Separate store failures from replays.** A replay-store API error must be
   `503`, not `409` with the internal error in the body. Keep error bodies
   fixed and send causes to the audit log.
7. **Cleanup of expired records must not recurse.** The first version ignored
   a delete error on an expired record and called itself again. Bound the claim
   to a few attempts and return the delete error.
8. **Bound event context at the door.** The agent contract allows 16 KiB of
   event-context JSON; the CRD allows 64 keys. Check both before claiming the
   event, so an event that can never run does not consume a record or a run.
9. **Distinguish invalid runs from outages.** An `Invalid` or `BadRequest`
   create cannot succeed on retry until an administrator fixes the template or
   policy. Answer `422` and leave the claim pending, so the event can still be
   delivered after the fix. A `5xx` tells the sender to retry for nothing.
10. **Long template names.** The run name is copied into label values, which
    are limited to 63 characters. Cut the template part and trim trailing `-`
    and `.`.
11. **Event runs are maximally privileged by construction.** Copying every
    allowed capability means an event run can do whatever its policy allows.
    Let the template name the subset an event run needs.
12. **RBAC follows the protocol.** The rework needed `get` on AgentRuns (to
    find a run by name) and `update` on replay ConfigMaps (to mark delivery),
    in addition to `create`, `get`, `list` and `delete` on records and Secret
    `get` in the key namespace only.
13. **Clock skew between replicas.** A replica accepts an event within one
    window of its own clock. Keep records for two windows, judged by whichever
    replica reads or sweeps them, so replicas whose clocks differ by less than
    one window cannot accept an event again.
14. **Test it end to end.** Every webhook test used a fake Kubernetes client or
    an in-memory replay store. The Kind e2e only checked that the Deployment
    started. A returning receiver needs a Kind case that signs an event, kills
    the create once, retries, and checks exactly one run, plus a two-replica
    race against the Kubernetes store.

## Before bringing it back

- A supported sender, with an adapter Sproozi ships or documents end to end,
  for example Alertmanager through a signing relay.
- A decision on whether event runs get every allowed capability or a declared
  subset.
- An ADR that replaces ADR 0043, and the Kind case in gotcha 14.
