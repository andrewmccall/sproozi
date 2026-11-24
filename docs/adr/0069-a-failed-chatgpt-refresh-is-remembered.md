# 0069: A failed ChatGPT refresh is remembered

Status: accepted
Date: 2026-11-17

## Problem and constraints

In `chatgpt` mode the gateway holds an administrator's ChatGPT sign-in and
refreshes it itself ([ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md),
[ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md)). The
provider rotates the refresh token on every refresh. A refresh is due when the
access token is within a minute of expiry. It always starts from the stored
session, and its result is saved before it is used.

A review found that a failed refresh left no state behind:

- **An outage.** Every model request that found the token due took the
  authenticator's lock and sent its own refresh. Ten requests arriving
  together sent ten refreshes, one after another, each waiting for those
  before it, up to 30 seconds each when the endpoint did not answer.
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
at a time per credential in a process, must not hold any caller past its own
deadline, and must not leave any state waiting on time with no other exit
([ADR 0060](0060-no-state-waits-on-a-deadline-to-end.md)).

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
| `held` | The last refresh failed, but not by a refusal | When due and the hold has ended |
| `refused` | The provider refused the refresh token | Never for these credentials |

| Event | From → to |
| --- | --- |
| `refreshed` | `ready`, `held` → `ready` |
| `failed` | `ready`, `held` → `held` |
| `refused` | `ready`, `held` → `refused` |
| `signed-in` | `held`, `refused` → `ready` |

- **Only `invalid_grant` refuses.** A refusal is a 400 or 401 whose JSON body
  has `error` equal to an entry of `refusedGrantErrors`. The list holds
  `invalid_grant`, the OAuth 2.0 code for a revoked, expired or spent grant
  and the only one the repository records for this provider. The gateway
  reads at most 4 KiB of an error reply, decodes only the `error` field, and
  carries only the list's own string onward. Everything else is held: no
  reply, a timeout, a redirect, every other status (a CDN's 403, a proxy's
  407, a moved endpoint's 404, a 400 with another error, an empty or oversized
  body), a 200 that cannot be read, and a 200 whose credential is incomplete,
  for example with no new refresh token. Sending the request again may then
  succeed, and none of these is the provider revoking the sign-in.
- **The hold is 10 seconds** (`RefreshHold`), from the failure. It equals
  `SessionReloadInterval`, the documented bound on how old the gateway's view
  of the stored credential may be, so a remembered failure is never older than
  a remembered session. It is not a wait: the first model request after it
  sends the refresh again.
- **A still-valid access token is used during a hold.** While a transient
  failure holds the refresh back, requests are served with the stored access
  token until 30 seconds (`expiryMargin`) before its computed expiry. The
  expiry is counted from when the refresh reply arrived, which the refresh
  client's 30-second timeout puts at most 30 seconds after the provider issued
  the token, so the margin keeps the gateway from sending a token the provider
  may already treat as expired. A refusal is never served, and from the margin
  on, requests fail until the next refresh.
- **A failure is remembered as the session is.** Requests answer from the
  in-memory copy and its refresh record for up to `SessionReloadInterval`
  after the store was last read, then read it again. A hold or a refusal costs
  one store read per 10 seconds, as a healthy session does. A failed store
  read is not remembered: while the store itself fails (an unreadable Secret
  or file), every model request reads it again, as before this change. A
  review measured 1,000 reads for 1,000 sequential requests. Bounding that is
  an open follow-up.
- **A new sign-in ends either state at that read.** When the stored
  credentials differ from those the failure was for, the record moves to
  `ready` before anything else. An administrator's new sign-in reaches model
  requests within `SessionReloadInterval`. `refused` has no exit by time: its
  exits are a new sign-in and a gateway restart.
- **The refresh runs in a goroutine the authenticator owns.** Each reload,
  and refresh when due, is one attempt. At most one runs per credential; it is
  started under the lock, which guards only which attempt is in flight and is
  never held across a store call or the refresh. Every caller, including the
  one that started it, waits for its result or for its own context, whichever
  comes first, and a caller that leaves gets its context's error at once. A
  caller that arrives while one is in flight joins it. Each step (the store
  read, the save of an unsaved refresh, the refresh, and its save) runs under
  its own 30-second bound and no caller's context, so the rotated token is
  saved even if every caller has left, and the next request uses it without a
  second refresh. An attempt ends within about 2 minutes in the worst case;
  no model request, and so no budget reservation, waits longer than its own
  deadline.
- **No redirects.** Both gateways refresh with `NewRefreshClient`: no ambient
  proxy, a 30-second timeout and no redirects, which would re-send the
  refresh token to the redirect's target. `NewChatGPT` also refuses redirects
  on any client it is given.
- **What a caller sees.** The gateway's existing 503 with type `server_error`
  and code `provider_auth_unavailable`. Its message, and the audit record's
  `denyReason`, add the cause, built from fixed phrases, an HTTP status and
  the matched list entry only: `provider authentication is unavailable: the
  ChatGPT sign-in could not be renewed (token endpoint HTTP 503); the gateway
  tries again 10s after the failure`, or `... the provider refused to renew
  the ChatGPT sign-in (token endpoint HTTP 400 invalid_grant); an
  administrator must sign in again`. Other provider authentication failures,
  including a caller that left, keep the bare message.

