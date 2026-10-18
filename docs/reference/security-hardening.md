# Security hardening

Sproozi separates agent execution from trusted policy and credentials. Use the
controls below when deploying it, and account for the current implementation
limits.

## Protect transport and identities

Use the shared inspected-TLS gateway with a dedicated trust bundle. Treat
`AgentRuntime.spec.gatewayEndpoint` as administrator-owned configuration. Deny
direct API/provider access and proxy bypasses through an enforcing CNI, and
verify those denials from the actual sandbox. Standard NetworkPolicy permits
traffic involving the Pod's own node. Do not infer API isolation from policy
alone when the API server is on that node. See
[Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

The gateway validates the ServiceAccount namespace, name, UID, audience and
active Run state. It checks each new request against live policy. It rechecks
open connections every second and closes them once the run is no longer active
or allowed. Authentication and each live policy lookup have a five-second
deadline, so a slow lookup can delay that close by up to five more seconds. A
timeout doesn't prove that the lookup or the upstream work stopped, and
revoking access can't undo changes an upstream service already made.

Both gateway backends (Kubernetes and Docker) apply these default limits:

| Limit | Default | When exceeded |
| --- | --- | --- |
| TCP connections before TLS and header parsing | 128 | Excess connections close. Exhausted CONNECT capacity returns 503. |
| Inspected sessions per execution | 8 | The gateway returns 429. |
| Session lifetime, including CONNECT and TLS setup | 5 minutes | The session closes. |
| Authentication and live policy lookup | 5 seconds each | The lookup times out; see above. |

Pending policy lookups also have a fixed number of slots. A slow lookup keeps
its slot until it returns, even after timing out. These limits bound
concurrency; they don't provide rate fairness or a measured capacity guarantee.
You can't override them with deployment environment variables.

The projected token lifetime is two hours; policy limits execution to one hour.
The demo reads the token once. Revocation depends on live gateway checks rather
than waiting for token expiry.

Keep `sproozi-agents` dedicated to managed sandboxes. Restrict who can create
workloads, ServiceAccounts or runtimes. All containers in a Pod share one run
identity. For completion to work today, use one container named `agent` and no
long-running sidecar. The [AgentRuntime container
rules](agentruntime.md#container-validation-rules) list what admission rejects.
Runtime authors are trusted: keep provider credentials out of public ConfigMaps
and literal environment values.

## Restrict remote MCP access

Treat the gateway's MCP registry and provider credential mounts as
administrator-owned configuration. Mount provider tokens only in the gateway,
under `/var/run/secrets/sproozi/mcp`. Registration and credential changes require
a gateway restart; run and policy authorization remain live.

Select tools explicitly in `AgentPolicy.spec.mcpServers` and use argument
restrictions for required tenant, namespace or repository scope. Discovered
schemas and policy restrictions check request arguments. They cannot prove that
a tool is read-only or that the server respects the requested resource scope.
Use reviewed providers, restricted credentials and upstream-native controls for
those guarantees. Registered provider authorities are reserved against weaker
destination routing; verify direct network denial from the sandbox as well.

Configured MCP budgets count attempted tool invocations, including failures.
They do not price provider work or charge initialization and discovery. See
[configured MCP capabilities](configured-mcp.md) for supported operations,
schema bounds and session limits, and
[ADR 0001](../adr/0001-mcp-capabilities-through-shared-gateway.md) for the boundary
and ownership decision.

## Retain GitHub protections

Install the GitHub App only on approved repositories and grant the minimum
permissions for branch push and PR creation. Keep branch protection or rulesets
enabled. The gateway authorizes bounded operations; it does not replace GitHub's
protected-branch rules. Review proposed PRs before merging or deploying them.
GraphQL reads validate the bounded document, repository argument bindings and
selected fields before forwarding. The supported subset is listed in
[client compatibility](client-compatibility.md); unsupported fragments, aliases
and selections are rejected. The pinned `gh pr create` workflow remains outside
that subset; use the documented bounded REST PR operation. Like GraphQL, REST PR
creation forwards a body rebuilt from the checked fields, so a case variant,
duplicate key, extra field or query parameter cannot change the head, base or
source repository that GitHub uses.

The Kubernetes route binds resources to core/v1 or apps/v1, removes the entire
Impersonate-* header family and refuses upstream redirects in every composition.
See [the resource scope](permissions-and-capabilities.md#kubernetes-read-scope).

## Model budgets are not hard spending ceilings

This section is the reference for how budget limits behave. Field syntax is in
[AgentPolicy budgets](agentpolicy.md#endpoint-budgets).

The deployed gateway stores reservations atomically in Kubernetes, keyed by Run
UID and capability. Before a model call, the gateway reserves capacity:

- OpenAI requests reserve the requested output-token limit, or all remaining
  capacity when the request sets none.
- Anthropic requests reserve all remaining token and cost capacity.

After the call, the gateway settles the provider-reported input and output
tokens. Input tokens can push usage past the reservation. When trusted usage
exceeds the reservation or the remaining allowance, the gateway withholds the
response and records the usage, capped at the allowance. The account is then
charged what the provider reported or is exhausted, and later requests are
refused, so repeating an oversized request cannot spend without limit. The
provider has already done the work, and usage above the allowance is not
recorded. If the accounting store itself fails, the response is withheld and
the reservation stays charged, which can undercount what was spent.

Within one gateway process, model calls that share a budget account run one at
a time. Cancellation and execution deadlines still apply while a call waits.

Administrator-owned pricing converts verified usage into cost; the
[pricing rules](agentpolicy.md#endpoint-budgets) say which rows a cost limit
needs. A missing price for a category the provider charged, or an uncertain
settlement, can still withhold a response after the provider did the work.
Under a cost limit, Anthropic requests that select a `context-1m` long-context
beta are refused, because the table has no premium long-context rate. Keep
the pricing table aligned with approved models, and use the provider's own
monitoring and spending limits.

Model handlers refuse a request body that is not one JSON value, or that
repeats a key within one object. In the objects the gateway's checks decode,
such as the top-level request, messages, content blocks and `cache_control`,
keys that differ only by case are also refused. The gateway's checks and the
provider could otherwise read different values from the same body.
`OPENAI_UPSTREAM_URL` and `ANTHROPIC_UPSTREAM_URL` must be bare `https` origins.

OpenAI admission accepts only `GET /v1/models`, `POST /v1/responses` and
`POST /v1/chat/completions`, without query strings or escaped paths. ChatGPT
retains its narrower Responses contract. Both OpenAI authentication modes remove
workload organization, project and account selector headers before applying the
trusted provider credential. Use a suitably scoped key; workload headers cannot
select the provider identity.

The API-key route has no complete allowlist of billable request fields. The
token ledger does not price provider-hosted tools, storage, image or audio work,
or other non-token charges. (Tools that the harness runs itself are supported;
they are not provider-hosted.) See
[model authentication and admission](../guides/model-provider-auth.md#protocol-and-budgets).

## Cleanup and state retention

Sproozi keeps a run's NetworkPolicy, ServiceAccount and contract until it sees
the Pod gone or terminal. A Pod in `Unknown` phase counts as still running. If
Sproozi can't confirm which Pod belongs to a run, cleanup waits. After an
interrupted run, check the Pod and its containment yourself. Don't remove
Sproozi finalizers by hand. A deleted Pod doesn't mean its usage was recorded.
[ADR 0014](../adr/0014-review-boundaries-and-native-contracts.md#implementation-notes)
records how release recovers ownership.

`retentionTTL` governs the AgentRun record. Budget ConfigMaps have no automatic
retention/deletion path. Webhook replay keys are stored atomically in ConfigMaps
in `sproozi-webhook-state`; they survive restarts and are shared by replicas.
Each record is kept for two replay windows after its event's timestamp, so
replicas whose clocks differ by less than one window cannot accept an event
again after another replica removes its record.
The webhook deletes expired replay entries once per replay window, so this state
stays bounded by recent event volume.

## Restrict trusted services

The installed controller-manager can manage Pods, identities, NetworkPolicies
and RBAC objects. Its binding does not grant Secret reads; the optional metrics
overlay adds TokenReviews and SubjectAccessReviews. Restrict exec/debug access
and reuse of trusted service accounts. The gateway holds provider credentials
and may update its rotating ChatGPT session Secret. ChatGPT refresh requires one active gateway process,
even though budget and replay state are shared. See
[model provider authentication](../guides/model-provider-auth.md).

The webhook receiver refuses a request with missing, malformed or stale
signature headers before it reads the body or calls the API server, and bounds
header, body, read, write and idle time. See the
[webhook contract](webhook-contract.md#request-checks-and-limits).

The local demo's short-lived CA and gitignored environment file are disposable
evaluation inputs. For deployment, supply managed certificates, least-privilege
credentials, namespace isolation and durable audit collection. The [home-ops playbook](../guides/home-ops-playbook.md) describes a planned
integration; its Alertmanager converter and signer are not supplied.


## Persistent assistant permissions and answers

The Hermes coordinator is a separate trusted workload. It keeps its own model
and optional channel credentials and persistent state. It has no Kubernetes
API token; the private task service holds namespaced Run create/observe/patch
permission and fixes the templates and capability subsets exposed to that
installation. Use separate task principals and bearer tokens for assistants
requiring different authority. A shared bot token does not authenticate each
channel user independently.

Worker final text is untrusted and can contain sensitive scoped tool results. It
is retained in AgentRun status, visible to authorized Run readers, and expires
with Run retention. Do not treat it as operating instructions or grant more
permissions to a later task because of answer text. Coordinator state has
separate retention and spending limits. See [orchestration](orchestration.md)
for the exact retry and result bounds.
