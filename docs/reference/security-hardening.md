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
active Run state. It checks new requests against live policy and closes existing
sessions through bounded revalidation. Authentication and each live authority
lookup have a five-second deadline. The default one-second revalidation interval
can therefore take up to another five seconds to resolve a slow lookup before
closing the session. A timeout does not prove the dependency or upstream work
stopped, and revocation cannot reverse committed side effects.

Both gateway backends admit at most 128 TCP connections before TLS/header parsing
and eight inspected sessions per execution by default. Excess connections close;
CONNECT capacity exhaustion returns 503 and per-execution session exhaustion
returns 429. Sessions retain an absolute five-minute lifetime, including CONNECT
acknowledgment and TLS setup. The transport also bounds pending authority callback
workers and retains their slots until those callbacks return. These are finite
concurrency limits, not traffic-rate fairness or a measured capacity guarantee.
The typed transport configuration validates changes; there are no deployment
environment overrides for these bounds.

The projected token lifetime is two hours; policy limits execution to one hour.
The demo reads the token once. Revocation depends on live gateway checks rather
than waiting for token expiry.

Keep `sproozi-agents` dedicated to managed sandboxes. Restrict who can create
workloads, ServiceAccounts or runtime templates. Runtime validation rejects init
and ephemeral containers. All normal containers in a Pod share one run principal.
For working completion today, use one container named `agent` and no long-running
ordinary sidecar. Every container must drop ALL Linux capabilities and add none,
use RuntimeDefault seccomp and a read-only root filesystem, and prohibit privilege
escalation. Shared process namespaces, envFrom, Secret environment references and
template-provided Secret or projected volumes are rejected. Allowed template
volumes contain exactly one positive size-limited EmptyDir, ConfigMap or
DownwardAPI source. The controller alone injects the gateway token and managed
continuity volumes. Runtime authors remain trusted: public ConfigMaps and literal
environment values must not contain provider credentials.

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
that subset; use the documented bounded REST PR operation.

The Kubernetes route binds resources to core/v1 or apps/v1, removes the entire
Impersonate-* header family and refuses upstream redirects in every composition.
See [the resource scope](permissions-and-capabilities.md#kubernetes-read-scope).

## Model budgets are not hard spending ceilings

The deployed gateway stores reservations atomically in Kubernetes, keyed by Run
UID and capability. Requests with an output-token limit reserve that limit,
while provider settlement counts total input and output tokens. Input usage can
therefore exceed the reservation. Rejecting a response after settlement fails
does not undo the provider call, and retained reservations can undercount the
consumption already incurred.

Administrator-owned pricing converts verified usage into cost. Cost-bounded
OpenAI API-key and Anthropic inference require an exact model row with positive
input, cached-input and output rates before provider work. OpenAI cost-bounded
requests select `service_tier: default`; omission inserts that value and explicit
nondefault tiers are refused. Anthropic retains its separate standard-tier and
cache-category checks. Missing consumed-category pricing or uncertain settlement
can still withhold a response after provider consumption. Keep the table aligned
with approved models and use provider-side monitoring and limits.

OpenAI admission accepts only `GET /v1/models`, `POST /v1/responses` and
`POST /v1/chat/completions`, without query strings or escaped paths. ChatGPT
retains its narrower Responses contract. Both OpenAI authentication modes remove
workload organization, project and account selector headers before applying the
trusted provider credential. Use a suitably scoped key; workload headers cannot
select the provider identity.

The API-key route still has no complete billable-field allowlist. The token
ledger does not price all provider-hosted tools, storage, image/audio work or
other non-token charges. Native client-executed tool declarations remain supported
and must not be confused with provider-hosted execution. These changes do not
make token or cost ceilings hard provider-spending bounds. See
[model authentication and admission](../guides/model-provider-auth.md#protocol-and-budgets).

## Cleanup and state retention

Terminal handling and explicit deletion retain containment until the Pod is
observed absent or terminal. An existing Pod with Unknown or empty phase does not
establish absence and keeps its contract, NetworkPolicy and ServiceAccount.
When a provisioning status write is missing, release looks up the deterministic
Pod name and requires its managed-by and exact Run UID labels before adoption.
It also recovers the deterministic identity/network names. Lookup errors or
ownership mismatches block cleanup. Inspect ownership and containment after
interrupted runs; this recovery does not justify forced removal of managed writer
finalizers or infer sealed usage from Pod deletion.

`retentionTTL` governs the AgentRun record. Budget ConfigMaps have no automatic
retention/deletion path. Webhook replay keys are stored atomically in ConfigMaps
in `sproozi-webhook-state`; they survive restarts and are shared by replicas.
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
