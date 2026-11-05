# 0052: Relay Copilot's policy-fetch query

Status: accepted
Date: 2026-11-04

The account-route scope below is expanded by
[ADR 0053](0053-copilot-account-namespaces-and-policy-verification.md).

## Problem and constraints

Copilot 1.0.103 sends `client_id`, `client_version` and
`copilot_runtime_version` with `GET /copilot_internal/managed_settings`.
The provider table allowed that method and path but refused queries, which
prevented Copilot from retrieving the account's MCP policy. The audit record
did not distinguish a query refusal from a method mismatch.

## Decision

Use the existing relay query support for this exact method and path. Forward
the request and provider response unchanged, including policy refusals. The
provider decides which MCP servers the account may use. Other account routes,
methods and hosts keep their restrictions from
[ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md).

Relay refusals report a recognized HTTP method and whether a query is present
in the existing denial reason. They never record query names, query values,
headers or bodies. Unrecognized methods are described as unrecognized because
they can contain arbitrary text.

## Alternatives and tradeoffs

Allowing queries on every account route would extend access without an
observed need. Returning a permissive local policy would bypass the account's
rules. Both are rejected. Restricting this endpoint's query to today's three
keys would duplicate the provider's parameter validation and break again
when it adds another client metadata field. The selected route forwards
queries only to the fixed policy endpoint, within the relay's existing bounds.

Query presence diagnoses the mismatch without exposing query content, but it
cannot identify a new parameter. Capture new request shapes with an isolated
fixture client before extending another route.

## Verification and revisit conditions

Handler tests exercise the observed query, preserve both policy data and an
upstream 403, and prove that refused methods, paths and queries on other
account routes never reach the provider. The installed-client verifier now
requires Copilot's policy request to reach its fixture provider as well as
completing the MCP tool and diagnosis scenarios.

Revisit when Copilot changes its policy endpoint or adds another account
request. Device-code sign-in remains the documented container flow because a
host browser cannot reach the container's loopback callback.

## Implementation

[Provider routes](../../internal/endpoints/model/providers.go),
[relay and refusal diagnostics](../../internal/endpoints/model/signin.go),
[client verifier](../../hack/verify/harness-process/main.go),
[local sign-in](../guides/local-sessions.md#the-agents-own-sign-in).