The record is held in memory by the one gateway process that owns the
refresh. A gateway restart forgets it, and the first due request after the
restart sends one refresh. The attempt's goroutine is not tied to the
gateway's drain: when the gateway stops, model calls end as before and the
attempt keeps running on its own bounds until the process exits. If the
process exits between the provider's reply and the save, the rotated token is
lost, as ADR 0026 records for a crash.

## Alternatives

- **Detach the refresh but keep it on the request's goroutine, under the
  lock.** This was the first version of this change. A review measured it: a
  request with a 50 ms deadline whose refresh took 1.5 s was held for the
  1.5 s, and so was an already-cancelled request queued behind it on the lock,
  each keeping its budget reservation, and up to 30 seconds for the refresh
  plus 30 for each save in the worst case. Joining an owned attempt cost one
  goroutine and one channel per attempt; the same test now returns at 51 ms
  and at once.
- **Match the error reply with `json.Unmarshal` into a struct.** It matches
  the `error` key without regard to case and lets a later duplicate key
  replace the first, so `{"ERROR":"invalid_grant"}` refused a sign-in. The
  gateway walks the reply with `encoding/json`'s token decoder instead, takes
  the first key spelled exactly `error`, and requires one object with nothing
  after it.
- **Refuse on any 4xx but 408 and 429.** Also the first version. A proxy's,
  CDN's or moved endpoint's 4xx, or a 400 caused by this gateway, would have
  ended a good sign-in with a false instruction to sign in again, where the
  old code recovered as soon as the endpoint answered.
- **Keep the record in the Secret or the private file.** It would survive a
  restart and reach any other reader, but it would add a field the login
  helper and `kubectl` users must preserve, and a store write on every
  failure. With one refresh owner (ADR 0026), the restart costs one refresh.
- **Back off exponentially, or honour `Retry-After`.** Fewer provider requests
  during a long outage, but the gateway would serve errors longer after the
  provider recovers, and the repository records nothing about the provider's
  rate limits to size it from. A fixed hold tied to an existing interval is
  enough to stop the per-request storm.
- **Read the store on every request while a failure is remembered.** An
  administrator's new sign-in would be used on the next request instead of
  within 10 seconds, at one Secret read per model request during an outage.

## Tradeoffs and consequences

- From 30 seconds before the access token's computed expiry, a transient
  failure fails model requests for up to 10 seconds even if the provider
  recovers sooner.
- The local gateway's refresh no longer uses an ambient `HTTPS_PROXY` or
  `HTTP_PROXY`. The cluster gateway's never did, and the local gateway's model
  calls already ignored them.
- An administrator's new sign-in can take up to 10 seconds to reach model
  requests after a failure, as it does when the sign-in is healthy.
- A provider that wrongly answers `invalid_grant` to a good refresh token
  leaves the sign-in `refused` until an administrator signs in again or the
  gateway restarts.
- A 200 without a new refresh token is still a failed refresh, and the new
  access token is dropped. If the provider had spent the old token, the next
  refresh is refused.
- A timeout while the provider is answering still loses the rotated token, as
  does a crash before the save (ADR 0026).
- An attempt can keep running for up to about 2 minutes after every caller
  has left.

Found and not fixed, each a follow-up:

- On a local installation, `FileSessionStore.Replace` reads, compares and
  renames the credential file under a per-process lock only. A `sproozi init
  --chatgpt` sign-in written while the gateway saves a refresh can be
  overwritten by the refreshed old sign-in. A review measured the
  administrator's sign-in lost in 398 of 400 runs of that collision on this
  change and 382 of 400 before it, a 21 to 27 ms window this change did not
  widen.
- A failed store read is not cached, so a failing store is read once per
  model request.
- The refresh client ignores `providerCAFile`, although the local gateway's
  model client honours it, so a local installation whose provider needs a
  private CA cannot refresh a ChatGPT sign-in through it.
- While a refresh is in flight in the last minute before expiry, model
  requests wait for it, and fail at their own deadline if the provider is
  slow, although the stored access token still works. Serving the previous
  copy while an attempt is in flight would fix it.

## Revisit conditions

Revisit if the gateway runs more than one replica that refreshes (then the
record must be shared), if the provider documents a retry policy, other
refusal codes or a non-rotating refresh, if operators need an alert on a
refused sign-in, or if refused sign-ins turn out to come from provider errors
rather than revocation.

## Links

- [Model provider authentication](../guides/model-provider-auth.md#chatgpt-sign-in-failures)
- [Required Secrets and configuration](../reference/required-secrets-and-config.md)
- `internal/modelauth/refresh.go` (`refreshTransitions`, `RefreshHold`, `RefreshError`, `refusedGrantErrors`, `NewRefreshClient`)
- `internal/modelauth/auth.go` (`reloadOrRefresh`, `run`, `reload`, `served`)
- `internal/endpoints/model/handler.go` (`forward`)
- `internal/modelauth/refresh_test.go`, `refresh_state_test.go`, `refresh_attempt_test.go`, `refresh_classify_test.go`, `refresh_redirect_test.go`, `refresh_valid_test.go`
