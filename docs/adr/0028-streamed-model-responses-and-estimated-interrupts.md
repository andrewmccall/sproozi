# Streamed model responses, estimated interrupts and bounded reservations

Status: accepted
Date: 2026-10-20

## Problem and constraints

The model gateway buffers each provider response, up to 1 MiB, and parses its
usage before the harness receives any byte
([ADR 0002](0002-native-cli-harnesses-and-model-protocols.md)). Each call holds
the run's only model slot ([ADR 0015](0015-default-clients-and-native-tool-schemas.md)).
Apart from an OpenAI call with an output bound, each call reserves all of the
run's remaining units, and every call under a cost cap reserves all remaining
cost. When the gateway can't read usage, it charges the whole reservation.

That design has three costs that users hit in ordinary use:

- **Interrupts spend the budget.** Pressing Esc in Claude Code or Codex
  cancels the request, the gateway can't read usage, and the run is charged
  everything it had left. The owner called this "a poor UX surprise for the
  user on a common action".
- **Long turns fail.** The harness receives nothing until the call completes,
  so a turn longer than the client's stream-idle timeout (300 s for Codex) is
  retried. A turn longer than the gateway's request deadline (4m50s) is cut and
  charged in full.
- **Long responses fail.** A response over 1 MiB of SSE fails with a 502 and is
  charged in full.

Buffering never limited what the provider bills. The provider bills the work
whether or not the gateway withholds the output, and budgets are documented as
not being hard spending ceilings. Buffering protects two things: output reaches
the harness only after usage was settled from the provider's own report, and a
workload can't read the full output and then cancel to avoid the charge.

What the gateway can count while a response streams:

- **Anthropic Messages.** `message_start` reports exact input, cache-read and
  cache-write usage within about a second. Output usage arrives in
  `message_delta`, usually only near the end.
- **OpenAI Responses**, with an API key or ChatGPT sign-in. Usage arrives only in
  `response.completed`, `response.incomplete` or `response.failed`.
- **Both providers.** Visible output deltas can be counted as bytes. Hidden
  reasoning and summarised thinking can't be counted from the stream at all.

No provider offers a usable way to fetch usage for a cancelled call afterwards.

## Decision

**Stream responses.** The gateway passes provider bytes to the harness as they
arrive. An incremental parser tees the stream and keeps only the current event,
up to a bound large enough for `response.completed`, which carries the whole
output. The 1 MiB response limit is removed. The 1 MiB request limit stays.

**Settle from provider usage when the call completes.** A call that ends with
the protocol's terminal event and its usage (`message_stop` after
`message_delta`, `response.completed` or `response.incomplete`) settles from
that usage, as before.

**Settle an estimate when the call is interrupted.** An interrupted call is one
of these:

- the harness cancels or disconnects;
- the call reaches its deadline;
- the gateway stops and drains its calls;
- the connection fails, or the wait for response headers times out, after the
  whole request was written to the provider;
- the stream ends with an error event and no usage.

The charge is the estimate E, capped at the reservation:

- **Input.** On Anthropic, use the exact usage from `message_start` once it has
  arrived. Otherwise use the request body's bytes ÷ 4, priced at the normal
  input rate.
- **Output.** Charge `min(output bound, max(visible output bytes,
  ceil(elapsed × R)))`, counting each visible output byte as one token. Once
  Anthropic's `message_delta` has reported the exact output, charge that
  instead. Elapsed time is measured from when the request was fully written
  to the provider, because the provider works from then; only if the
  transport doesn't report the write is it measured from the response
  headers. R is a ceiling on output tokens per second, 250 by default.

A response that doesn't stream is relayed as it is read. If its whole JSON
body has arrived when the harness cancels, the call settles from that body's
usage (`usageSource` `provider`, `endReason` `client_cancelled`). Dropping the
connection between the headers and the end of the body is therefore not free:
the call settles from the usage if the body is whole, and is estimated
otherwise.

**Charge an interrupt before the headers once the provider has the request.**
A call that doesn't stream receives its response headers only when the model
has finished, so the provider may have done all of the work before any header
arrives. The reservation is released only when the request was never fully
written: a DNS, connect or TLS failure, or a cancel before the write completed.
Once the request was written, a cancel, deadline, header-wait timeout or
failure before the headers is estimated as above, with no visible output and
elapsed time from the write. Its `endReason` is `client_cancelled`, `deadline`,
`gateway_restart` or `upstream_error`; a header-wait timeout is
`upstream_error`. A header-wait timeout or failure still gets a 502 with
`upstream request failed` and is audited as a failure. A gateway stop instead
always gets a 503 with `the model gateway is shutting down; retry the call`
(code `gateway_stopping` in the OpenAI body; the Anthropic body has no code),
whether the call was waiting for its budget, its
request wasn't fully written (nothing is charged in either case) or it was
waiting for its headers (estimated as above); all three are audited with
`endReason` `gateway_restart`. A harness that leaves is
not a failure and gets no reply. It is audited as allowed with `endReason`
`client_cancelled` whether it left while waiting for its budget (the gateway
answers 408 `model request cancelled while waiting for its budget`), before
the write completed (the reservation is released) or after it (the estimate is
charged).

