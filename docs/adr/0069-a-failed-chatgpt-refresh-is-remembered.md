# 0069: A failed ChatGPT refresh is remembered

Status: accepted
Date: 2026-11-17

## Problem and constraints

In `chatgpt` mode the gateway holds an administrator's ChatGPT sign-in and
refreshes it itself ([ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md),
[ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md)). The
provider rotates the refresh token on every refresh. A refresh is due when the
access token is within a minute of expiry; it runs under the gateway's lock,
always from the stored session, and the result is saved before it is used.

A review found that a failed refresh left no state behind:

- **An outage.** Every model request that found the token due took the lock
  and sent its own refresh. Ten requests arriving together sent ten refreshes,
  one after another, each waiting for those before it, up to 30 seconds each
  when the endpoint did not answer.
- **A refused sign-in.** After the provider refused the refresh token
  (`invalid_grant`, a 400 or 401), every model request sent the same spent
  token again, for as long as the gateway ran. Nothing ended that except an
  administrator's new sign-in, and nothing told anyone one was needed: the
  caller got `provider authentication is unavailable`, and the audit record
  said no more.
- **A request that left.** The refresh ran under the model request's
  context. When the harness left, or the model call deadline passed, while
  the provider was answering, the provider had already spent the old refresh
  token, but the gateway dropped the new one. The next request sent the spent
  token and was refused, so the sign-in was lost.

The fix must not leak token material to callers or logs, must keep one refresh
at a time per credential in a process, and must not leave any state waiting on
time with no other exit ([ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md)).

## Decision

**The gateway remembers the outcome of the last refresh** in a refresh record
beside the session it describes, in `internal/modelauth/refresh.go`. It follows
[ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md)'s pattern
as far as a small record needs it: a typed state, a check of the record's
structure on every move, a `(from, event) → to` table and one `apply` function.
There is no deadline column: no execution deadline applies.

| State | Meaning | Refresh sent |
| --- | --- | --- |
| `ready` | No failure remembered | When due |
| `held` | The last refresh failed transiently | When due and the hold has ended |
| `refused` | The provider refused the refresh token | Never for these credentials |

| Event | From → to |
| --- | --- |
| `refreshed` | `ready`, `held` → `ready` |
| `failed` | `ready`, `held` → `held` |
| `refused` | `ready`, `held` → `refused` |
| `signed-in` | `held`, `refused` → `ready` |

- **Which failures refuse.** A token endpoint reply of 4xx other than 408 and
  429 is a refusal: OAuth 2.0 returns `invalid_grant` and the other grant and
  client errors as 400 or 401, and sending the same token again cannot change
  the answer. No reply, a timeout, 408, 429, 5xx, a 200 that cannot be read
  and a 200 whose credential is incomplete (for example, with no new refresh
  token) are transient.
- **The hold is 10 seconds** (`RefreshHold`), from the failure. It equals
  `SessionReloadInterval`, the documented bound on how old the gateway's view
  of the stored credential may be, so a remembered failure is never older than
  a remembered session. It is not a wait: the first model request after it
  sends the refresh again. With the refresh client's 30-second timeout, a
  provider that does not answer gets at most one refresh every 40 seconds from
  the gateway, instead of one per model request.
- **A new sign-in ends either state at once.** Every request in a hold or
  after a refusal still reads the store, as before. When the stored
  credentials differ from those the failure was for, the record moves to
  `ready` before anything else, so an administrator's new sign-in reaches the
  next model request. `refused` has no exit by time: its only exit is the
  event that fixes it.
- **Requests that waited for the lock** see the failure the refresh before
  them recorded, so a refresh failure costs one provider request however many
  model requests were waiting. As before, no request uses the still-valid
  access token during the refresh window once its refresh failed.
- **What a caller sees.** The gateway's existing 503 with type `server_error`
  and code `provider_auth_unavailable`. Its message, and the audit record's
  `denyReason`, add the cause, built from fixed phrases and an HTTP status
  only: `provider authentication is unavailable: the ChatGPT sign-in could not
  be renewed (token endpoint HTTP 503); the gateway tries again 10s after the
  failure`, or `... the provider refused to renew the ChatGPT sign-in (token
  endpoint HTTP 400); an administrator must sign in again`. Other provider
  authentication failures keep the bare message.
- **A refresh finishes once it has started.** The refresh, and each save of
  its result, run under a context detached from the model request, bounded at
  30 seconds, the timeout of the client both the cluster and the local gateway
  refresh with. A harness that leaves no longer drops a rotated token.

The record is held in memory by the one gateway process that owns the
refresh. A gateway restart forgets it, and the first due request after the
restart sends one refresh.

## Alternatives

- **Keep the record in the Secret or the private file.** It would survive a
  restart and reach any other reader, but it would add a field the login
  helper and `kubectl` users must preserve, and a store write on every
  failure. With one refresh owner (ADR 0026), the restart costs one refresh.
- **Back off exponentially, or honour `Retry-After`.** Fewer provider requests
  during a long outage, but the gateway would serve errors longer after the
  provider recovers, and the repository records nothing about the provider's
  rate limits to size it from. A fixed hold tied to an existing interval is
  enough to stop the per-request storm.
- **Serve the still-valid access token while a refresh is held.** During the
  last minute before expiry the token still works. It would hide short
  outages, but it changes what a request sees during a refresh, which this
  change keeps.
- **Let one request refresh in the background while others return at once.**
  Waiters would not queue behind a slow refresh, but it needs a goroutine and
  a result channel per refresh. With the hold, the queue is one refresh long.

## Tradeoffs and consequences

- A refresh failure in the last minute before expiry makes model requests fail
  for up to 10 seconds even if the provider recovers sooner.
- A provider that wrongly answers 4xx to a good refresh token leaves the
  sign-in `refused` until an administrator signs in again or the gateway
  restarts.
- A 200 without a new refresh token is still treated as a failed refresh, and
  the new access token is dropped. If the provider had spent the old token,
  the next refresh is refused.
- A request whose harness left can still hold its handler for up to 30
  seconds while its refresh finishes.
- A timeout while the provider is answering still loses the rotated token, as
  does a crash before the save (ADR 0026).

## Revisit conditions

Revisit if the gateway runs more than one replica that refreshes (then the
record must be shared), if the provider documents a retry policy or a
non-rotating refresh, if operators need an alert on a refused sign-in, or if
refused sign-ins turn out to come from provider errors rather than revocation.

## Links

- [Model provider authentication](../guides/model-provider-auth.md#chatgpt-sign-in-failures)
- [Required Secrets and configuration](../reference/required-secrets-and-config.md)
- `internal/modelauth/refresh.go` (`refreshTransitions`, `RefreshHold`, `RefreshError`)
- `internal/modelauth/auth.go` (`reloadOrRefresh`, `detached`)
- `internal/endpoints/model/handler.go` (`forward`)
- `internal/modelauth/refresh_test.go`, `internal/modelauth/refresh_state_test.go`
