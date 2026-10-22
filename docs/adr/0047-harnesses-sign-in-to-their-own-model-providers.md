# 0047: Harnesses can sign in to their own model providers

Status: accepted
Date: 2026-10-22

## Problem and constraints

The gateway holds every model provider credential: an OpenAI or Anthropic API
key, or one ChatGPT plan sign-in it refreshes itself
([ADR 0002](0002-native-cli-harnesses-and-model-protocols.md),
[ADR 0026](0026-single-gateway-refresh-owner-with-recreate.md)). Each harness
receives a placeholder key, and the gateway replaces it on every model call.

That model can't use the subscriptions people already pay for:

- Anthropic's terms say a third party "may not collect, store, or intermediate
  Claude.ai credentials or session tokens", and sign-in must complete through
  Anthropic's own flow. They allow signing in to the unmodified Claude Code,
  including where a platform hosts it
  ([legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)).
- GitHub Copilot's subscription works only through a GitHub sign-in that the
  stock client makes itself.
- Making Claude the one exception would give the gateway two credential models.

Gateway-held keys still matter. Organizations use Anthropic and OpenAI API
keys under their own agreements, and a key the sandbox never sees is the
stronger boundary. So both must remain available.

## Decision

- **Two credential owners, chosen per harness or provider.** The gateway holds
  a key, as before, or the harness signs in itself with its native login: a
  subscription sign-in, a long-lived token from the provider's own flow, or the
  person's own API key. Locally the choice is per harness (`sproozi init
  --sign-in`); on Kubernetes, `MODEL_AUTH_MODE=workload` makes each provider
  without a gateway key forward the harness's sign-in, so one gateway can hold
  an OpenAI key while Claude Code signs in. Sproozi never runs a provider
  sign-in, never reads a native login store, and never copies a sign-in
  between placements.
- **For a harness that signs in, the gateway passes its credential through.** A new
  `modelauth` authenticator forwards the workload's own credential headers
  instead of replacing them. Model routes keep TLS inspection, capability
  checks, refusal of hidden background work, audit, `diagnose` and token
  accounting. Audit and diagnostics never record the credential.
- **MCP, GitHub and destination credentials stay on the gateway.**
  [ADR 0001](0001-mcp-capabilities-through-shared-gateway.md) is unchanged.
- **Budgets keep counting tokens.** Reservations, settlement and the optional
  pricing table stay as they are. A monetary cap only means something for a
  metered API key.
- **The gateway refuses a leaked credential.** It keeps each run's observed
  provider credentials in memory for the run's lifetime only, and refuses any
  request to a non-provider route it inspects whose headers or body contain
  one. It never persists them.

### Three ways to authenticate

Each harness offers every mode its provider allows:

| Harness and provider | Gateway-held sign-in | Gateway-held API key | Harness's own sign-in |
| --- | --- | --- | --- |
| Codex, OpenAI | ChatGPT plan, `MODEL_AUTH_MODE=chatgpt` (Kubernetes) | Yes | ChatGPT sign-in in Codex |
| OpenCode and Hermes, OpenAI | ChatGPT mode's Responses route, not yet established for these clients | Yes | Their own key in the sandbox |
| Claude Code, Anthropic | Not offered: Anthropic forbids a third party storing or intermediating a Claude sign-in | Yes | `/login` or `claude setup-token` |
| Copilot, OpenAI or Anthropic models | Through the OpenAI route's ChatGPT mode | Yes, in bring-your-own-key mode (ADR 0046) | GitHub sign-in in Copilot |

A gateway-held GitHub Copilot sign-in would need a GitHub model route with an
undocumented wire, so it isn't offered.

### Where a harness's own sign-in lives

| Placement | Interactive managed session | Non-interactive run |
| --- | --- | --- |
| Local Docker | A per-harness credential volume, mounted only at the harness's credential path and reused by that harness's later sessions. The harness writes and refreshes it. | Not applicable |
| Kubernetes | A per-user PersistentVolumeClaim used the same way | A Secret the person creates from a token they generated through the provider's own flow, which the AgentRuntime mounts into the sandbox |

Only the credential file is persisted, never the harness's whole home: a shared
home would let one session plant hooks, settings or instructions that run in
the next. Checkpoints exclude the credential volume, so a moved session signs in
again at its new placement. The controller mounts these objects; it never reads
them.

### Per-harness sign-in

| Harness | Interactive sign-in and file | Non-interactive credential | Status |
| --- | --- | --- | --- |
| Claude Code | `/login`, `$CLAUDE_CONFIG_DIR/.credentials.json` (mode 0600), refreshed by Claude Code | `claude setup-token`, a one-year token in `CLAUDE_CODE_OAUTH_TOKEN` | From Anthropic's [authentication docs](https://code.claude.com/docs/en/authentication) |
| Codex | ChatGPT sign-in, `$CODEX_HOME/auth.json` (mode 0600; ID, access and refresh tokens) | `codex login --with-api-key` or `--with-access-token` | Observed with 0.163.0: a ChatGPT-signed-in `codex exec` connects only to `chatgpt.com`, `ab.chatgpt.com` and `github.com`, not `api.openai.com`, so it needs a `chatgpt.com` model route |
| Copilot | GitHub sign-in | `COPILOT_GITHUB_TOKEN` | Environment variable from `copilot help environment`; storage and the model wire are to verify |
| OpenCode | `opencode auth login` | Provider API key | To verify |
| Hermes | Its own provider configuration | Provider API key | To verify |

Each harness's model hosts become model routes. Copilot needs new ones:
`api.github.com` and `api.individual.githubcopilot.com` were observed, and
Business and Enterprise plans probably use other hosts.

## Alternatives

- **Remove gateway-held keys and have every harness sign in.** One credential
  model, but it would drop the stronger boundary that organizations using API
  keys rely on, and it was this decision's first form. Reversed the same day.
- **Keep gateway-held credentials and add Claude as an exception.** Two
  credential models with no rule for the next harness.
- **The gateway stores subscription tokens and injects them.** Anthropic's
  terms forbid it, and it repeats ADR 0026's refresh-owner problem for every
  provider.
- **Persist the whole harness home.** Simpler mounts, but it carries hooks,
  settings and instructions from one session into the next.
- **The first Kubernetes run writes its login into a ConfigMap.** Sandboxes
  have no Kubernetes credentials, by design. Having the controller copy the
  login into a Secret instead would make Sproozi the store the terms forbid,
  and ConfigMaps aren't for secrets.
- **Count only requests.** Simpler, but one request can cost a cent or several
  dollars on a metered key. Token accounting already exists.
- **Stop inspecting model traffic.** It would lose refusal of hidden work,
  usage records and `diagnose`, for little saving.

## Consequences and limits

- **A harness that signs in can read its own provider credential.** A misled agent could send
  it out through any destination the gateway allows. The leak check covers
  only routes the gateway inspects, not opaque egress tunnels, and a token
  that is encoded or split can still get past it.
- A stolen subscription token works until it is revoked, and a Claude
  `setup-token` lasts a year. Revoke it from the provider's settings.
- On a provider route that forwards the harness's sign-in, workload headers
  choose the provider account. The acceptance rules "Provider credentials stay
  in the gateway" and "Workload headers cannot select another account" still
  hold for gateway-held keys and for MCP, GitHub and destination routes.
- Two sessions that share one refreshing login may race if the provider rotates
  refresh tokens. That needs testing per harness. The fallback is one session
  at a time or a long-lived token.
- Every moved session signs in again at its new placement.
- **Open question.** Anthropic forbids third parties to "intermediate"
  session tokens. A TLS-inspecting proxy that forwards the token unchanged and
  holds it in memory for the leak check is ordinary network handling, as with
  a corporate proxy, but the wording doesn't say so. Check before relying on it
  for Claude subscriptions.
- Extends ADR 0002 and leaves ADR 0026 in place: gateway-held API keys and
  ChatGPT mode remain for harnesses that don't sign in.

Revisit if a provider offers scoped, short-lived tokens that a harness can
request per run, if the leak check proves too noisy or too weak, or if a
provider's terms forbid an inspecting proxy.

## Implementation

Slice 1 (pass-through and leak check): `modelauth.Workload` and
`MODEL_AUTH_MODE=workload` in the Kubernetes gateway, which enables both model
routes, forwards the harness's credential on each provider without a gateway
key, and refuses a request with no credential before reserving budget; `gateway.LeakGuard`, which wraps each gateway's Dispatcher,
records credentials on routes marked `ProviderCredential`, and refuses them in
the URL, headers or streamed body of every other route. Credentials shorter than
16 characters aren't watched. The local Docker gateway is wrapped too, but
still uses its stored keys until slice 2.

Slice 2 (Claude Code in local Docker): a placement whose harness has no
`providerTokenFile` sets `SignIn`. Its agent mounts one file of a persistent
volume, `sproozi-<installation>-<placement>-login`, at
`/workspace/session/claude/.credentials.json` (`volume-subpath`, Docker Engine
26 or later), and its launch has no placeholder key, which Claude Code would
otherwise prefer. The helper image's `checkpoint init-login`, run as root with
only `CAP_CHOWN` and no network, creates the empty mode-0600 file for uid 1000
and leaves the volume's directory with root, so later runs can inspect it.
Checkpoints already select only Claude's transcript. The local gateway runs
`modelauth.Workload`, relays `POST /v1/oauth/token` on `platform.claude.com`,
and covers that host. The proxy transport keeps a client's `Authorization`
header only on a route marked `ProviderCredential`; slice 1 had missed that
it stripped it everywhere. `sproozi init --sign-in` stores no key for Claude
Code; a later run with a key switches it back, and a run with neither keeps
its choice.

Found while building it, with Claude Code 2.1.302: Claude replaces a linked
`.credentials.json` with a regular file when it refreshes, so a link into a
volume loses every refresh; it writes in place when the file itself is a mount;
an empty file reads as "Not logged in"; and each refresh returns a new refresh
token, so concurrent sessions sharing one sign-in can invalidate each other.
`make verify-local-onboarding` seeds the volume with a fixture sign-in and
checks that every Claude request to the fixture provider carried it.

Remaining slices: Codex and Copilot; Kubernetes Secrets and
PersistentVolumeClaims.