The time floor means cancelling while the model reasons, where its output is
hidden, doesn't avoid paying for that reasoning. Both the input divisor and R
are estimates. They can under-charge relative to the provider's bill, for
example when hidden reasoning runs faster than R, or when the input tokenises
at under 4 bytes a token. They can also over-charge.

**Charge in full only when nothing can be estimated, or at a budget cut.**
Nothing can be estimated when an interrupted request references input the
gateway can't size, or when one event exceeds the parser's 8 MiB bound. The
references are `previous_response_id`, `conversation`, a `prompt` object,
`file_id`, remote documents or images in message content, an `item_reference`
input item, any input item that carries only `id`, `type` and `status`, and a
reasoning item without `encrypted_content`, or with an empty one, which
replays stored reasoning by id. A reasoning item with a non-empty
`encrypted_content` carries its content, so it stays sizable. Then the whole reservation is charged, as before. Missing or invalid
usage on a completed call is estimated rather than charged in full. So is a
response whose content type doesn't match the request. A provider error
response, such as a 400 or 429, releases the reservation. OpenAI's is relayed
with its status, headers and body, up to 1 MiB (a longer body becomes a 502
`failed to read upstream response`); Anthropic's keeps its status but its
headers and body are replaced with an `api_error` `provider rejected request`.

**Refuse detached work.** The gateway refuses, before reserving or forwarding,
a request with `background` other than omitted, `null` or `false`, or `n` or
`best_of` other than omitted, `null` or a JSON number equal to 1, such as `1`,
`1.0` or `1e0`; a quoted `"1"`, 0 and 128 are refused. The reply is a 400 with type
`invalid_request_error`, code `unsupported_request` and a message that names
the field and the fix: `background responses are not supported by the model
gateway; background must be omitted or the boolean false`, or `n must be 1 (or
omitted): the model gateway bounds one output per request; send one request
per output` (the same for `best_of`). A background response returns at once
without usage while the provider runs the whole job, and `n` or `best_of`
multiplies the output under one output bound. Either way the provider does
work the gateway can't see in the response or bound by the reservation. Other
refusals before inference keep their own codes: a 400 `invalid_json` for a
body that Go's `encoding/json` can't decode, such as one nesting deeper than
10,000 levels, or one that isn't a JSON object, `number_too_large` for a
number too large to read, `ambiguous_json` for duplicate keys, compared
without case in the request, its messages and their content blocks,
`trailing_data` for data after the body's one JSON value, or `invalid_field`
for a `stream` that isn't a boolean, and
a 403 `pricing_unavailable` for missing administrator pricing or
`unsupported_operation` for any other unsupported operation or option.

**Bound reservations.** A request reserves a bounded amount when it is
self-contained and carries an output bound:

- **Units:** request bytes plus the output bound. A token is at least one byte,
  so request bytes bound input usage.
- **Cost:** request bytes at the highest input or cache-write rate, plus the
  output bound at the output rate.

