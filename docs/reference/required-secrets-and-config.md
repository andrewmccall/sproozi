# Required secrets and config

This page lists credentials and configuration for the Kubernetes deployment.
Provider credentials stay in trusted cluster objects. Agent Pods receive client
configuration, public trust material and a gateway identity token for the run.
Follow [Install on Kubernetes and run your first Pod](../guides/install-kubernetes.md) for setup steps.

## Objects

| Name | Namespace | Kind | Used by | Required keys |
| --- | --- | --- | --- | --- |
| `model-gateway-credentials` | `sproozi-system` | `Secret` | model module in API-key mode | `openai-api-key`, `anthropic-api-key` or both, each a single line |
| `model-gateway-chatgpt` | `sproozi-system` | `Secret` | model module in ChatGPT mode | `session.json` |
| `model-gateway-auth` | `sproozi-system` | `ConfigMap` | administrator auth selection (optional; default API-key mode) | `mode` |
| `sproozi-model-gateway-pricing` | `sproozi-system` | `ConfigMap` | shared gateway model module | `pricing.json` |
| `capability-proxy-github-app` | `sproozi-system` | `Secret` | shared gateway GitHub module | `app-id`, `installation-id`, `private-key.pem` (PKCS#1 or PKCS#8 RSA PEM) |
| `sproozi-egress-gateway-profiles` | `sproozi-system` | `ConfigMap` | shared gateway egress module | `profiles.json` |
| `sproozi-gateway-tls` | `sproozi-system` | `Secret` | shared gateway inspection certificate | `tls.crt`, `tls.key` |
| `sproozi-sandbox-trust` | `sproozi-agents` | immutable `ConfigMap` | sandbox clients | `ca-bundle.pem`, public CA only |
| `<template secret>` | `sproozi-webhooks` | `Secret` | webhook | `hmac-key`, at least 32 random bytes |

The install adds the `sproozi-` prefix to every object it creates, so the
ConfigMaps it ships are `sproozi-model-gateway-pricing`,
`sproozi-egress-gateway-profiles` and `sproozi-mcp-servers`. Objects the
administrator creates, such as `model-gateway-credentials` and
`capability-proxy-github-app`, keep the names in this table.

The shared gateway deployment mounts `sproozi-gateway-tls` and requires
`SPROOZI_GATEWAY_CERT_FILE` and `SPROOZI_GATEWAY_KEY_FILE` to point at the
mounted files. Inspection authorities come from enabled semantic modules and
egress profiles. Startup checks that the certificate covers every host.

Two optional environment variables set the gateway connection limits, a third
sizes its Go memory limit and a fourth bounds each model call. Each must be a whole number in its range, or
the gateway exits at startup with a message naming the variable. Because the
gateway Deployment uses the `Recreate` strategy, it stays down until you
correct the value:

| Variable | Default | Range | Limits |
| --- | --- | --- | --- |
| `SPROOZI_GATEWAY_MAX_CONNECTIONS` | 2048 | 1–4096 | TCP connections, CONNECT requests in progress and pending authority lookups |
| `SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION` | 16, or the connection limit if lower; the install leaves it unset | 1–128, and no more than the connection limit | Inspected connections held by one execution, so one run can't take the whole pool. The name says sessions, but it counts inspected connections, not user sessions |
| `SPROOZI_GATEWAY_MEMORY_LIMIT` | The container's `limits.memory`, filled by the install | At least 67108864 bytes (64Mi), checked even when `GOMEMLIMIT` is set | The gateway sets its Go memory limit to 90% of this value. An explicit `GOMEMLIMIT` takes precedence; with neither set, Go's own default applies |
| `SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` | 1200 (20 minutes); the install leaves it unset | 1–3600 | The [model call deadline](../../GLOSSARY.md#model-call-deadline): how long one model call may stream, counted from when the gateway receives it. `AgentPolicy.spec.maxModelCallSeconds` can lower it for a policy's runs but not raise it. See [long model calls](../guides/model-provider-auth.md#long-model-calls) |

At startup the gateway logs the Go memory limit in effect and where it came
from: `Go memory limit is N bytes, from 90% of SPROOZI_GATEWAY_MEMORY_LIMIT=...`,
`... from GOMEMLIMIT=...`, or `Go memory limit is unlimited, from ...`.
An explicit `GOMEMLIMIT` must be a valid Go value: a byte count with an
optional `B`, `KiB`, `MiB`, `GiB` or `TiB` suffix, or `off`. Any other value
makes the Go runtime abort at startup, before Sproozi code runs, with
``fatal error: malformed GOMEMLIMIT; see `go doc runtime/debug.SetMemoryLimit` ``.

An invalid value names the variable and the range, for example
`SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS="x" must be an integer from 1 to 3600:
check the gateway Deployment environment`.

If the per-execution limit is larger than the connection limit, the gateway
exits with `SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION (N) must not exceed
SPROOZI_GATEWAY_MAX_CONNECTIONS (M): check the gateway Deployment environment`.

A streaming inspected connection measured about 150–160 KiB of heap and stack
across runs. The install budgets 256 KiB for each and allows one connection
per MiB of the gateway's memory limit, so the default 2048 connections take a
quarter of the 2Gi limit. The rest is headroom for model bodies, the gateway's
own heap and the garbage collector. Each model request in flight holds up to
about 2 MiB of request and response body data outside the connection budget,
plus allocation overhead at peak. Lower the connection limit together with the memory limit; see
[concurrency and gateway capacity](security-hardening.md#concurrency-and-gateway-capacity).

After startup validation passes and the TLS listener on 8443 is bound, the
gateway opens a plain HTTP health port, 8081, for its readiness probe. It
serves only `GET /readyz`, which returns `ok`. A gateway with invalid
configuration exits and never becomes ready.

`SPROOZI_ENABLED_CAPABILITIES` selects installed modules as a comma-separated
list. The binary defaults to `kubernetes.read`; the demo Deployment explicitly
enables all implemented capabilities. Disabled model and GitHub modules need no
provider credentials. Enabling a module does not grant access. The run and
current policy must still permit every request.

The shared gateway writes newline-delimited, secret-free JSON audit events to
container stdout. Operators should collect that stream from the shared gateway
deployment; semantic gateway decisions are not silently discarded.

`MODEL_AUTH_MODE` selects `api_key` (default) or `chatgpt`. Only the selected
mode's credential Secret is required. ChatGPT credentials are created through the
local consent helper, not by copying a Codex login cache. See
[Model provider authentication](../guides/model-provider-auth.md) for the Kind
setup, token-only subscription budget and single-refresh-owner limitation. In
ChatGPT mode the gateway reads the ChatGPT credential Secret again at most every 10
seconds, so an administrator's replacement or removal of the Secret reaches
every model request that authorizes 10 seconds or more after it, without a
restart.

## Controller flags

| Flag | Default | Effect |
| --- | --- | --- |
| `--max-active-runs` | `0` (no limit) | The most AgentRuns that can be `Admitted` or `Running` at once across the cluster. A negative value stops the controller at startup. |

The default install leaves `--max-active-runs` unset. Per-policy caps use
[`AgentPolicy.spec.maxActiveRuns`](agentpolicy.md#concurrent-runs).
[Limit concurrent runs](../guides/install-kubernetes.md#limit-concurrent-runs)
shows how to set the flag.

## Egress profile format

`profiles.json` maps a profile name to exact inspected HTTPS combinations of
scheme, host and port. Ports are exact and may differ from 443; plaintext HTTP
is rejected at startup. Adding a profile creates its route, but the template and
live policy must both select it. The TLS certificate must include each profile
host:


```json
{
  "go-modules": [
    { "scheme": "https", "host": "proxy.golang.org", "port": 443 },
    { "scheme": "https", "host": "sum.golang.org", "port": 443 }
  ]
}
```

## Local vs. home-ops usage

- the local demo creates Secrets from gitignored `.local/demo.env`; no credentials are checked into the example bundle
- the `home-ops` path should keep the Secrets in **SOPS-managed GitOps manifests**

That static-secret guidance does not apply to the rotating ChatGPT credential:
do not continuously reconcile its live Secret from a stale bootstrap file or
SOPS manifest. The trusted gateway owns token replacements after deployment.

Never mount these credentials directly into sandbox workloads.

## Model pricing format

When `model.inference` is enabled, the shared gateway loads `MODEL_PRICING_PATH` at startup (default
`/etc/sproozi/model/pricing.json`). Startup fails closed if the file is absent,
malformed, empty, or contains non-positive prices. Model inference under a
positive `budgets[model.inference].maxCostMicros` requires an exact model row with
positive input, cached-input and output rates before OpenAI API-key or Anthropic
inference. OpenAI selects the standard `default` service tier and refuses explicit
nondefault tiers under that cap. Token-only API-key requests and model discovery
do not require a price row. Unknown consumed categories can still fail settlement
after provider work. This table does not price all non-token provider charges or
establish a hard spending ceiling.

Prices are integer millionths of a US dollar per million tokens:

```json
{
  "models": {
    "gpt-6.1-sol": {
      "inputMicrosPerMillionTokens": 2000000,
      "cachedInputMicrosPerMillionTokens": 100000,
      "outputMicrosPerMillionTokens": 10000000
    }
  }
}
```

The example uses illustrative administrator-supplied rates. The installed table
prices `gpt-6.1-sol`, `gpt-5.3-codex` and the Claude Code example model
`claude-sonnet-4-5-20250929`. The Anthropic row uses Anthropic's published list
price for Claude Sonnet 4.5 (USD 3 input and USD 15 output per million tokens,
cache reads at 0.1 times and five-minute cache writes at 1.25 times the input
rate). Review the checked-in table against provider pricing when selecting a model. It does not model every
billing tier or surcharge. ChatGPT mode uses token-only budgets rather than API
dollar prices. Model admission can underestimate total consumption; see
[security hardening](security-hardening.md).

## State and deployment controls

The deployed budget ledger writes ConfigMaps in `sproozi-system`, selected by
`BUDGET_NAMESPACE` and labelled `app.kubernetes.io/component=budget-ledger`.
The gateway's `shared-gateway-budget` Role grants `get`, `list`, `create` and
`update` on ConfigMaps there; it lists them to settle expired
[leases](../../GLOSSARY.md#lease) every minute. If you change the variable,
create that Role and its RoleBinding in the new namespace. The webhook writes replay ConfigMaps in
`sproozi-webhook-state`, selected by `REPLAY_NAMESPACE`. The install includes
the webhook state namespace and namespaced state RBAC; `retentionTTL` does not
clean this durable state.

The webhook reads HMAC Secrets only in `sproozi-webhooks`, selected by
`HMAC_SECRET_NAMESPACE`, never in the template's namespace. The install
creates that namespace and a Role there that grants only Secret `get`. If you
change the variable, create the Role and RoleBinding in the new namespace.
See [ADR 0022](../adr/0022-webhook-hmac-secrets-in-own-namespace.md).

Transport, GitHub protection, replay retention and trusted-service restrictions
are documented in [security hardening](security-hardening.md).

## Remote MCP registration

The gateway mounts the `sproozi-mcp-servers` ConfigMap and reads its `servers.json` through
`MCP_SERVERS_PATH`. An empty registry enables no remote providers. Provider tokens
are optional Secret files mounted under `/var/run/secrets/sproozi/mcp` only in the
gateway; registry entries reference
their paths with `bearerTokenFile`. Restart after changing either registrations or
tokens. See [configured MCP capabilities](configured-mcp.md) for the format,
policy predicates and client preparation.


## Optional persistent orchestration

[Run a persistent Hermes assistant on Kubernetes](../guides/hermes-orchestration.md) prepares the private task
service and trusted coordinator. These credentials stay outside agent Pods:

| Object | Namespace | Purpose |
| --- | --- | --- |
| `sproozi-task-token` Secret | `sproozi-system` | Task service bearer token, `token` key |
| `sproozi-task-tls` Secret | `sproozi-system` | Task server TLS certificate and key |
| `sproozi-tasks` immutable ConfigMap | `sproozi-system` | Fixed principal and workflow map, `tasks.json` |
| `hermes-coordinator` Secret | `sproozi-coordinators` | Separate model key, native API key and the `task-token` key: the task service token, or the remote executor's work token in [Hermes with remote work](../guides/retained-work-kubernetes.md) |
| `hermes-task-trust` ConfigMap | `sproozi-coordinators` | Normal roots plus public task-service CA, `ca-bundle.pem` |
| `hermes-config` immutable ConfigMap | `sproozi-coordinators` | Reviewed native model, platform and MCP configuration |

Changing service credentials or native environment configuration requires
restarting the affected Deployment. The stock coordinator keeps writable native
state in its PVC; the task service keeps no separate persistent ledger.
