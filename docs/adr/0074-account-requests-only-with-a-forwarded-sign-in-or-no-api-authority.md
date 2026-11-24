# 0074: Account requests are served only with a forwarded sign-in or for a provider with no API authority

Status: accepted
Date: 2026-11-24

Extends [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md)
and [ADR 0053](0053-copilot-account-namespaces-and-policy-verification.md).

## Problem and constraints

A provider-table entry can name account requests (`AccountAuthority`,
`AccountPrefixes`, `AccountRoutes`): the account and policy requests a
signed-in client makes on a host another capability owns, served as path
routes there. Copilot's, on `api.github.com`, are the only ones today
([ADR 0053](0053-copilot-account-namespaces-and-policy-verification.md)).
The relay attaches the provider's credential to each request it forwards to
that host.

The two gateway compositions decided differently when to serve them:

- **Kubernetes** served a provider's account requests with any credential
  it had: a gateway-held API key or token as well as the harness's own
  sign-in. It also kept the signed-in routes in a map keyed by authority, so
  in workload mode one provider's account route could replace another's on
  the same host.
- **Docker** served them only when the credential forwards the harness's own
  sign-in, or when the provider has no API authority and a gateway-held
  token is how its clients reach it.

Neither rule changed what is served today, because Copilot has no API
authority. A provider with both would differ. A review checked this by
giving the Anthropic entry a hypothetical account authority on
`api.github.com`: the Kubernetes composition served it with the gateway's
`AnthropicAPIKey`, or with its Anthropic sign-in token, and so sent that
credential to GitHub's origin. The Docker composition served nothing. Both
compositions now build their model routes with one function,
`Provider.Routes` ([ADR 0070](0070-gateway-routes-declare-admission-and-requests-record-one-decision.md)),
which has to choose one rule.

Constraints:

- A gateway-held credential is the administrator's, issued for the
  provider's API. It must not reach a host that API does not own.
- A harness's own sign-in is already the harness's to send, and a client
  signed in to a subscription makes these requests itself (ADR 0047).
- A provider with no API authority, such as Copilot, is reached only through
  its signed-in hosts, so a gateway-held token for it is meant for those
  hosts.

## Decision

`Provider.Routes` serves a provider's account requests only when its
credential forwards the harness's own sign-in, or when the provider has no
API authority. A gateway-held API key, token or ChatGPT sign-in for a
provider with an API authority serves that authority and nothing else.

This is the Docker composition's rule. Both compositions get it from
`Provider.Routes`, which appends one path route per account prefix, so two
providers' account routes on one host no longer replace each other.

`TestAGatewayHeldKeyServesNoAccountAuthority` builds a provider with both an
API authority and account requests and checks that a gateway-held key or
token builds no route for the account authority while a forwarded sign-in
does.

## Alternatives

- **Keep a rule per composition.** It changes nothing today. But the two
  compositions would disagree about the next such provider, and the
  Kubernetes rule sends the administrator's key to another capability's
  host.
- **Refuse account requests for a provider with an API authority when the
  table is defined.** This is stricter: such an entry could not be written.
  It would also refuse a provider whose signed-in clients make account
  requests that a forwarded sign-in can serve, which is the case these
  routes exist for.

## Tradeoffs and consequences

- No provider is affected today. Copilot is the only provider with account
  requests and has no API authority, so its routes are unchanged with a
  gateway-held GitHub token and with the harness's own sign-in.
- A future provider with an API authority and account requests serves the
  account requests only to harnesses that sign in themselves. A
  harness using the gateway's key for it gets no account routes and must
  work without them.
- The rule is in one place, `Provider.Routes`, and the model route records
  of both compositions (`testdata/model_routes.txt`) show which credential
  each account route holds.

## Revisit conditions

- A provider's account requests need the administrator's credential, for
  example an organisation policy that only an administrator's key may read.
  That needs its own credential setting for the account host, not the API
  key.
- A provider has account requests on a host its own API owns. They would
  then be ordinary routes of that authority, and this rule should not apply.

## Links

- Implementation: `internal/endpoints/model/providers.go` (`Provider.Routes`)
- Tests: `internal/endpoints/model/providers_test.go`,
  `cmd/gateway/testdata/model_routes.txt`,
  `internal/gateway/docker/testdata/model_routes.txt`
- Related: [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md)
  (provider facts are data the gateways build routes from),
  [ADR 0053](0053-copilot-account-namespaces-and-policy-verification.md)
  (Copilot's account namespaces),
  [ADR 0070](0070-gateway-routes-declare-admission-and-requests-record-one-decision.md)
  (model routes built from the provider table)