Anthropic requests always carry `max_tokens`, so self-contained Anthropic
requests are now bounded too. A request without an output bound still reserves
all remaining units and cost. That includes Codex and every ChatGPT-mode
request. So does a request that uses a provider-run tool, such as web search
(including chat completions' `web_search_options`), file search, code
execution, image generation or a remote MCP server, because the provider bills
that work separately.

**Run bounded calls in parallel.** Calls with bounded reservations from one run
proceed in parallel while their reservations fit. A call whose reservation
doesn't fit waits for an earlier call to settle. It is refused only when
nothing else is pending and the run's budget can't cover it. A call that
reserves all remaining capacity still runs alone: it waits for calls in flight
to settle, and calls after it wait for it. A bounded call that can't fit while
nothing else is pending reserves what remains and runs alone, like an unbounded
call; it is refused only when nothing remains. This replaces ADR 0015's strict
per-account serialization for bounded calls. Accounts remain independent.

**Cut an unbounded stream at its reservation.** An unbounded call reserves
everything remaining, so the gateway watches the observed usage of each
unbounded streaming call whose input it can size. Observed usage
is the known input, or request bytes ÷ 4, plus the visible output bytes ÷ 4.
This differs from the interrupt estimate, which counts each visible byte as one
token: bytes ÷ 4 approximates real usage, so the cut doesn't end English output
at a quarter of its reservation, while the estimate leans towards
over-charging. If observed usage reaches the reservation, the gateway sends the
protocol's error event, which says the budget is exhausted, cancels the
upstream call and charges the reservation. Elapsed time doesn't count towards
the cut. Counting it would end
a long Codex or ChatGPT turn after a few minutes and charge it in full,
whatever it really used. The time floor prices an interrupt; it doesn't end a
call. Bounded calls are never cut. Their reservation already covers the
request, and the provider enforces their output bound itself. An unbounded call
whose input the gateway can't size is never cut either: the request bytes
don't bound its input, so observed usage would mean nothing. It settles from
provider usage, or is charged its whole reservation if interrupted.

**Settle provider-reported failures from their usage.** A `response.failed`
event that carries usage settles from it. Without usage, it is estimated.

**Treat usage above the reservation as exceeded.** When final usage exceeds
the call's own reservation, the output has already reached the harness. The
audit record marks the call `exceeded`, and it no longer replaces the output
with a 502. Usage within the reservation is charged as reported and is never
`exceeded`. Usage beyond it is charged `min(used, max(limit − settled −
otherReserved, reserved))`, where `limit` is the run's budget (at home, the
execution's allowance), `settled` is usage already settled and
`otherReserved` is what other pending calls still hold. Cost is capped the same
way, and `limit` is the limit in force when the call reserved. Settled usage
therefore never passes the limit, and calls still in flight keep their
reservations. A policy change closes the run's connections, so a call in
flight is cancelled and charged an estimate capped at its reservation. Only a
call that completes within the roughly one-second revocation window settles
against the old limit, so only it can charge above a lowered limit.

**Give model calls their own deadline.** A model stream may outlive its
connection's 5-minute lifetime, up to a maximum call duration:

- 20 minutes by default;
- set by `SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` for the shared gateway and by
  `gateway.maxModelCallSeconds` for the home gateway;
- optionally lowered per run by `AgentPolicy.spec.maxModelCallSeconds`.

The model handler asks the connection layer to extend its request's deadline
(`proxytransport.ExtendModelCall`). The connection stays open until that call
ends. Revocation checks continue throughout. At the connection's request
deadline the gateway still stops keep-alive and refuses new requests on that
connection. At the call's deadline the stream ends without an error event, and
the call is charged as interrupted. The provider client has no whole-call
timeout. The shared gateway waits up to 4m50s for response headers, and the
home gateway waits 45 seconds. Both end a stream that sends nothing for 5
minutes. Editing `maxModelCallSeconds` is a policy spec change, so it closes
the run's open connections like any other spec edit.

**Record how each call was charged.** Audit records gain `usageSource`
(`provider`, `estimated`, `reservation`, `recovered` or `unsettled`) and `endReason` (`completed`,
`client_cancelled`, `deadline`, `upstream_error`, `budget_cut`, `parse_limit`,
`exceeded` or `gateway_restart`). The gateway logs `Charged estimated model usage` with
`runID`, `runUID`, `endReason`, `units`, `costMicros`, `inputSource`,
`visibleBytes` and `elapsed`, so the estimate's accuracy can be measured
later. A full charge logs `Charged model reservation in full` with `runID`,
`runUID`, `endReason`, `detail` (why), `units` and `costMicros`, taken from
the call's own reservation so parallel settlements can't distort them. When
the ledger refuses a settlement, release or full charge, the gateway logs no
charge and the audit record has `tokensUsed` 0: `usageSource` `recovered`,
with `Model reservation was already recovered`, when the call's lease had
already been settled at its recovery charge, and otherwise `unsettled`, with
`Failed to settle model reservation` and `runID`, `runUID`, `endReason` and
`error`. Settling one reservation twice fails with `budget: reservation
already settled`.

**Drain model calls when the gateway stops.** On SIGTERM the shared gateway
stops accepting connections and cancels every model call in flight with
`endReason` `gateway_restart`. A call whose request reached the provider settles
through the estimate path, as an interrupt; a call still waiting for its budget,
or whose request wasn't fully written, releases its reservation. Each that
hasn't received its response headers gets a 503 (OpenAI code
`gateway_stopping`). The gateway waits up to 30 seconds for them to settle, then
exits; its Pod sets `terminationGracePeriodSeconds: 45` to leave room for that
wait. A call that arrives during shutdown gets the same 503 with `the model
gateway is shutting down; retry the call` (OpenAI code `gateway_stopping`).
The home daemon drains the same way for up to 10 seconds when it closes.

