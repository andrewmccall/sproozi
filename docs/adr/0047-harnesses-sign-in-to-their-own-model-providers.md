# 0047: Every credential mechanism for every harness

Status: accepted
Date: 2026-10-22

## Problem and constraints

The gateway held every model provider credential: an OpenAI or Anthropic API
key, or one ChatGPT plan sign-in it refreshed itself
([ADR 0002](0002-native-cli-harnesses-and-model-protocols.md),
[ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md)). Each harness
received a placeholder key, and the gateway replaced it on every model call.

That couldn't use the subscriptions people already pay for, such as a Claude
or ChatGPT plan signed in from the stock client, or GitHub Copilot's own
sign-in. Gateway-held keys still matter: organizations use API keys under
their own agreements, and a key the sandbox never sees is the stronger
boundary. A first form of this decision removed gateway-held keys, and a
second made Claude Code a special case. Both were reversed the same day: the
person choosing a mechanism owns its provider terms, and per-harness code
would have to be repeated for each new harness.

## Decision

- **Three mechanisms, offered to every harness.** The gateway holds an API
  key; the gateway holds a sign-in (a stored token, or a ChatGPT plan sign-in
  it refreshes); or the harness signs in itself and the gateway forwards that
  credential. Each harness offers every mechanism its provider's API accepts.
- **Provider facts are data, not harness code.** One table in
  `internal/endpoints/model/providers.go` lists, for each provider, its model
  authorities (an API host, and for OpenAI the `chatgpt.com` backend a
  signed-in client calls), its sign-in relay routes, and the headers a
  gateway-held token needs. Each harness registry entry adds only where its
  stock client keeps its login file (`SignInFile`). The gateways build routes
  from these; nothing branches on the harness.
- **Compressed requests are decoded.** A model request with a gzip or zstd
  `Content-Encoding` is decoded before inspection and forwarded uncompressed,
  so every mechanism inspects the same JSON.
- **Forwarding keeps inspection.** For a harness that signs in itself, every
  model authority of its provider forwards the harness's credential headers
  (`modelauth.Workload`). Model routes keep TLS inspection, capability checks,
  refusal of hidden background work, audit, `diagnose` and token accounting,
  and never record the credential. A request with no credential is refused
  before budget is reserved. The provider's sign-in relay passes only its
  sign-in and refresh requests, unrecorded and uncharged.
- **The gateway refuses a forwarded credential elsewhere.** `LeakGuard` keeps
  each run's forwarded credentials in memory until the run's maximum duration
  has passed, and refuses any inspected request to another authority whose
  URL, headers or streamed body contain one.
- **Budgets keep counting tokens.** Reservations, settlement and the optional
  pricing table are unchanged for every mechanism.
- **MCP, GitHub and destination credentials stay on the gateway.**
  [ADR 0001](0001-mcp-capabilities-through-shared-gateway.md) is unchanged.

### How each mechanism is configured

| Mechanism | Local (`sproozi init`) | Kubernetes gateway | Kubernetes sandbox |
| --- | --- | --- | --- |
| Gateway-held API key | Default; `--credential-file`, `--credential-stdin` or the prompt | `openai-api-key`, `anthropic-api-key` | Placeholder key |
| Gateway-held sign-in token | `--token` with a key input; `providerTokenKind: token` | `openai-sign-in-token`, `anthropic-sign-in-token` | Placeholder key |
| Gateway-held ChatGPT plan sign-in | `--chatgpt`: browser sign-in, stored in `credentials/chatgpt-session.json` | `MODEL_AUTH_MODE=chatgpt` | Placeholder key |
| The harness's own sign-in | `--sign-in`: a persistent login volume per installation and placement | `MODEL_AUTH_MODE=workload` and no key or token for the provider | `clientConfig.signIn`: a Secret delivered as a file and, optionally, an environment variable; no placeholder |

A Kubernetes gateway chooses per provider: a key, then a token, then, in
workload mode, forwarding. One gateway can hold an OpenAI key while Claude Code
signs in itself.

### Where a harness's own sign-in lives

Locally, the agent container mounts one file of a persistent Docker volume,
`sproozi-<installation>-<placement>-login`, at
`/workspace/session/<native directory>/<SignInFile>` (`volume-subpath`, Docker
Engine 26 or later). The helper image's `checkpoint init-login`, run as root
with only `CAP_CHOWN` and no network, creates an empty mode-0600 file owned by
uid 1000 and leaves the volume's directory with root, so later runs can
inspect it. The harness writes and refreshes the file itself; Sproozi never
reads it. Checkpoints select only each harness's conversation, so a sign-in
never moves with a session.

