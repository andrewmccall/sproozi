# AgentPolicy

`AgentPolicy` sets the permissions and limits for an `AgentRun`. It defines
allowed capabilities, resources, usage and execution duration. The gateway
checks current policy on service requests, so policy changes can affect active
runs.

## What it defines

- allowed capability kinds
- Kubernetes namespace and resource scope
- GitHub repository scope
- named MCP server tools and additional argument restrictions
- named egress profiles
- optional per-capability consumption budgets
- package ecosystem, artifact digest and dependency scope
- maximum runtime-selected resources
- maximum execution duration
- terminal retention TTL
- optional cap on concurrent active runs
- optional lower deadline for each model call

## Key fields

```yaml
spec:
  allowedCapabilities:
    - kubernetes.read
    - github.pull_request
    - model.inference
    - network.egress
  kubernetesRead:
    namespaces:
      - sproozi-demo
    resources:
      - pods
      - pods/log
      - events
      - services
      - deployments
      - replicasets
      - statefulsets
  githubPullRequest:
    repositories:
      - andrewmccall/home-ops
    allowedBaseBranches: [main]
  egressProfiles:
    - go-modules
    - pypi
  budgets:
    model.inference:
      maxUnits: 100000
      maxCostMicros: 500000
  resourceBounds:
    max:
      cpu: "2"
      memory: 2Gi
      ephemeral-storage: 4Gi
  maxExecutionDuration: 1h
  retentionTTL: 720h
  maxActiveRuns: 4
  maxModelCallSeconds: 900
```

## Relationships

- referenced by `AgentTemplate.spec.policyRef`
- evaluated when a run is admitted
- consulted again by the shared gateway before each semantic or MCP operation during an active run

## Policy changes and denials

Policy changes affect later gateway decisions for active runs. Deleting an
AgentPolicy fails every non-terminal run whose template currently references
it, running ones included, with `PolicyNotFound`, and stops their sandboxes. A denied request
returns a structured denial to the sandbox. The gateway does not widen access to
make the request succeed.

All applicable checks must pass: network restrictions, run identity, requested
capabilities, current policy, run status, operation scope and upstream
restrictions. Policy controls retention and lifetime. Task text cannot change
them.

## Concurrent runs

`spec.maxActiveRuns` is optional and must be from 1 to 10000. It caps how many
AgentRuns admitted under this policy can be `Admitted` or `Running` at once.
Omit it for no per-policy limit; the default install sets no limit. Templates
that reference the same policy share its cap. Zero is not accepted here,
whereas a budget limit set to `0` leaves that dimension uncapped and `--max-active-runs=0` means no
cluster cap.