**Recover abandoned reservations.** A gateway killed without a drain, such as by
SIGKILL or running out of memory, can't settle its calls. The ledger records
every reservation as a lease with an expiry and a recovery charge, and
settling the call removes it. A model call's lease has:

- **Expiry.** The call's model call deadline plus 5 minutes, after any
  settlement the call could still make.
- **Charge.** The request bytes ÷ 4 for input, plus the whole model call
  deadline at R for output, capped at the output bound, priced as an
  estimate. A request whose input can't be sized is charged its whole
  reservation. The ledger caps the charge at the reservation.

Every other reservation, for a GitHub, package, Kubernetes, egress or MCP
request, gets a lease too. It expires at the request's deadline plus 5
minutes and charges the whole reservation, as its gateway does when it can't
settle the request. So a pending reservation without a lease can only come
from a gateway from before this decision.

The next Reserve or Seal on the account settles every expired lease; Seal does
so before it checks for unbounded pending usage, so a seal after the expiry
succeeds. So that an idle account doesn't keep an abandoned lease, the shared
gateway also sweeps every budget ConfigMap (label
`app.kubernetes.io/component=budget-ledger`, which needs `list` in its budget
Role), in pages of 500, at start and every minute, and the home daemon sweeps
its ledger files when it opens them and every minute. A failed sweep logs
`Failed to recover expired budget reservations: <error>`. A ledger that can't
be decoded is skipped with `Skipped corrupt budget ledger` (`configMap`,
`error`), and the sweep carries on and reports that error. A recovery, settle
or sweep that finds the account deleted stops without recreating it, so a
deleted account stays deleted. A Reserve or Seal saves the recovery in
an update of its own before the update that reserves or seals, so it persists even when
that reservation or seal then fails. Each settlement logs, only once that
save has committed, `Settled abandoned model reservation` for a model call's
lease, or `Settled abandoned budget reservation` for any other, with
`account`, `endReason` `gateway_restart`, `usageSource` `estimated`, `units`
and `costMicros`. If the original gateway later tries to settle a recovered
reservation, the ledger refuses it with `budget: reservation expired and was
settled at its recovery charge`, and that gateway logs `Model reservation was
already recovered` and audits the call as `usageSource` `recovered`. Until a
lease expires its reservation still counts as pending, so while an unbounded
call's lease holds everything that remained, later calls on the account,
unbounded ones included, get 429, and its seal fails with `pending usage has
no finite bound`, so the session can't stop or move until the lease expires.
An account holds at most 64 leases, so at most 64 of its calls can be pending
at once; a further call gets 429 with type `rate_limit_error` and `too many
model calls are pending on this budget; retry when one finishes` (OpenAI code
`too_many_pending_calls`; the Anthropic body has no code) until one settles.

Settlements of one account on the shared gateway write one ConfigMap and can
conflict. A conflicting write retries with jittered exponential backoff until
its 30-second settlement deadline, not a fixed number of attempts, so a drain
normally settles every call; one still failing at the deadline fails with
`budget: ledger update conflicted until its deadline: …` and is left to
recovery.
This narrows ADR 0015's rule that a
restart can't release an unknown reservation: such a reservation is still
never refunded, but it is no longer held forever.

## Alternatives

- **Keep buffering.** It keeps settled-before-delivered output, but it keeps
  all three user-facing failures, and it doesn't limit provider spend.
- **On interrupt, charge input plus the output bound.** It never under-charges.
  But an Esc in Claude Code would cost its 32k–64k output bound, and Codex
  sends no bound, so the Esc problem would remain for Codex.
- **On interrupt, charge only the visible output.** It is the cheapest for
  users, but a workload could cancel during hidden reasoning and avoid paying
  for it.
- **Fetch usage after the call.** OpenAI's stored responses need `store:true`,
  which ChatGPT mode forbids. Anthropic's usage API is aggregated, delayed and
  needs an administrator key.
- **Let the provider finish after the harness disconnects**, to read its real
  usage. It raises real spend and holds the run's capacity.
- **Release the reservation for any interrupt before the response headers.**
  It is simple, but a call that doesn't stream gets its headers only when the
  model finishes, so a cancel just before then would be free although the
  provider bills the whole response.
- **Accept background responses and `n` or `best_of` above 1, and charge the
  whole reservation.** The provider's work can outlast or exceed any
  reservation the gateway could hold, so refusing them is the only bound.