On Kubernetes, `clientConfig.signIn` names a Secret or a PersistentVolumeClaim
in `sproozi-agents`. A Secret holds a credential the person obtained through
the provider's own flow, such as `claude setup-token`; the controller mounts it
read-only at `/etc/sproozi/sign-in/<key>` and, with `env`, sets it as that
variable. A claim is mounted read-write there. For a managed session the
supervisor copies the harness's `SignInFile` from that directory into its
native home before launch, and writes the refreshed file back after the
harness exits when the directory is writable. A Kubernetes pod can't mount the
file into the session folder directly, as Docker does: the folder is restored
after the pod starts, so a mount there would break the restore's ownership
checks.

## Evidence

All with deterministic providers unless stated:

- Claude Code 2.1.302: a linked `.credentials.json` is replaced by a regular
  file on refresh, so a link into a volume loses every refresh; a mounted
  file is written in place; an empty file reads as "Not logged in"; and every
  refresh returns a new refresh token.
- Codex 0.163.0, with a real ChatGPT sign-in through a recording proxy on the
  user's machine (method, path and status only; two account routing fields
  read once): model calls go to `chatgpt.com/backend-api/codex/responses`;
  Codex completes with only `codex/models`, `wham/accounts/check` and
  `wham/settings/user` allowed and plugins, ChatGPT's connector MCP
  (`ps/mcp`), analytics and usage refused; with WebSocket upgrades refused it
  falls back to HTTPS; it refreshes at `auth.openai.com/oauth/token` and
  writes a mounted `auth.json` in place; its terminal offers device-code
  sign-in. Account routing fields must be `NO_CONSTRAINT` in the fixture.
- `make verify-local-onboarding` passed both ways with the stock clients:
  gateway-held keys for Codex and Claude Code, and with `SIGN_IN=1` each
  harness's own seeded sign-in, every model request carrying it. That run
  found that a ChatGPT-signed-in Codex compresses its request bodies (zstd,
  its `enable_request_compression` feature), so the model handler decodes
  gzip or zstd before inspection, bounded like any body, and forwards plain
  JSON; another encoding is refused.

## Alternatives

- **Remove gateway-held keys.** One mechanism, but it drops the stronger
  boundary that organizations using API keys rely on.
- **Make Claude Code a special case.** Two credential models and code to
  repeat for each harness.
- **The gateway runs each provider's sign-in.** The person can store a
  provider-issued token instead; a gateway-run OAuth flow per provider would
  be provider code the gateway doesn't need.
- **Persist the whole harness home.** Simpler mounts, but it carries hooks,
  settings and instructions from one session into the next.
- **The first Kubernetes run writes its login into a ConfigMap.** Sandboxes
  have no Kubernetes credentials, by design, and ConfigMaps aren't for
  secrets.
- **Stop inspecting forwarded traffic.** It would lose refusal of hidden
  work, usage records and `diagnose`.

## Consequences and limits

- **A harness that signs in itself can read its own credential.** A misled
  agent could send it through any destination the gateway allows. The leak
  check sees only inspected traffic, and an encoded or split credential gets
  past it. A stolen sign-in works until the person revokes it.
- Whether a provider's terms allow a given mechanism is the choice of the
  person who configures it.
- On a forwarding route, workload headers choose the provider account. The
  acceptance rules "Provider credentials stay in the gateway" and "Workload
  headers cannot select another account" still hold for gateway-held
  credentials and for MCP, GitHub and destination routes.
- Two sessions sharing one refreshing sign-in may race, because refresh
  tokens rotate; a long-lived token avoids it.
- Codex tries WebSockets first and falls back after the gateway refuses each
  upgrade; see [WebSocket model traffic](../explanations/future-architecture/websocket-model-traffic.md).
- GitHub Copilot's own GitHub sign-in needs Copilot's model hosts and
  `api.github.com`, which the GitHub pull-request capability already serves;
  it isn't routed yet, so Copilot uses a key or token in bring-your-own-key
  mode (ADR 0046).
- On Kubernetes, a managed session's refreshed login is saved only when the
  harness exits; a supervisor killed without that loses the refresh, and the
  person signs in again.
- Extends ADR 0002 and leaves ADR 0026 in place.

Revisit if a provider offers scoped, short-lived tokens a harness can request
per run, if the leak check proves too noisy or too weak, or when Copilot gains
its own sign-in.

## Implementation

[Provider table](../../internal/endpoints/model/providers.go),
[ChatGPT backend](../../internal/endpoints/model/chatgpt_backend.go),
[sign-in relay](../../internal/endpoints/model/signin.go),
[leak guard](../../internal/gateway/leak.go),
[credential modes](../../internal/modelauth/auth.go),
[local routes](../../internal/gateway/docker/gateway.go),
[login volume](../../internal/runtime/docker/managed_engine.go),
[Kubernetes sign-in delivery](../../internal/runtime/kubernetes/sandbox.go).
