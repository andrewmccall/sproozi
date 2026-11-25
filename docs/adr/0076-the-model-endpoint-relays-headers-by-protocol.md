# 0076: The model endpoint relays headers by protocol, and responses drop a deny-list

Status: accepted
Date: 2026-11-25

Extends [ADR 0002](0002-native-cli-harnesses-and-model-protocols.md) and
[ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md).

## Problem and constraints

A harness calls a model provider through the gateway's model endpoint. The
gateway holds the provider's key, or forwards the harness's own sign-in, and
relays the answer to the sandbox. Two header lists decide what crosses: the
request headers that reach the provider and the response headers that reach
the sandbox. No record said what they were. ADR 0002 says only that the
gateway installs the key "after filtering client headers". ADR 0047 says that
on a forwarding route the workload's headers choose the account. The
reference pages said nothing about response headers.

The code has two rules, one per protocol, and
`testdata/header-outcomes.txt` records their result for every provider route
and credential mode:

- **Anthropic Messages** uses fixed lists in both directions.
- **OpenAI** (OpenAI, the ChatGPT backend and Copilot's chat route) relayed
  every provider response header to the sandbox, including `Set-Cookie` and
  `Openai-Organization`. Under a gateway-held key the latter is the
  organisation ID of the administrator's key. On the request side it
  forwarded every client header that is not a credential, a cookie or an
  account selector.

Constraints:

- A sandbox must learn nothing about the administrator's account from a
  response, and must not be handed state it cannot use.
- Harness clients send feature and beta headers that change often, and the
  gateway cannot know them ahead of the harnesses.
- The rules must be the same in every credential mode where a mode gives no
  reason for a difference.

## Decision

**Requests to the provider.**

- On every route the gateway never forwards `Cookie`, `Proxy-Authorization`
  or `Content-Encoding` (it forwards the body it decoded and inspected), and
  removes `Accept-Encoding` so its own transport negotiates compression.
- The harness's `Authorization` and `X-Api-Key` are forwarded only when the
  harness signs in itself (workload mode). Otherwise the gateway installs its
  own credential. The OpenAI key, token and ChatGPT credentials also remove
  the harness's `OpenAI-Organization`, `OpenAI-Project` and
  `ChatGPT-Account-ID`, so the credential chooses the account. In workload
  mode those three are forwarded, as ADR 0047 allows.
- **Anthropic Messages** forwards a fixed list: `Content-Type`, `Accept`,
  `Anthropic-Beta` and the credential headers above. `Anthropic-Version` is
  set to `2023-06-01` whatever the harness sent. Copilot's Messages route
  (`KeepClientHeaders`) forwards the harness's other headers as well.
- **OpenAI** forwards everything else the harness sent. This is intended.

When the harness supplies its own Anthropic sign-in, both `Authorization` and
`X-Api-Key` are forwarded and the provider chooses between them; the header
record shows both reaching `api.anthropic.com`.

**Responses to the sandbox.**

- **Anthropic Messages** relays `Content-Type` and `Request-Id`, nothing else.
  A provider's non-2xx answer is replaced by the gateway's own error.
- **OpenAI** relays every header except `Set-Cookie`, `Set-Cookie2`,
  `Openai-Organization` and `Openai-Project`, matched on canonical names
  (`deniedResponseHeaders` in `internal/endpoints/model/handler.go`). This
  holds in every credential mode, and for streamed, JSON, non-2xx and model
  discovery answers: all pass through one function, `copyResponseHeaders`.
  The diagnostics path (`describeResponse`) reads `X-Request-Id` and
  `Content-Type` into the gateway's own record and copies no header to the
  sandbox.

A header belongs in the deny-list when it identifies the account,
organisation or project of the credential the gateway sent, or sets client
state the gateway will never send back (it strips `Cookie` on every request).
Request IDs, rate limits and processing times are not in it.

The denied headers are dropped in every mode, not only under a gateway-held
key. With the harness's own sign-in the organisation is the harness user's
own, but a relayed `Set-Cookie` is useless in every mode, one rule is simpler
to state and test, and no client code in the repository reads any of the four.

## Alternatives

- **Allow-lists in both directions.** Strongest, and what Anthropic Messages
  does. For requests it would break a harness each time a client adds a
  feature or beta header, and they do so often. For OpenAI responses it
  would drop rate-limit and request-ID headers that a harness can use for
  backoff and support, for no identity gain.
- **Relay everything on responses.** The behaviour before this record. It
  gives the sandbox the administrator's organisation and project and a cookie
  it cannot use.
- **Deny the response headers only under a gateway-held key.** It keeps
  everything for a forwarded sign-in, but the exception serves no client and
  doubles the cases the record must show.
- **An allow-list for OpenAI responses.** Rejected for the first reason
  above, and because a provider adds response headers without notice.

## Tradeoffs and consequences

- A new identifying response header that OpenAI or Copilot adds is relayed
  until `deniedResponseHeaders` names it. The list is kept short and is
  checked by the header record, not by the provider.
- A harness can send any non-credential header to the provider under the
  administrator's key, such as a client identifier or a feature flag the
  provider honours. The gateway's checks cover the path, body, budget and
  credential, not these headers.
- A forwarded sign-in's own account selectors reach the provider, because
  they are the harness user's.
- Anthropic's lists are stricter than OpenAI's. The difference is recorded, not
  accidental, and `testdata/header-outcomes.txt` shows it for each route.

## Revisit conditions

- A provider adds a response header that identifies the credential's account.
  Add it to `deniedResponseHeaders` and the record in one change.
- A harness depends on a denied header, or a provider needs a request header
  the OpenAI rule would pass but policy should refuse. Move that direction to
  an allow-list.
- A request-side header lets a harness change billing or account behaviour
  at the provider. Add it to the strip list or move requests to an allow-list.

## Links

- Implementation: `internal/endpoints/model/handler.go`
  (`copyProviderHeaders`, `copyResponseHeaders`, `deniedResponseHeaders`),
  `internal/modelauth/auth.go` (account selectors)
- Tests: `internal/endpoints/model/header_record_test.go` and
  `testdata/header-outcomes.txt`;
  `internal/endpoints/model/response_headers_test.go`
- Reference: [headers on model calls](../reference/harnesses.md#headers-on-model-calls)
- Related: [ADR 0002](0002-native-cli-harnesses-and-model-protocols.md),
  [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md),
  [ADR 0074](0074-account-requests-only-with-a-forwarded-sign-in-or-no-api-authority.md)