- **Refund or keep unknown reservations after a crash.** Refunding them lets
  a crash or kill erase work the provider billed. Keeping them, as ADR 0015
  did, locks an unbounded account until an administrator repairs its record.
- **Charge an abandoned reservation in full.** It never under-charges, but an
  unbounded call's reservation is the whole remaining budget, so one gateway
  crash would exhaust every run that had a call in flight.
- **Estimate OpenAI input as the full request bytes.** It never under-charges,
  but it over-charges about four times and ignores cache discounts. The owner
  chose bytes ÷ 4.

## Tradeoffs and consequences

- Output reaches the harness before its usage is settled. A call whose final
  usage exceeds its reservation can deliver output beyond what was reserved.
  Budgets were already not hard spending ceilings, and this makes that more
  visible.
- Interrupted calls are charged an estimate. The estimate can differ from the
  provider's bill in either direction, and its bounds are R and the input
  divisor. Pressing Esc is free only while the call waits for its budget or
  before the request was fully written to the provider; otherwise even a
  cancel before the response headers is charged.
- A gateway restart charges calls in flight an estimate, and a crash leaves
  their reservations held until their leases expire, up to the call deadline
  plus 5 minutes. An unbounded account can't make calls until then.
- Rolling back is a risk: older versions of the shared gateway, the profile
  service and the home daemon refuse to read a budget record that holds
  leases, and every pending reservation a current binary makes, model call or
  not, holds one. A drained stop normally settles
  every lease; a crash, a drain that runs out of time or a failed settle
  leaves them behind, and the sweep settles those after their expiry. So
  before rolling back, wait the model call deadline plus 5 minutes and one
  sweep with a current binary running.
- The first upgrade from a gateway that doesn't drain behaves as that gateway
  did: its calls in flight are cut off, and their reservations have no lease.
  A legacy pass charges the whole reservation of each such reservation, as
  ADR 0015 did, so the account isn't blocked for good. The upgraded shared
  gateway runs it before serving and, if it fails, logs `Failed to recover
  legacy budget reservations; retrying: <error>`, serves anyway and retries
  with backoff from 1 second up to 1 minute until it succeeds once; the home
  daemon runs it with each sweep from when it opens its ledger until one
  succeeds. Each charge logs `Settled legacy budget reservation`. The pass is
  safe while gateways serve because every reservation a current gateway makes
  has a lease, so a pending reservation without one can only come from a
  gateway from before this decision.
- Clients can't use background responses or ask for several outputs in one
  request through the gateway; they send one request per output instead.
- Bounded reservations hold more capacity than most calls use, because request
  bytes overstate input tokens. That limits how many bounded calls run at once,
  but never how much a run can spend.
- A model call can hold gateway memory and a reservation for up to its maximum
  duration.
- ADR 0002's buffering, withheld output and response bound, and its "disconnected
  provider response consumes the reservation" cost, are replaced. ADR 0009's
  rule of charging the bound after provider uncertainty now applies only when
  no estimate is possible, or at a budget cut. ADR 0015's per-account serialization now applies
  only to unbounded calls.

## Revisit when

- Telemetry from `Charged estimated model usage`, compared with provider bills,
  shows the estimate is consistently far off. Then tune R, the input divisor,
  or both, or charge differently.
- A provider offers usage for cancelled calls, or reports usage incrementally.
- Accounts reach the 64-lease limit in normal use, or crashes leave unbounded
  accounts locked for too long.
- A workload is found exploiting the estimate, for example by repeatedly
  cancelling just before completion.
- Per-run model call limits need more than a policy field, for example a limit
  set per run or per template.

## Links

- Implementation: `internal/endpoints/model/`, `internal/budget/`,
  `internal/proxytransport/modelcall.go`, `cmd/gateway/main.go`,
  `config/manager/gateway.yaml`, `config/rbac/shared_gateway_budget_role.yaml`,
  `api/v1alpha1/agentpolicy_types.go`,
  `internal/session/home/compose.go` and `internal/session/home/service.go`.
- References: [Model provider authentication](../guides/model-provider-auth.md),
  [Security hardening](../reference/security-hardening.md),
  [AgentPolicy](../reference/agentpolicy.md) and
  [Harnesses](../reference/harnesses.md).
- Earlier records:
  [ADR 0002](0002-native-cli-harnesses-and-model-protocols.md),
  [ADR 0009](0009-runtime-backends-and-sealed-accounting.md) and
  [ADR 0015](0015-default-clients-and-native-tool-schemas.md).