A run that would exceed the cap stays `Queued` with `Ready` reason
`WaitingForConcurrencyCap`; it is never denied. Its message names the cap,
for example `AgentRun is held by a concurrency cap: policy cap 1 (AgentPolicy
sre) reached`. A run counts against the policy recorded in its
`status.policyName` when it was admitted. If its template is re-pointed to
another policy while the run is active, the run fails with reason
`PolicyChanged` and the message `AgentTemplate T now references AgentPolicy B,
but this run was admitted under AgentPolicy A; submit a new run`. The gateway
denies the run's new connections at once and closes its open ones within the
[revocation bound](security-hardening.md#protect-transport-and-identities),
under 16 seconds. Permissions and the cap therefore always come from the same
policy. A run admitted before `status.policyName` existed has no recorded
policy: it isn't failed, counts against the policy its template references, and
its new connections are checked against that policy. Queued runs under other policies are not held
behind a full policy. Raising or removing the cap admits waiting runs without
waiting for another run to finish.

Editing `maxActiveRuns` doesn't close a run's open inspected connections at
the gateway, because the cap bounds how many runs start, not what an admitted
run may do. Every other edit to the policy's `spec` still closes them; label
and annotation edits don't. Gateway audit records
keep the `policyGeneration` the connection was admitted under, so after a
cap-only edit they show the earlier generation.

A policy edit can end an admitted run. The controller re-evaluates every
non-terminal run against the live policy, so an edit that makes a run
inadmissible fails it with the denial reason: removing a capability it
requested, dropping an egress profile its template selects, lowering a resource
bound below its runtime, or leaving a requested package or MCP capability
without a valid scope. Narrowing a valid scope, such as removing one MCP tool,
leaves the run running and only changes which of its later requests the gateway
allows. Shortening `maxExecutionDuration` below a run's elapsed time times it
out. See [policy edits](agentrun.md#policy-edits) in the AgentRun reference. Other
edits leave the run `Running` with `Ready` `True`: its open connections close and
its new connections get the edited policy. The only trace is the gateway log
line `Revoked inspected connection`. Read it with
`kubectl logs -n sproozi-system deployment/sproozi-gateway`.

The controller flag `--max-active-runs` sets a separate cluster cap; there is
no cluster cap by default. See
[Limit concurrent runs](../guides/install-from-checkout.md#limit-concurrent-runs)
and the
[concurrency warnings](security-hardening.md#concurrency-and-gateway-capacity).

## Model call deadline

`spec.maxModelCallSeconds` is optional and must be from 1 to 3600. It lowers
the [model call deadline](../../GLOSSARY.md#model-call-deadline), the longest
one model call of a run may stream, counted from when the gateway receives the
call. The gateway's own maximum applies when it is lower or when this field is
unset: 1200 seconds unless the administrator sets
`SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` on the shared gateway or
`gateway.maxModelCallSeconds` on the home gateway. The field can never raise
that maximum.

At the deadline the stream ends without an error event, and the call is
charged an estimate; see
[long model calls](../guides/model-provider-auth.md#long-model-calls). Editing
`maxModelCallSeconds` is a spec edit, so like any other spec edit it closes the
run's open inspected connections, including a model call in progress.

## Endpoint budgets

`spec.budgets` is an optional map keyed by capability. Omit an entry to leave that
capability's consumption uncapped. `maxUnits` measures tokens for model inference
and upstream requests for the other endpoints. An entry must set both
`maxUnits` and `maxCostMicros`; the API server rejects an entry that omits one,
for example `spec.budgets.kubernetes.read.maxCostMicros: Required value`. At
least one of the two must be positive. Set the other to `0` to leave that
dimension uncapped. Each limit can be at most 10^15 (1000000000000000).

The deployed Kubernetes ledger shares accounting across gateway replicas, with
separate accounts for each Run UID and capability. Policy changes do not reset
consumption. Non-model endpoints spend one request unit before each authorized
upstream call.

`maxCostMicros` requires trusted endpoint pricing:

- Model inference uses the administrator's pricing table. Cost-bounded OpenAI
  API-key and Anthropic requests need an exact model row with positive input,
  cached-input and output rates before the gateway forwards them.
- Cost-bounded OpenAI requests use `service_tier: default`. The gateway inserts
  it when missing and rejects other tiers. Anthropic has its own standard-tier
  and cache rules, and refuses `context-1m` long-context betas, which the
  pricing table cannot price.
- Other endpoints have no trusted pricing, so they reject a cost limit.
- In ChatGPT-plan mode, set a token limit and `maxCostMicros: 0`.

Token and cost limits are not hard provider-spending ceilings.
[Security hardening](security-hardening.md#model-budgets-are-not-hard-spending-ceilings)
explains how reservations and settlement work and what they miss.
[Model providers](harnesses.md#model-providers) lists the protocol-specific
rules.

Kubernetes namespaces, resources and verbs are checked on each request by the
Kubernetes endpoint. Workload service accounts have no native read bindings.
See [Security hardening](security-hardening.md) for deployment controls.

## Limits

| Field | Limits |
| --- | --- |
| `spec.allowedCapabilities` | 1 to 32 entries. |
| `spec.mcpServers` | At most 32 servers; each name is a DNS label. |
| `spec.budgets` | At most 32 entries, keyed by a native or `mcp.<server>` capability. |

## Durations

| Field | Default | Limits |
| --- | --- | --- |
| `spec.maxExecutionDuration` | `1h` | Greater than zero and at most `1h`. The API server rejects `2h` or `0s`. |
| `spec.retentionTTL` | `720h` (30 days) | Greater than zero. A terminal AgentRun is deleted this long after it finished. |

Omit a field to take its default. A Go client using the API types also omits
them when they are unset.

## Package installs

`spec.packagesInstall` lists the exact Python wheels a run may download through
the `packages.install` capability. Nothing outside the list is served.

| Field | Meaning | Limits |
| --- | --- | --- |
| `ecosystems` | Registries this scope covers. Only `pypi` is supported. | Up to 8 entries. |
| `artifacts[]` | The complete set of wheels and their locked dependencies. | Up to 128 entries. |
| `artifacts[].name` | Normalized package name. | 1 to 128 characters, lowercase with `.`, `_` or `-` separators. |
| `artifacts[].version` | Exact PEP 440 version. | 1 to 64 characters. |
| `artifacts[].sha256` | Lowercase hexadecimal SHA-256 of the wheel bytes. | 64 characters. |
| `artifacts[].filename` | Exact wheel filename from registry metadata. | 1 to 255 characters. |
| `artifacts[].dependencies[]` | `name` and `version` of each locked dependency. Each must also appear in `artifacts`. | Up to 64 entries. |
| `allowSourceBuilds` | Must stay `false`: only wheels are supported. | |
| `maxDownloadBytes` | Cap on each artifact or metadata response. | 1 byte to 1 TiB (1099511627776 bytes). |

```yaml
packagesInstall:
  ecosystems: [pypi]
  maxDownloadBytes: 52428800
  artifacts:
    - name: requests
      version: "2.32.3"
      filename: requests-2.32.3-py3-none-any.whl
      sha256: <64 hex characters from the registry>
      dependencies:
        - {name: idna, version: "3.10"}
    - name: idna
      version: "3.10"
      filename: idna-3.10-py3-none-any.whl
      sha256: <64 hex characters from the registry>
```

## Named MCP capabilities

Allow `mcp.<server>` and provide an explicit `spec.mcpServers[server].tools`
map. Optional `arguments` restrictions use JSON Schema to limit tenants,
repositories or other inputs. The gateway checks these rules as well as the
provider's discovered input schema. Missing tools or invalid restrictions deny
the run at admission or the request at the gateway. One MCP budget unit means
one attempted tool invocation; discovery is bounded but uncharged. Provider URLs
and credentials belong to gateway registration, not policy or run input. See
[configured MCP capabilities](configured-mcp.md) for examples, schema bounds and
transport limits.
