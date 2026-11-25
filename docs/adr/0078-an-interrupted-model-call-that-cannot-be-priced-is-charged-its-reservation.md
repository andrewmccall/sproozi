# 0078: An interrupted model call that cannot be priced is charged its reservation

Status: accepted
Date: 2026-11-25

Extends [ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md#decision).

## Problem and constraints

[ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md) charges
a model call that is interrupted an estimate, capped at its reservation. For an
Anthropic call under a cost cap, the estimate uses the exact usage from
`message_start` once it has arrived, and prices each category at the model's
rate in the administrator's pricing file.

A call that completes is settled differently. Settlement prices the provider's
reported usage with `costAnthropic`, which fails when a category has tokens and
the model's price has no rate for it. The call is then charged its whole
reservation, and the audit event records `usageSource` `reservation`.

The two disagreed about a missing rate:

- The estimate priced a missing cache-write rate at the model's input rate,
  and a missing cache-read, input or output rate at 0.
- Settlement charged the whole reservation.

So an interrupted call cost less than the same call completed, and the
cheapest way to use a model with an incomplete price was to interrupt the call.
Anthropic admission checks only that the model has a price row, not its rates,
so the gap could be reached.

The recorded cases (`testdata/outcomes.txt`), with a 1,135-unit bounded
reservation, show it. Units charged after `message_start`, before and after
this decision, with the completed call for comparison:

| Missing rate | Interrupted before | Interrupted after | Completed |
| --- | --- | --- | --- |
| Cache write (5 minute, 50 tokens) | 210 units, 210 micro-dollars | 1,135 units, 1,135 micro-dollars | 1,135 units |
| Cache read (40 tokens) | 160 units, 120 micro-dollars | 1,135 units, 1,135 micro-dollars | 1,135 units |
| Input (120 tokens) | 160 units, 40 micro-dollars | 1,135 units, 1,000 micro-dollars | 1,135 units |
| Output (1 token at `message_start`) | 160 units, 135 micro-dollars | 1,135 units, 135 micro-dollars | 1,135 units |

After `message_delta` the estimate was 218 units in the last three rows. The
cost in micro-dollars is what the call's reservation held, which depends on the
price.

Constraints:

- The gateway must not charge a call less than settlement would, because the
  workload chooses when to interrupt.
- Only an Anthropic call under a cost cap is affected. OpenAI admission
  already refuses a model whose price lacks a positive input, cached-input or
  output rate, so an OpenAI call with a missing rate never starts. A call
  without a cost cap prices nothing.
- A pricing file the gateway loads must have positive input, cached-input and
  output rates, so in practice only the optional cache-write rates are
  missing. The other categories are reachable only through a pricing table
  built in code, but the rule must not depend on which category is missing.

## Decision

An Anthropic call under a cost cap that is interrupted after its stream has
reported a non-zero count in any token category the model's price has no rate
for is charged its whole reservation, as a completed call with that usage is.
The check asks `costAnthropic`, the function that prices a completed call,
whether the usage reported so far can be priced, so the estimate and settlement
cannot disagree about what is unpriced. The charge is audited as `usageSource`
`reservation` with the interrupt's own `endReason`, and logged as `Charged model
reservation in full` with the detail `administrator pricing is unavailable`,
exactly as for a completed unpriced call. A call that has reported no usage yet,
or only zero counts in an unrated category, is estimated as before, and so is
every call without a cost cap.

## Alternatives

- **Require the rates at admission.** Refuse a request for prompt caching on a
  model whose price lacks a cache-write rate. It tells the administrator at
  once, and it would charge nothing. It was rejected because the request does
  not say whether the provider will write a cache, and a cache write is
  reported only after the work is done. Admission would refuse every call to
  the model or none, and the existing rule for a completed call would still
  be needed.
- **Make settlement fall back to another rate, as the estimate did.** The
  completed call would be priced at the input rate for a missing cache-write
  rate. It charges less than the whole reservation, and the estimate and
  settlement would agree. It was rejected because the fallback is a guess at a
  price the administrator did not set, a cache write costs more than input,
  and an under-charge breaks the cost cap the administrator did set.
- **Leave the difference.** The completed path is the rare one that charges
  the reservation, and interrupting is already cheaper than completing. It was
  rejected because the difference is chosen by the workload, which can reach
  the cheaper charge by cancelling.

## Tradeoffs and consequences

- An interrupted call on a model with an incomplete price is charged more than
  before: the whole reservation, not an estimate. A harness that cancels often
  while using prompt caching on such a model uses its cost cap faster.
- An administrator learns of an incomplete price from the log line and the
  `reservation` usage source in the audit event, not from a refusal. The
  reference pages say to carry both cache-write rates for a model used with
  prompt caching.
- The estimate no longer has a rate to fall back to, so the fallback in the
  estimate's pricing is gone.
- A zero count in an unrated category is not unpriced, so an output count of 0
  at `message_start` leaves the call estimated, and the estimate prices its
  output at the model's output rate.

## Revisit when

- The pricing file's loader accepts models with fewer required rates, or the
  gateway reads rates from a provider rather than a file. The rule then
  depends on what a price can lack.
- Telemetry shows calls charged this way are common. Then admission should
  require the rates, or the rule should change.
- The OpenAI protocol admits a model with a missing rate. It has the same
  disagreement then, and the same rule should apply.

## Links

- Implementation: `internal/endpoints/model/call.go`
  (`reportedUsageUnpriced`, `settleInterrupted`),
  `internal/endpoints/model/pricing.go` (`costAnthropic`).
- Tests: `internal/endpoints/model/testdata/outcomes.txt` (rows "under a cost
  cap").
- References: [Model provider authentication](../guides/model-provider-auth.md#interrupted-calls),
  [Harnesses](../reference/harnesses.md),
  [Required secrets and config](../reference/required-secrets-and-config.md#model-pricing-format)
  and [Security hardening](../reference/security-hardening.md).
- Related: [ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md)
  (the estimate and the whole-reservation charge for what cannot be estimated).
