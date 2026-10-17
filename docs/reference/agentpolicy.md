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
```

## Relationships

- referenced by `AgentTemplate.spec.policyRef`
- evaluated when a run is admitted
- consulted again by the shared gateway before each semantic or MCP operation during an active run

## Policy changes and denials

Policy changes affect later gateway decisions for active runs. A denied request
returns a structured denial to the sandbox. The gateway does not widen access to
make the request succeed.

All applicable checks must pass: network restrictions, run identity, requested
capabilities, current policy, run status, operation scope and upstream
restrictions. Policy controls retention and lifetime. Task text cannot change
them.

## Endpoint budgets

`spec.budgets` is an optional map keyed by capability. Omit an entry to leave that
capability's consumption uncapped. `maxUnits` measures tokens for model inference
and upstream requests for the other endpoints. At least one limit must be positive.
A zero limit leaves that dimension uncapped.

The deployed Kubernetes ledger shares accounting across gateway replicas, with
separate accounts for each Run UID and capability. Policy changes do not reset
consumption. Non-model endpoints spend one request unit before each authorized
upstream call.

Model inference calls that share one budget account run one at a time. A second
call waits for the first to finish and settle usage. Cancellation and execution
deadlines still apply while it waits. Calls using different accounts remain
independent.

OpenAI requests reserve the requested output limit, or remaining capacity when
no output limit is supplied. They then settle provider-reported total usage. Input tokens
can exceed the reservation. Rejected settlement retains the reservation but does
not undo upstream consumption or fully record it. Token and cost limits are not
hard provider-spending ceilings. See [security hardening](security-hardening.md).

`maxCostMicros` requires trusted endpoint pricing. Model inference uses the
administrator's pricing table. On the OpenAI path, missing prices are rejected
during settlement after the provider call. Other endpoints reject a monetary ceiling because they
do not have trusted pricing. A configured budget
in ChatGPT-plan mode uses a token ceiling and `maxCostMicros: 0`.

Anthropic requests reserve all remaining configured token and monetary capacity.
When a monetary cap applies, the gateway requires an exact model price before
forwarding. Missing rates for a consumed category can still fail settlement.
Neither provider route guarantees a hard spending ceiling. See
[model providers](harnesses.md#model-providers) for the protocol-specific rules.

Kubernetes namespaces, resources and verbs are checked on each request by the
Kubernetes endpoint. Workload service accounts have no native read bindings.
See [Security hardening](security-hardening.md) for deployment controls.

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
