# Model provider authentication

The trusted gateway supports two administrator-selected authentication modes.
`MODEL_AUTH_MODE=api_key` is the default and uses `OPENAI_API_KEY`.
`MODEL_AUTH_MODE=chatgpt` uses an independently authorized ChatGPT-plan OAuth
session. It does not read Codex's login cache and does not fall back to an API
key if OAuth fails.

This is separate from workload authentication. AgentRuns still authenticate to
the shared gateway with their projected `sproozi-gateway` ServiceAccount token.
The sandbox receives neither a provider API key nor OAuth credentials. The
stock Codex client keeps its OpenAI-compatible provider configuration and its
harmless API-key placeholder in either mode.

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
**gpt-6.1-sol**. The helper reuses an existing, unexpired private session without
repeating browser consent. It checks the account's model catalog first. If the
exact model is absent, it sends one tiny inference using low reasoning effort;
this consumes plan tokens and must return `response.completed` for that exact
model. Catalog visibility can lag actual access. There is no model fallback.
If verification fails, the login remains saved but the demo mode is not switched.

To deliberately renew browser consent (including before bootstrapping another
cluster after the live gateway has rotated its credentials), run:

```bash
bash hack/demo/chatgpt-login.sh --reauthorize
```

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

Each renewable session has **one active gateway process**. The supplied
Deployment is one replica with `Recreate`, so updates do not overlap refresh
owners. Do not scale it, share its session with another cluster, or run another
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

ChatGPT mode uses the public `https://api.openai.com/v1/responses` endpoint,
not a ChatGPT backend endpoint. Inference requires `store:false`, `stream:true`
and an input array. Unsupported request fields are rejected, not silently
removed. Chat completions are unavailable in this mode; model discovery is
allowed. Account-selection headers supplied by the workload are discarded.
See the provider's
[inference contract](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)
and [preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations).

Both modes retain capability checks, live policy, token admission, completed
response/usage verification, accounting and audit. Audit events include the
non-secret `providerAuthMode`. ChatGPT mode requires a token-only policy:
the demo renderer sets `spec.budgets["model.inference"]` to
`maxCostMicros: 0` and `maxUnits: 300000`. Stock Codex
resends its instructions and conversation on each turn; the original 100000
token demo allowance was exhausted before creating a PR. API-key mode retains
100000 tokens and its 500000-microdollar cap.
A nonzero API-dollar cap is rejected before inference because API pricing is
not the cost of subscription usage. This is not a provider-side output-token
ceiling: the preview does not accept `max_output_tokens`, and an unexpectedly
large response can consume tokens before the gateway verifies usage.

To switch back to API-key billing, select `api_key` in
`bash hack/demo/configure.sh`. API-key mode retains existing token and dollar
budget behavior. Authentication mode is not selectable by an AgentRun.

## Verification status

Local tests cover the browser callback/code exchange against a mock provider,
PKCE, identity validation, account preservation, private storage, serialized
rotation, persistence retry, stale-writer rejection, both gateway auth modes,
Responses usage settlement and rendered RBAC/policy configuration.

These tests alone are not a ChatGPT-backed acceptance result. The
[recorded 4 October acceptance](../demos/verified-sre-demo.md) verified two
real inference-to-PR runs, explicit denials and scoped cleanup using the
stock CLI and bounded REST PR creation. Those are isolated demo results,
not general provider/client compatibility or production readiness. This narrow local demo
does not implement a multi-account UI, sign-out/revocation command or
multi-replica session coordination. Access can be disconnected in ChatGPT
Settings.
