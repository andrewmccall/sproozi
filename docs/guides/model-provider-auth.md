# Model provider authentication

This guide covers OpenAI authentication for the Kubernetes gateway. An
administrator selects one of two modes. For Anthropic configuration, see [model
providers](../reference/harnesses.md#model-providers). `MODEL_AUTH_MODE=api_key`
is the default and uses `OPENAI_API_KEY`. `MODEL_AUTH_MODE=chatgpt` uses an
independently authorized ChatGPT-plan OAuth sign-in, the ChatGPT credential. It does not read Codex's
login cache and does not fall back to an API key if OAuth fails.

This is separate from workload authentication. AgentRuns still authenticate to
the shared gateway with their projected `sproozi-gateway` ServiceAccount token.
The sandbox receives neither a provider API key nor OAuth credentials. The
stock Codex client keeps its OpenAI-compatible provider configuration and its
harmless API-key placeholder in either mode.

## Existing cluster

To use a ChatGPT plan on a cluster you installed yourself, follow
[Run a model on this cluster](install-kubernetes.md#7-run-a-model-on-this-cluster).
It uses the same sign-in helper, Secret, ConfigMap and `--model-auth chatgpt`
policy budget as the Kind demo below.

## Kind demo

On your local desktop, run:

```bash
make demo-chatgpt-login
```

For a first login, approve Sproozi's requested ChatGPT-plan permissions in the browser. The helper
starts a loopback-only callback listener, performs PKCE, validates the ID token
using published JWKS, issuer, audience, expiry and nonce, and requires the
`chatgpt.tokens.use.direct` scope. Returning sign-ins retain the issued client
ID and reject changes to the validated account identity. This uses OpenAI's
[documented registration and sign-in flow](https://developers.openai.com/siwc/token-sharing-open-source/sign-in).

The helper saves `.local/chatgpt-session.json` atomically with mode `0600`,
retains a stable host registration in `.local/chatgpt-host.json`, and updates
the gitignored `.local/demo.env` with the selected mode and credential path.
Your existing API key and GitHub App configuration are preserved. The demo uses
**gpt-6.1-sol**. The helper reuses an existing, unexpired private ChatGPT credential without
repeating browser consent, and prints a warning that the credential must not be
in use by another gateway. It checks the account's model catalog first. If the
exact model is absent, it sends one tiny inference using low reasoning effort;
this consumes plan tokens and must return `response.completed` for that exact
model. Catalog visibility can lag actual access. There is no model fallback.
If verification fails, the login remains saved but the demo mode is not switched.

To deliberately renew browser consent (including before bootstrapping another
cluster after the live gateway has rotated its credentials), run:

```bash
bash hack/demo/chatgpt-login.sh --reauthorize
```

`make demo-chatgpt-login CHATGPT_REAUTHORIZE=1` does the same through make.

Then start a new isolated demo cluster:

```bash
make demo IMG=controller:sproozi-demo KIND_CLUSTER=sproozi-chatgpt-demo
```

The setup creates the `model-gateway-chatgpt` Secret in `sproozi-system` and
the `model-gateway-auth` ConfigMap selecting `chatgpt`. Only the trusted gateway
has get/update permission on that exact Secret. It serializes refreshes and
persists rotating access/refresh tokens together before using them. A failed
credential read or refresh fails closed; an unsaved refresh replacement is
retried without performing a second rotation in the same process.

Model requests use the gateway's in-memory copy of the ChatGPT credential. The gateway
reads the Secret again at most every 10 seconds, and sooner when the access
token is within a minute of expiry. If you replace or remove the Secret, every
model request that authorizes 10 seconds or more after the change uses it,
without a gateway restart. Only a
reload or refresh takes the gateway's lock, so concurrent model requests don't
wait on each other.

Each renewable ChatGPT sign-in has **one active gateway process**. The supplied
Deployment is one replica with `Recreate`, so updates do not overlap refresh
owners ([ADR 0026](../adr/0026-single-gateway-refresh-owner-with-recreate.md)). Do not scale it, share its ChatGPT credential with another cluster, or run another
refreshing client against its credentials. The Kubernetes Secret becomes the
authoritative credential record after deployment. The local bootstrap file
is not synchronized back after refresh: sign in again before provisioning
another cluster. A crash after provider rotation but before persistence can
require reauthorization. Coordinated multi-replica refresh is not implemented.
Do not let a GitOps reconciler restore an old bootstrap credential record over
the rotating live Secret.
These constraints follow the provider's
[rotating-session requirements](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions).

## Protocol and budgets

The gateway accepts only these routes and refuses any other with a 403:

| Provider and mode | Routes | Other routes get |
| --- | --- | --- |
| OpenAI, API key | `GET /v1/models`; `POST /v1/responses`, JSON or SSE; `POST /v1/chat/completions`, JSON only (a streaming request gets a 403 with code `streaming_not_supported`) | code `path_not_allowed` |
| OpenAI, ChatGPT mode | `GET /v1/models`; `POST /v1/responses` with `store:false` and `stream:true` only | code `chatgpt_request_not_supported` for chat completions (`streaming_not_supported` if the request streams), otherwise `path_not_allowed` |
| Anthropic | `POST /v1/messages`, optionally with `?beta=true`, JSON or SSE; see [model providers](../reference/harnesses.md#model-providers) | `unsupported Anthropic operation` |

Query strings, an explicit empty query and escaped-path variants are rejected. Bodyless model discovery
does not require a pricing row. Both API-key and ChatGPT authentication remove
workload `OpenAI-Organization`, `OpenAI-Project` and `ChatGPT-Account-ID` headers.
The configured credential selects provider identity; use an appropriately scoped
API key rather than sandbox-supplied account selectors.

Cost-bounded API-key inference requires the exact requested model's positive
administrator input, cached-input and output rates before forwarding. Omitted
`service_tier` becomes `default`; explicit nondefault tiers are refused so the
gateway's standard token rates do not knowingly price a different tier.
Token-only requests retain their existing behavior without a pricing requirement.
The [pricing format](../reference/required-secrets-and-config.md#model-pricing-format)
describes the table. The table still omits non-token billing, and the API-key
route has no complete billable-field allowlist. Provider-hosted tool, storage and
image/audio charges require separate coverage; harness-executed tools keep
their existing request semantics.

The gateway also refuses these requests before it reserves anything or
contacts the provider, so none of them is charged. A 400 has type
`invalid_request_error` and a 403 has type `permission_error`. The code is in
the OpenAI body only; the Anthropic body has no code field:

| Request | Status and code | Message |
| --- | --- | --- |
| A body that isn't valid JSON or that Go's `encoding/json` can't decode, such as one nested deeper than 10,000 levels | 400 `invalid_json` | `request body is not valid JSON` |
| A body that is valid JSON but not an object, such as an array, a string or `null` | 400 `invalid_json` | `request body must be a JSON object` |
| A number too large to read, such as `1e400` | 400 `number_too_large` | `request body contains a number too large to read` |
| Data after the body's one JSON value | 400 `trailing_data` | `request body must be exactly one JSON value; remove the data after it` |
| A body that repeats a key, or has keys that differ only by case in the request, its messages or their content blocks | 400 `ambiguous_json` | `request body must be one JSON value without duplicate keys; the keys of the request, its messages and their content blocks are compared without case` |
| `stream` other than omitted, `null`, `true` or `false`, such as `"true"` or `1` | 400 `invalid_field` | `request body field stream must be a boolean` |
| `background` other than omitted, `null` or `false` | 400 `unsupported_request` | `background responses are not supported by the model gateway; background must be omitted or the boolean false` |
| `n` other than omitted, `null` or a JSON number equal to 1, such as `1`, `1.0` or `1e0` (a quoted `"1"`, 0 and 128 are refused) | 400 `unsupported_request` | `n must be 1 (or omitted): the model gateway bounds one output per request; send one request per output` |
| `best_of` other than omitted, `null` or a JSON number equal to 1, as for `n` | 400 `unsupported_request` | `best_of must be 1 (or omitted): the model gateway bounds one output per request; send one request per output` |
| A cost-capped call for a model without the administrator pricing it needs, in API-key mode or on Anthropic | 403 `pricing_unavailable` | `administrator pricing is unavailable` |
| Any other operation or option the gateway doesn't support, such as a nondefault `service_tier` or a missing `model` under a cost cap, in API-key mode or on Anthropic | 403 `unsupported_operation` | The reason, such as `OpenAI cost-capped inference requires the default service tier` |
| ChatGPT mode: a policy whose `model.inference` budget has a nonzero `maxCostMicros` | 403 `chatgpt_request_not_supported` | `ChatGPT plan usage requires a token-only policy; API dollar caps do not apply` |
| ChatGPT mode: any route other than `GET /v1/models` and `POST /v1/responses`, such as chat completions | 403 `chatgpt_request_not_supported` | `ChatGPT plan usage supports only Responses inference and model discovery` |
| ChatGPT mode: a Responses request with an empty body | 403 `chatgpt_request_not_supported` | `invalid ChatGPT Responses request` |
| ChatGPT mode: `store` other than `false`, or `stream` other than `true` | 403 `chatgpt_request_not_supported` | `ChatGPT Responses requires store:false and stream:true` |
| ChatGPT mode: `input` missing, `null` or not an array | 403 `chatgpt_request_not_supported` | `ChatGPT Responses requires an input array` |
| ChatGPT mode: any of `background`, `conversation`, `max_output_tokens`, `max_tool_calls`, `metadata`, `moderation`, `multi_agent`, `prompt`, `prompt_cache_retention`, `safety_identifier`, `temperature`, `top_logprobs`, `top_p`, `truncation`, `user` or `previous_response_id`, with any value, even `null` | 403 `chatgpt_request_not_supported` | `ChatGPT Responses request contains an unsupported field` |

A background response returns before the provider has done the work, without
its usage, and `n` or `best_of` multiplies the output under one output bound,
so the gateway could neither see nor bound what the provider bills.

ChatGPT mode uses the public `https://api.openai.com/v1/responses` endpoint,
not a ChatGPT backend endpoint. Inference requires `store:false`, `stream:true`
and an input array. Unsupported request fields are rejected, not silently
removed; the table above lists each refusal. Chat completions are unavailable
in this mode; model discovery is allowed. Account-selection headers supplied by
the workload are discarded. If the saved ChatGPT credential can't be used, for
example because it lacks the `chatgpt.tokens.use.direct` scope, each call gets
a 503 with code `provider_auth_unavailable` and `provider authentication is
unavailable`; sign in again.
See the provider's
[inference contract](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)
and [preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations).

Both modes check capabilities, current policy and token admission. They
settle each call from the usage the provider reports, or
[estimate](#interrupted-calls) an interrupted one, then record accounting and
audit events. Audit events include the non-secret `providerAuthMode`. ChatGPT mode requires a
token-only policy: the demo renderer sets `spec.budgets["model.inference"]` to
`maxCostMicros: 0` and `maxUnits: 300000`. Stock Codex resends its instructions
and conversation on each turn; the original 100000 token demo budget was
exhausted before creating a PR. API-key mode retains 100000 tokens and its
500000-microdollar cap. A nonzero API-dollar cap is rejected before inference
because API pricing is not the cost of subscription usage. This is not a
provider-side output-token ceiling: the preview does not accept
`max_output_tokens`, so every call reserves all remaining tokens, and an
unexpectedly large response can consume tokens before the gateway reads its
usage.

To switch back to API-key billing, select `api_key` in
`bash hack/demo/configure.sh`. API-key mode uses the pricing and standard-tier
checks above and retains the documented reservation and settlement limits.
An AgentRun cannot select the authentication mode.

## Current limits

These sections describe how the gateway streams and charges model calls.
[ADR 0028](../adr/0028-streamed-model-responses-and-estimated-interrupts.md)
records why. Here a run's [budget](../../GLOSSARY.md#budget) is its
`model.inference` budget on Kubernetes or, on the home gateway, the
execution's [allowance](../../GLOSSARY.md#allowance), which the home daemon's
`allowances` configuration sets; the gateway charges both the same way.

### Long model calls

The gateway passes the provider's response to the harness as it arrives, so a
long turn streams like it does without Sproozi. A model call can run for up to
the [model call deadline](../../GLOSSARY.md#model-call-deadline), counted
from when the gateway receives the request:

- 20 minutes by default;
- on the shared Kubernetes gateway, set by
  `SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` on the gateway Deployment (1 to
  3600; see [required configuration](../reference/required-secrets-and-config.md#objects));
- on the home gateway, set by `gateway.maxModelCallSeconds` in the home
  configuration (1 to 3600; omit it for 1200; see
  [the home CLI reference](../../cmd/sproozi/README.md#gateway-certificates-and-provider));
- lowered for one policy's runs by
  [`AgentPolicy.spec.maxModelCallSeconds`](../reference/agentpolicy.md#model-call-deadline),
  which can't raise the gateway's maximum.

A harness reaches the provider through a CONNECT tunnel to the gateway, and the
gateway closes each tunnel 5 minutes after it opens. A model call can outlive
that: the tunnel stays open for the call until it ends. Other requests on the
tunnel still have to finish by its request deadline, 10 seconds before the
5 minutes are up. At that deadline the gateway turns off keep-alive, so the
harness opens a new tunnel for its next request:

- An idle tunnel closes.
- A running request's reply carries `Connection: close`, if its headers
  haven't been sent yet.
- A request that arrives after the deadline gets a 503 with
  `Connection: close` and the plain-text body `gateway connection reached its
  lifetime; open a new connection and retry`. Nothing is reserved or charged.

Three other timers apply to a model call:

- **Response headers.** The shared gateway waits up to 4 minutes 50 seconds
  for the provider's response headers, and the home gateway waits 45 seconds.
  A streamed response sends its headers within seconds. A call that doesn't
  stream (`stream` false or absent) sends them only once the model has
  finished, so it must finish within that wait. If the headers don't arrive,
  the harness gets a 502 with `upstream request failed` (`endReason`
  `upstream_error`), or, if the gateway [stopped](#gateway-restarts) first, a
  503 with `the model gateway is shutting down; retry the call` (for OpenAI,
  code `gateway_stopping`; `endReason` `gateway_restart`). Either way the
  provider already had the request, so the call is charged an estimate; see [interrupted calls](#interrupted-calls). The home gateway's wait is separate from the CLI's 45-second
  timeout for requests to a profile service, described in
  [the home CLI reference](../../cmd/sproozi/README.md#recover-or-cancel).
- **Idle provider.** The gateway ends a response that sends nothing for 5
  minutes, and charges it as [interrupted](#interrupted-calls) (`endReason`
  `upstream_error`).
- **Waiting for budget.** A call that waits for other calls from the same run
  to settle before its reservation fits in the run's budget waits until its
  deadline at most. It then gets a 408 with `model request cancelled while
  waiting for its budget` (for OpenAI, code `request_cancelled`). Nothing
  is charged. The audit record is allowed, with `endReason` `deadline`;
  `client_cancelled` if the harness left while it waited; or
  `gateway_restart` if the gateway [stopped](#gateway-restarts) while it
  waited. A gateway stop instead gets a 503 with `the model gateway is
  shutting down; retry the call` (for OpenAI, code `gateway_stopping`), and is
  not charged either.

At the model call deadline the stream simply ends, without an error event, and
the call is charged as interrupted (audit `endReason` `deadline`). The harness
may retry, and the retry starts the turn again. If turns keep reaching the
deadline, raise it or choose a model or reasoning setting that finishes turns
sooner. Editing `maxModelCallSeconds` on a policy is a spec edit, so it closes
the run's open connections, including a call in progress, like any other spec
edit.

### Interrupted calls

A call is interrupted when, after the whole request was written to the
provider:

- the harness cancels or disconnects, for example when you press Esc in Claude
  Code or Codex, or a revocation closes its connection;
- the call reaches its deadline, the provider's response headers don't arrive
  in time, or the provider sends nothing for 5 minutes;
- the gateway [stops](#gateway-restarts) while the call runs;
- the connection to the provider fails;
- the stream ends without its terminal event and usage, or with an error event
  and no usage;
- a completed response is missing valid usage, or a streamed response has the
  wrong content type.

An interrupted call is charged even when it ends before the response headers
arrive. A call that doesn't stream receives its headers only when the model
has finished, so by then the provider may have done, and billed, the whole
response. Leaving between the headers and the end of the body isn't free
either: if a response that doesn't stream has arrived whole, the call settles
from its usage (`usageSource` `provider`, `endReason` `client_cancelled`), and
otherwise it is estimated. Pressing Esc is free only in the cases listed in
[calls that aren't charged](#calls-that-arent-charged). A cancel by the
harness gets no reply and is audited as allowed with `endReason`
`client_cancelled`, not as a provider failure.

An interrupted call is charged an estimate, never more than its
[reservation](../../GLOSSARY.md#reservation):

- **Input.** On Anthropic, the exact input, cache-read and cache-write tokens
  from `message_start`, once it has arrived. Otherwise, and always on OpenAI,
  the request body's bytes divided by 4, priced at the normal input rate.
- **Output.** The larger of the visible output bytes the harness received,
  counted as one token each, and 250 tokens for each second since the request
  was fully written to the provider, but no more than the request's output
  bound (`max_tokens`, `max_completion_tokens` or `max_output_tokens`), when it
  has one. The seconds count from the response headers only if the transport
  doesn't report the write. Visible output is the streamed text, tool-call
  arguments and thinking; a call interrupted before its headers has none. On
  Anthropic, once `message_delta` has reported the exact output, the output is
  that number instead.

The time term means cancelling while the model reasons, where its output is
hidden, still pays for that reasoning.

For example, Claude Code sends a 200,000-byte request with `max_tokens`
32,000, under a 300,000-token budget. `message_start` reports 1,500 input,
45,000 cache-read and 2,000 cache-write tokens, so 48,500 input tokens. You
press Esc 40 seconds after the request was fully written to the provider,
when 3,000 bytes of output have appeared. The output term is the larger of
3,000 and 40 × 250 = 10,000, so 10,000, below the 32,000 bound. The charge is
58,500 tokens, well within the call's 232,000-token reservation.

The same Esc on a Codex call with a 400,000-byte request estimates 100,000
input tokens. Codex sends no output bound, so a call cancelled 2 minutes
after its request was written, with less than 30,000 bytes of visible output, is charged
100,000 + 120 × 250 = 130,000 tokens, or whatever remains of the budget if that
is less.

The estimate is not the provider's bill, and it can be wrong either way:

- **Under-charge.** Hidden reasoning that runs faster than 250 tokens a
  second, or input that tokenises at fewer than 4 bytes a token, such as some
  code or non-English text, costs more than the estimate.
- **Over-charge.** Counting each visible byte as a token overstates English
  output about four times. Time spent while the provider is slow counts at
  250 tokens a second. On OpenAI, input estimated from bytes ignores cache
  discounts.

The gateway logs each estimate as `Charged estimated model usage` with
`runID`, `runUID`, `endReason`, `units`, `costMicros`, `inputSource`
(`provider` or `request_bytes`), `visibleBytes` and `elapsed`. The audit
record carries `usageSource` `estimated` and the charged units in
`tokensUsed`.

If the ledger refuses a charge from usage, an estimate or the whole
reservation, the gateway doesn't log the charge, and the audit record carries
`tokensUsed` 0 with one of two `usageSource` values:

- `recovered`: the call's [lease](#gateway-restarts) had already expired and
  been settled at its recovery charge, which stands. The gateway logs `Model
  reservation was already recovered` with `runID`, `runUID` and `endReason`.
- `unsettled`: the ledger recorded nothing for this call, for example because
  the update failed. The gateway logs `Failed to settle model reservation`
  with `runID`, `runUID`, `endReason` and `error`. After a failed update the
  reservation's lease stays, and is settled at its recovery charge once it
  expires.

A call that completes with its terminal event and usage (`message_stop` after
`message_delta`, `response.completed` or `response.incomplete`, or a
`response.failed` event that carries usage) is charged the usage the provider
reported, as `usageSource` `provider`.

Each audit record's `endReason` says how the call ended, and `usageSource`
how it was charged:

Any `endReason` can go with `usageSource` `reservation`, charged the whole
reservation, when the call is [unverifiable](#unverifiable-calls): an
interrupted request that references input the gateway can't size, or reported
usage whose cost can't be priced.

| `endReason` | Cause | `usageSource` and charge |
| --- | --- | --- |
| (no `endReason`) | The audit record has no `endReason` field: the gateway refused the call before reserving; it failed after reserving but before contacting the provider; the provider returned an error response; or the request was never fully written for a reason other than the harness leaving or a gateway stop. See [calls that aren't charged](#calls-that-arent-charged). | No `usageSource`; nothing is charged. |
| `completed` | The response reached its terminal event with usage. | `provider`: the reported usage. |
| `exceeded` | The provider reported usage above the call's own reservation, whether in a completed response, a `response.failed` event or a whole JSON body that arrived before the harness left. | `provider`: the reported usage, capped as in [usage above the reservation](#usage-above-the-reservation). |
| `client_cancelled` | The harness cancelled or disconnected. | None while the call waited for its budget or before the request was fully written; `provider` (the reported usage) if a response that doesn't stream had arrived whole; otherwise `estimated`: the estimate. |
| `deadline` | The call reached its model call deadline. | None while the call waited for its budget (the 408 above); `provider` if a response that doesn't stream had arrived whole; otherwise `estimated`: the estimate. |
| `gateway_restart` | The gateway stopped while the call ran or waited; see [gateway restarts](#gateway-restarts). A call that hadn't received its response headers gets a 503 (for OpenAI, code `gateway_stopping`). | None while the call waited for its budget or before the request was fully written; `provider` if a response that doesn't stream had arrived whole; otherwise `estimated`: the estimate. |
| `upstream_error` | After the request was written: the headers didn't arrive in time, the connection failed, the provider sent nothing for 5 minutes, the stream ended without its terminal event and usage or with an error event, or a response had missing usage or the wrong content type; or a `response.failed` event, with or without usage. | `provider` for a `response.failed` event that carries usage; otherwise `estimated`: the estimate. |
| `budget_cut` | An unbounded call's observed usage reached its reservation. | `reservation`: the whole reservation. |
| `parse_limit` | A provider event, or a response that doesn't stream, exceeded 8 MiB. | `reservation`: the whole reservation. |

`upstream_error` also appears as an error code, with a different meaning. A
502 body with `upstream request failed` and `"code":"upstream_error"` tells the
harness the gateway couldn't complete the call with the provider, and the
audit record is a failure. When the request was never fully written, that 502
releases the reservation and nothing is charged. When it was written and the
headers then failed, timed out or missed the model call deadline, the same 502
goes with an audit `endReason` `upstream_error` or `deadline` and an estimated
charge. A gateway stop never gets this 502: a call it ends before its response
headers always gets the 503 `the model gateway is shutting down; retry the
call` (for OpenAI, code `gateway_stopping`), with `endReason`
`gateway_restart`.

### Calls that aren't charged

A model call costs nothing in these cases, grouped by where it stopped:

- **Refused before inference.** Every refusal before the gateway reserves,
  such as the 400 and 403 refusals in [protocol and budgets](#protocol-and-budgets),
  a 413 for a body over 1 MiB, a 429 when the budget is exhausted (type
  `budget_error`; for OpenAI, code `budget_exhausted`) or the account has too
  many pending calls (type `rate_limit_error`; for OpenAI, code
  `too_many_pending_calls`), and a 503 while the gateway
  [stops](#gateway-restarts) or a connection reaches its lifetime. The 429's
  `budget_exhausted` refuses a call before it starts and charges nothing; it
  is not the `budget_exhausted` error event of a [budget cut](#budget-cut),
  which ends a call already streaming and charges the whole reservation. Model discovery (`GET /v1/models`) is never charged either.
- **Refused while waiting for its budget.** The 408 in
  [long model calls](#long-model-calls), including a harness that leaves
  while it waits, and the 503 (for OpenAI, code `gateway_stopping`) for a
  gateway that stops while it waits.
- **Failed after reserving, before contacting the provider.** A 503 with
  `provider authentication is unavailable` (for OpenAI, code
  `provider_auth_unavailable`) when the provider credential can't be used, or
  a 500 with `failed to build upstream request`. The gateway releases the
  reservation, and the audit record is a denial with no `endReason`.
- **The request never fully reached the provider.** A DNS, connect or TLS
  failure, the harness leaving, or the model call deadline or a gateway stop
  arriving before the request was fully written. The gateway releases the
  reservation. When the harness left, nothing is sent. A gateway stop gets a
  503 with `the model gateway is shutting down; retry the call` (for OpenAI,
  code `gateway_stopping`); anything else gets a 502 with `upstream request
  failed`.
- **The provider returned an error response**, such as a 400 or 429. The
  gateway releases the reservation. On OpenAI, in both modes, it relays the
  provider's status, headers and body, up to 1 MiB; a longer error body gets
  a 502 with code `upstream_error` and `failed to read upstream response`
  instead. On Anthropic it keeps the provider's status but replaces the
  headers and body with
  `{"type":"error","error":{"type":"api_error","message":"provider rejected request"}}`.

Any other call that reached the provider is charged its reported usage, an
[estimate](#interrupted-calls) or its [whole reservation](#unverifiable-calls).

### Unverifiable calls

The gateway charges a call's whole reservation in two situations: at a
[budget cut](#budget-cut), which ends an unbounded call that reached its
reservation, and when the call is unverifiable. An unverifiable call is one
whose usage the gateway can neither read from the provider nor estimate:

- **The request references input the gateway can't size**, and the call is
  interrupted. The references are `previous_response_id`, `conversation`, a
  `prompt` object, a `file_id` or uploaded container file, remote documents
  or images in message content, an `item_reference` input item, any input
  item that carries only `id`, `type` and `status`, and a reasoning item
  without `encrypted_content` or with an empty one. The request bytes don't
  contain that input, so they can't bound it. A reasoning item with a
  non-empty `encrypted_content` carries its content, so it doesn't count. Such a call
  settles from provider usage when it completes, as usual.
- **One provider event exceeds the gateway's 8 MiB parse limit**, or a response
  that doesn't stream exceeds 8 MiB. The gateway relays the rest unparsed and
  can't read its usage (audit `endReason` `parse_limit`).
- **Under a cost cap, an Anthropic response reports tokens in a category whose
  rate the administrator didn't set**, such as a cache write. The output has
  already reached the harness.

For either kind of whole-reservation charge, the gateway logs `Charged model
reservation in full` with `runID`, `runUID`, `endReason`, `detail`, `units`
and `costMicros`: what this call's own reservation held, 0 in a part of the
budget with no limit, so calls settling in parallel can't distort it. The
`detail` values are `request references input the gateway cannot size`,
`provider event exceeded the parse limit`, `administrator pricing is
unavailable` and, at a budget cut, `observed usage reached the reservation`.
The audit record carries `usageSource` `reservation` and the charged units in
`tokensUsed`. If the ledger refuses the charge, the gateway logs neither the
charge nor these units; see [interrupted calls](#interrupted-calls) for the
`recovered` and `unsettled` records it writes instead.

The charge applies only in the parts of the budget that have a limit. With no
unit limit no units are charged, and with no cost cap no cost is charged.

A cost-capped call for a model with no administrator pricing row is not
charged: both the OpenAI and Anthropic routes refuse it with a 403 and
`administrator pricing is unavailable` before inference. The OpenAI route also
needs positive input, cached input and output rates at that point. A provider
error response, such as a 400 or 429, releases the reservation and reaches the
harness as [calls that aren't charged](#calls-that-arent-charged) describes:
relayed on OpenAI, replaced on Anthropic. A provider that never received the
whole request also releases it, such as after a DNS, connect or TLS failure:
the harness gets a 502 with
`upstream request failed`. A failure after the request was written gets the
same 502 but is [estimated](#interrupted-calls). The 502's OpenAI body is
`{"error":{"message":"upstream request failed","type":"server_error","code":"upstream_error"}}`;
its Anthropic body has no code field. A [gateway stop](#gateway-restarts)
gets a 503 with `the model gateway is shutting down; retry the call` instead
of the 502 in either case (for OpenAI, code `gateway_stopping`).

### Reservations and parallel calls

Before forwarding a call, the gateway reserves what it may use. A
[bounded reservation](../../GLOSSARY.md#bounded-reservation) applies when the
request is self-contained and carries an output bound:

- **Units:** the request's bytes plus its output bound. A token is at least one
  byte, so the bytes bound the input.
- **Cost:** the request's bytes at the model's highest input or cache-write
  rate, plus the output bound at the output rate.

Any other request reserves all remaining units and cost. That includes a
request without an output bound, one that references input the gateway can't
size, and one that enables a tool the provider runs itself, such as web
search (including chat completions' `web_search_options`), web fetch, file
search, code execution, image generation or a remote MCP server, because the
provider bills that work separately. If the gateway can't read a request's
tools, for example because `tools` isn't an array, it treats them as
provider-run, and a body it can't read counts as referencing input it can't
size.

Which harnesses get which:

- **Claude Code** sends `max_tokens` on every request, so its self-contained
  requests are bounded.
- **Codex** sends no output bound, so each call reserves everything that
  remains. **ChatGPT mode** can't carry `max_output_tokens`, so every
  ChatGPT-mode call does too.

Calls from one run with bounded reservations run in parallel while their
reservations fit in what remains. A call that doesn't fit waits for an earlier
call to settle. When nothing else is pending, it reserves everything that
remains and runs alone, and it is refused only when nothing remains. A call
that reserves everything that remains also runs alone: it waits for calls in
flight to settle, and later calls wait for it. Bounded reservations overstate
most calls, because request bytes overstate input tokens. That limits how many
calls run at once, not how much a run can spend. Each account is separate, so
calls from different runs never wait for each other.

The ledger records every reservation as a [lease](../../GLOSSARY.md#lease)
until its call settles, and an account holds at most 64 leases. So at most 64
calls on one account can be pending at once, counting any
[abandoned](#gateway-restarts) leases not yet settled. A 65th gets a 429 with
type `rate_limit_error` and `too many model calls are pending on this budget;
retry when one finishes` (the code `too_many_pending_calls` is in the OpenAI
body only), and is not charged; retry it once a call has settled.

### Budget cut

While an unbounded call streams, the gateway compares its observed usage with
its reservation, at each event and once a second. Observed usage is the known
input, or the request bytes ÷ 4, plus the visible output bytes ÷ 4. This is
not how an [interrupt](#interrupted-calls) is estimated, which counts each
visible byte as one token: the cut uses visible bytes ÷ 4, close to real
usage for English, so it doesn't end a call at a quarter of its reservation.
The time term doesn't count here: elapsed time prices an interrupt but
never ends a call. Bounded calls are never cut: their reservation already
covers the request, and the provider stops their output at its bound. An
unbounded call whose request references input the gateway can't size is never
cut either: its request bytes don't bound that input, so observed usage means
nothing. It settles from the provider's usage when it completes, or is charged
its whole reservation if interrupted. When an
unbounded call's observed usage reaches its reservation, the gateway sends the
protocol's error event, cancels the provider call and charges the whole
reservation (audit `endReason` `budget_cut`). For OpenAI the event is:

```text
event: error
data: {"type":"error","code":"budget_exhausted","message":"model budget exhausted: the call reached its reservation","param":null}
```

For Anthropic it is:

```text
event: error
data: {"type":"error","error":{"type":"budget_error","message":"model budget exhausted: the call reached its reservation"}}
```

This event ends a call that is already streaming. It is not the 429 that
refuses a new call when the budget has no room, which shares its OpenAI code
`budget_exhausted` and its type `budget_error` but charges nothing; see
[calls that aren't charged](#calls-that-arent-charged).

### Usage above the reservation

A call's usage can also exceed its reservation without a cut, because hidden
reasoning and tokens the stream doesn't show aren't observed until the
provider reports its usage, and bounded calls are never cut. That output has
already reached the harness. `endReason` `exceeded` means the reported usage
was above the call's own reservation:

- Usage within the call's reservation is charged as reported and is never
  `exceeded`.
- Usage beyond it is charged `min(used, max(budget − settled − otherReserved,
  reserved))`, where `settled` is the run's usage already settled,
  `otherReserved` is what other pending calls still hold, and `reserved` is
  this call's reservation. Cost is capped the same way.

`budget` is the budget in force when the call reserved. So settled usage
never passes the run's budget, calls still in flight keep their reservations,
and later calls are refused once the budget is used up. Lowering a budget
changes the policy, which closes the run's connections: a call in flight is
cancelled and charged an estimate capped at its reservation. Only a call that
completes within the roughly one-second revocation window settles against the
old budget, so only it can charge above the lowered one.

Budgets are therefore not hard spending ceilings. Under a cost cap, one
unbounded call, such as any Codex call, can be charged all remaining cost.
Size each cost cap to what you would accept losing on one call, and set limits
per run rather than giving one run a large budget for many tasks. See
[model budgets](../reference/security-hardening.md#model-budgets-are-not-hard-spending-ceilings).

### Gateway restarts

On SIGTERM, such as during a rollout, the shared gateway drains its model
calls:

1. It stops accepting connections.
2. It cancels every model call in flight with `endReason` `gateway_restart`.
   A call the provider had received is charged as
   [interrupted](#interrupted-calls). A call still waiting for its budget, a
   call whose request wasn't fully written, and a call the provider had
   received that was still waiting for its response headers, such as one that
   doesn't stream, get a 503 with `the model gateway is shutting down; retry
   the call` (for OpenAI, code `gateway_stopping`; the Anthropic body has no
   code). Only the last is charged.
3. It waits up to 30 seconds for the calls to settle, then exits. The gateway
   Pod's `terminationGracePeriodSeconds` is 45, which leaves room for the wait.

A call that arrives on an open connection during the drain gets a 503 with
`the model gateway is shutting down; retry the call` (for OpenAI, code
`gateway_stopping`). Nothing is charged. The home daemon drains its model
calls the same way for up to 10 seconds when it closes.

Calls that settle at once can collide on the shared gateway's ledger, a
ConfigMap per account. A settle that collides retries with a randomised,
growing backoff for up to 30 seconds, so a drain normally settles every call.
A settle that still fails logs `Failed to settle model reservation` with the
error `budget: ledger update conflicted until its deadline: …`, its audit
record has `usageSource` `unsettled`, and its lease is left to the recovery
below.

Every reservation is recorded in the budget ledger as a
[lease](../../GLOSSARY.md#lease), which settling the call removes. A model
call's lease has:

- **Expiry.** The call's model call deadline plus 5 minutes.
- **Recovery charge.** The request bytes ÷ 4 for input, plus the whole model
  call deadline at 250 tokens a second for output, capped at the output bound
  and at the reservation. A request that references input the gateway can't
  size is charged its whole reservation.

GitHub, package, Kubernetes, egress and MCP requests that reserve budget get a
lease too. It expires at the request's deadline plus 5 minutes and charges the
whole reservation, as their gateway does when it can't settle them.

A lease outlives its call only when its gateway didn't settle it: the gateway
was killed without a drain, such as by SIGKILL or running out of memory, the
drain ran out of time, or the settle failed. After the expiry the lease is
settled at its recovery charge by whichever comes first:

- the next call or seal on that account;
- a sweep. The shared gateway sweeps every budget ConfigMap (label
  `app.kubernetes.io/component=budget-ledger`) when it starts and every
  minute after; the home daemon sweeps its own ledger files when it opens
  them and every minute after. A sweep that fails logs `Failed to recover
  expired budget reservations` with the error.

The process that settles it, the gateway, the home daemon or the service that
seals the account when an execution ends, saves the recovery in an update of
its own before the call or seal that found it, so the recovery persists even
if that reservation or seal then fails. Only after that save does it log
`Settled abandoned model reservation` for a model call's lease, or `Settled
abandoned budget reservation` for any other lease, with `account`,
`endReason` `gateway_restart`, `usageSource` `estimated`, `units` and
`costMicros`. `account` is the account key, or, when a sweep settled the
lease, the ConfigMap or ledger file name. There is no audit record for it. If
the original gateway tries to settle the call later, the ledger refuses it:
that gateway logs `Model reservation was already recovered` and its audit
record has `usageSource` `recovered` and `tokensUsed` 0.

A recovery, settle or sweep that finds the account deleted stops there: it
never recreates the account. The shared gateway's sweep lists budget
ConfigMaps 500 at a time. A ledger it can't decode is skipped with `Skipped
corrupt budget ledger`, with `configMap` and `error`, and the sweep carries on
with the others but reports that error as its failure.

Every reservation a current gateway makes has a lease, so a pending
reservation without one was left by a gateway from before leases. After such
an upgrade, the shared gateway runs a legacy pass before it serves: its sweep
also charges the whole reservation of every pending reservation without a
lease. If that pass fails, the gateway logs `Failed to recover legacy budget
reservations; retrying: <error>`, starts serving anyway and retries the pass
with backoff, from 1 second up to 1 minute, until it succeeds once. The home
daemon makes its first sweep after opening its ledger the legacy pass, and
repeats it on each sweep until one succeeds. Once a legacy charge is saved,
the process logs `Settled legacy budget reservation` with `account`,
`endReason` `gateway_restart`, `usageSource` `reservation`, `reservations`,
`units` and `costMicros`. See
[gateway restarts, upgrades and rollback](../reference/security-hardening.md#gateway-restarts-upgrades-and-rollback).

Until a lease expires its reservation still counts. An abandoned unbounded
call's lease holds everything that remained, so until the model call deadline
plus 5 minutes have passed:

- every later call on that account, unbounded ones included, gets 429;
- stopping or moving the session fails, because sealing its account fails
  with `budget: pending usage has no finite bound`. Retry after the expiry:
  the seal settles the expired lease first, then succeeds.

An abandoned lease also counts towards the account's
[64 pending calls](#reservations-and-parallel-calls).

### Accounts

This setup has no multi-account UI, sign-out command or multi-replica ChatGPT
credential coordination. You can disconnect access in ChatGPT Settings.
