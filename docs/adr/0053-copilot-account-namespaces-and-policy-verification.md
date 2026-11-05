# 0053: Copilot account namespaces and policy verification

Status: accepted
Date: 2026-11-05

## Problem and constraints

GitHub-backed Copilot reads account metadata under both `/copilot_internal/`
and `/copilot/` on `api.github.com`. The gateway mounted only the former.
In particular, `GET /copilot/mcp_registry` could not reach GitHub.
[ADR 0052](0052-copilot-policy-fetch-queries.md) fixed a query on the separate
managed-settings endpoint, without proving this registry-policy route.

Hosted Copilot E2E used OpenAI bring-your-own-key mode. The manual GitHub
process fixture represented an individual account, which could complete a
model/tool turn without requesting the registry policy. Its positive
managed-settings counter did not prove registry routing.

## Decision

For GitHub-backed Copilot, relay GET requests under both account namespaces,
including queries. Keep the fixed `api.github.com` origin, existing credential
mechanism, request/response bounds, no redirects and no-store responses.
Other methods, unrelated GitHub API paths and noncanonical paths are refused.
Other providers retain exact method/path rules. OpenAI-key Copilot does not
gain GitHub account routes.

GitHub continues to decide account permissions and MCP policy. Forward its
successes and refusals unchanged. The gateway does not invent a permissive
policy or log queries, credentials or policy bodies.

Docker and Kubernetes mount the same two prefixes from the provider table.
Tests use literal request paths independently of that table and verify that
the selected relay forwards the request and preserves the provider response.

PR/main CI and the latest-client canary run the GitHub-authenticated process
verifier with the relevant lockfile client. A separate explicit request to
`/copilot/mcp_registry` traverses the same authenticated TLS proxy and must
return the fixture policy. This probe is recorded separately from the stock
client's tool turn. It cannot be replaced by a model-completion result or a
counter for another endpoint. The fixture also rejects unmodeled account
requests so a newly exercised client path needs review.

## Alternatives and tradeoffs

Adding only one exact endpoint would fix this report but retain a fragile
inventory of Copilot's changing read-only account APIs. The administrator
approved both account namespaces. Allowing the whole GitHub API or arbitrary
write methods would cross that scope and is rejected.

The individual fixture does not reproduce an organization's live policy
selection. The explicit probe proves gateway routing and response fidelity;
the stock process proves the exercised signed-in model/MCP flow. Neither
claims a real subscription or every enterprise setting. Handler tests also
preserve upstream 403 and 404 responses.

## Verification and revisit conditions

Revisit if Copilot needs another host, a write method or a namespace outside
these two roots. Such a change needs a new boundary review. A real-account
failure after successful routing must be investigated separately from a
gateway refusal.

## Implementation

[Provider routes](../../internal/endpoints/model/providers.go),
[account relay](../../internal/endpoints/model/signin.go),
[stock process verifier](../../hack/verify/harness-process/main.go),
[CI checks](../../.github/workflows/test.yml).
