# 0047: Harnesses sign in to their own model providers

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

The claim that "the sandbox never holds a provider credential" was also never
the whole protection: a misled agent can spend its run's budget either way, and
MCP and GitHub credentials stay on the gateway regardless.

## Decision

- **Every harness signs in itself.** Each uses its native login: a subscription
  sign-in, a long-lived token from the provider's own flow, or the person's own
  API key. Sproozi never runs a provider sign-in, never reads a native login
  store, and never copies a credential between placements.
- **The gateway passes the harness's credential through.** A new
  `modelauth` authenticator forwards the workload's own credential headers
  instead of replacing them. Model routes keep TLS inspection, capability
  checks, refusal of hidden background work, audit, `diagnose` and token
  accounting. Audit and diagnostics never record the credential.
- **The gateway holds no model provider credentials.** API-key mode, ChatGPT
  mode, the `model-gateway-credentials` and `model-gateway-chatgpt` Secrets,
  the refresh owner and `sproozi credentials` are removed.
- **MCP, GitHub and destination credentials stay on the gateway.**
  [ADR 0001](0001-mcp-capabilities-through-shared-gateway.md) is unchanged.
- **Budgets keep counting tokens.** Reservations, settlement and the optional
  pricing table stay as they are. A monetary cap only means something for a
  metered API key.
- **The gateway refuses a leaked credential.** It keeps each run's observed
  provider credentials in memory for the run's lifetime only, and refuses any
  request to a non-provider route it inspects whose headers or body contain
  one. It never persists them.

### Where credentials live

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

- **Keep gateway-held credentials and add Claude as an exception.** Two
  credential models, and Claude's would be the weaker one anyway.
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

- **The agent can read its own provider credential.** A misled agent could send
  it out through any destination the gateway allows. The leak check covers
  only routes the gateway inspects, not opaque egress tunnels, and a token
  that is encoded or split can still get past it.
- A stolen subscription token works until it is revoked, and a Claude
  `setup-token` lasts a year. Revoke it from the provider's settings.
- Workload headers now choose the provider account. This replaces the
  acceptance rules "Provider credentials stay in the gateway" and "Workload
  headers cannot select another account" for model routes; they still hold
  for MCP, GitHub and destination routes.
- Two sessions that share one refreshing login may race if the provider rotates
  refresh tokens. That needs testing per harness. The fallback is one session
  at a time or a long-lived token.
- Every moved session signs in again at its new placement.
- **Open question.** Anthropic forbids third parties to "intermediate"
  session tokens. A TLS-inspecting proxy that forwards the token unchanged and
  holds it in memory for the leak check is ordinary network handling, as with
  a corporate proxy, but the wording doesn't say so. Check before relying on it
  for Claude subscriptions.
- Supersedes the credential parts of ADR 0002 and all of ADR 0026, once
  implemented.

Revisit if a provider offers scoped, short-lived tokens that a harness can
request per run, if the leak check proves too noisy or too weak, or if a
provider's terms forbid an inspecting proxy.

## Implementation

Slice 1 (pass-through and leak check): `modelauth.Workload` and
`MODEL_AUTH_MODE=workload` in the Kubernetes gateway, which enables both model
routes without a gateway key and refuses a request with no credential before
reserving budget; `gateway.LeakGuard`, which wraps each gateway's Dispatcher,
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
it stripped it everywhere. `sproozi init` stores no key for Claude Code, and a
second run drops an existing one from the configuration.

Found while building it, with Claude Code 2.1.302: Claude replaces a linked
`.credentials.json` with a regular file when it refreshes, so a link into a
volume loses every refresh; it writes in place when the file itself is a mount;
an empty file reads as "Not logged in"; and each refresh returns a new refresh
token, so concurrent sessions sharing one sign-in can invalidate each other.
`make verify-local-onboarding` seeds the volume with a fixture sign-in and
checks that every Claude request to the fixture provider carried it.

Remaining slices: the pass-through authenticator and leak check; Claude Code
in local Docker with a credential volume; Codex and Copilot; Kubernetes
Secrets and PersistentVolumeClaims; then removing gateway-held credentials,
`sproozi credentials` and their docs. Each slice updates this record if the
implementation differs.
