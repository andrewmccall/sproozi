# Required secrets and config

**Audience:** operators

Sproozi keeps credentials and destination policy in trusted cluster objects, not
in sandbox pods. The sandbox receives only standard client configuration and a
per-run gateway identity token.

## Objects

| Name | Namespace | Kind | Used by | Required keys |
| --- | --- | --- | --- | --- |
| `model-gateway-credentials` | `sproozi-system` | `Secret` | model module in API-key mode | `openai-api-key` |
| `model-gateway-chatgpt` | `sproozi-system` | `Secret` | model module in ChatGPT mode | `session.json` |
| `model-gateway-auth` | `sproozi-system` | `ConfigMap` | administrator auth selection (optional; default API-key mode) | `mode` |
| `model-gateway-pricing` | `sproozi-system` | `ConfigMap` | shared gateway model module | `pricing.json` |
| `capability-proxy-github-app` | `sproozi-system` | `Secret` | shared gateway GitHub module | `app-id`, `installation-id`, `private-key.pem` |
| `egress-gateway-profiles` | `sproozi-system` | `ConfigMap` | shared gateway egress module | `profiles.json` |
| `sproozi-gateway-tls` | `sproozi-system` | `Secret` | shared gateway inspection certificate | `tls.crt`, `tls.key` |
| `sproozi-sandbox-trust` | `sproozi-agents` | immutable `ConfigMap` | sandbox clients | `ca-bundle.pem`, public CA only |
| `<template secret>` | template namespace | `Secret` | webhook | `hmac-key` |

The shared gateway deployment mounts `sproozi-gateway-tls` and requires
`SPROOZI_GATEWAY_CERT_FILE` and `SPROOZI_GATEWAY_KEY_FILE` to point at the
mounted files. Inspection authorities come from enabled semantic modules and
egress profiles. Startup checks that the certificate covers every host.

`SPROOZI_ENABLED_CAPABILITIES` selects installed modules as a comma-separated
list. The binary defaults to `kubernetes.read`; the demo Deployment explicitly
enables all implemented capabilities. Disabled model and GitHub modules need no
provider credentials. Module availability never grants a run authority: the run
and live policy must still permit every request.

The shared gateway writes newline-delimited, secret-free JSON audit events to
container stdout. Operators should collect that stream from the shared gateway
deployment; semantic gateway decisions are not silently discarded.

`MODEL_AUTH_MODE` selects `api_key` (default) or `chatgpt`. Only the selected
mode's credential Secret is required. ChatGPT sessions are created through the
local consent helper, not by copying a Codex login cache. See
[model provider authentication](../guides/model-provider-auth.md) for the Kind
setup, token-only subscription budget and single-refresh-owner limitation.

## Egress profile format

`profiles.json` maps a profile name to exact inspected HTTPS destination tuples.
Ports are exact and may differ from 443; plaintext HTTP is rejected at startup.
Adding a profile creates its route, but the template and live policy must both
select it. The TLS certificate must include each profile host:


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

That static-secret guidance does not apply to the rotating ChatGPT session:
do not continuously reconcile its live Secret from a stale bootstrap file or
SOPS manifest. The trusted gateway owns token replacements after deployment.

Never mount these credentials directly into sandbox workloads.

## Model pricing format

When `model.inference` is enabled, the shared gateway loads `MODEL_PRICING_PATH` at startup (default
`/etc/sproozi/model/pricing.json`). Startup fails closed if the file is absent,
malformed, empty, or contains non-positive prices. Model inference under a
positive `budgets[model.inference].maxCostMicros` rejects a successful response
for an unpriced model during settlement, after the upstream call. A missing price
does not prevent provider consumption.

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

The example uses illustrative administrator-supplied rates. Review the checked-in
table against provider pricing when selecting a model. It does not model every
billing tier or surcharge. ChatGPT mode uses token-only budgets rather than API
dollar prices. Model admission can underestimate total consumption; see
[security hardening](security-hardening.md).

## State and deployment controls

The deployed budget ledger writes ConfigMaps in `sproozi-system`, selected by
`BUDGET_NAMESPACE`. The webhook writes replay ConfigMaps in
`sproozi-webhook-state`, selected by `REPLAY_NAMESPACE`. The install includes
the webhook state namespace and namespaced state RBAC; `retentionTTL` does not
clean this durable state.

Transport, GitHub protection, replay retention and trusted-service restrictions
are documented in [security hardening](security-hardening.md).

## Remote MCP registration

The gateway mounts the `mcp-servers` ConfigMap and reads its `servers.json` through
`MCP_SERVERS_PATH`. An empty registry enables no remote providers. Provider tokens
are optional Secret files mounted under `/var/run/secrets/sproozi/mcp` only in the
gateway; registry entries reference
their paths with `bearerTokenFile`. Restart after changing either registrations or
tokens. See [configured MCP capabilities](configured-mcp.md) for the format,
policy predicates and client preparation.


## Optional persistent orchestration

The [Hermes guide](../guides/hermes-orchestration.md) prepares the private task
service and trusted coordinator. These credentials stay outside worker Pods:

| Object | Namespace | Purpose |
| --- | --- | --- |
| `sproozi-task-token` Secret | `sproozi-system` | Task service bearer token, `token` key |
| `sproozi-task-tls` Secret | `sproozi-system` | Task server TLS certificate and key |
| `sproozi-tasks` immutable ConfigMap | `sproozi-system` | Fixed principal and workflow map, `tasks.json` |
| `hermes-coordinator` Secret | `sproozi-coordinators` | Separate model key, task token and native API key |
| `hermes-task-trust` ConfigMap | `sproozi-coordinators` | Normal roots plus public task-service CA, `ca-bundle.pem` |
| `hermes-config` immutable ConfigMap | `sproozi-coordinators` | Reviewed native model, platform and MCP configuration |

Changing service credentials or native environment configuration requires
restarting the affected Deployment. The stock coordinator keeps writable native
state in its PVC; the task service keeps no separate persistent ledger.
