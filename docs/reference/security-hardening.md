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

Sandboxes resolve no names, on Kubernetes as on Docker. The NetworkPolicy has
no DNS rule, the Pod's `dnsPolicy` is `None` with `127.0.0.1` as its only
nameserver, and its hosts file maps the gateway host to the address the
controller resolved when it created the Pod. A query for an attacker's domain
therefore never leaves the Pod, so DNS cannot carry data past the gateway's
policy, audit and accounting. Clients reach every other host through the
gateway as an HTTPS proxy, which resolves names itself. If the gateway Service
is recreated with a new cluster IP, running sandboxes lose the gateway until
their runs end; new runs get the new address. Keep `gatewayEndpoint` a host
name: managed harnesses also use that host for their control connection, and
the gateway does not serve an IP address as an inspection host. See
[ADR 0040](../adr/0040-sandboxes-reach-the-gateway-without-dns.md).

Every Pod in `sproozi-agents` is denied all ingress and egress by
`sproozi-sandbox-default-deny`, and each run's own NetworkPolicy then allows the
gateway. Policies add up, so a sandbox whose run labels are changed, or whose
per-run policy is deleted, has no network rather than the cluster's default.
Release refuses to report a run-named Pod whose labels no longer match the run,
so someone looks at it.

The gateway also checks which Pod a projected token was issued to. The
TokenReview must carry exactly one `authentication.kubernetes.io/pod-name` and
one `authentication.kubernetes.io/pod-uid` extra, so a token with no Pod
binding is refused. That covers one minted with
`kubectl create token sproozi-<uid> --audience sproozi-gateway` and one bound
to a Secret. The Pod name must be the run's sandbox name, and the Pod UID must
equal the UID of the sandbox Pod the gateway reads live from `sproozi-agents`
when the connection opens. A token from any other Pod is refused, even if that
Pod uses the run's ServiceAccount, and so is a token from an earlier Pod with
the sandbox's name. Every refusal returns the same `401 invalid gateway
identity`; the gateway logs the cause, never the token, once per cause every
10 seconds, with a count of the repeats.

The CNI must also enforce a Pod's policy from the moment the Pod starts.
Sproozi creates each run's NetworkPolicy before its Pod, but some
implementations apply it only after the Pod is running. Kind's default
network (kindnet) let a new sandbox reach the API server directly for about a
second; Calico v3.30.4 on Kind blocked it on every attempt. Run the first
check from [Install on Kubernetes](../guides/install-kubernetes.md) more than
once on a warm node: a policy applied late shows as
`UNEXPECTED: direct API server path was reachable`.

The gateway validates the ServiceAccount namespace, name, UID, audience and
active Run state. Each new connection (CONNECT) is checked with a live
TokenReview and live reads of the run, template, policy, ServiceAccount and
sandbox Pod.
The gateway then checks each request on that connection, and rechecks the open
connection every second, and closes it once the run is no longer active or
allowed.

On Kubernetes, those per-request and one-second checks read the run, template,
policy and ServiceAccount from the gateway's watch cache, so for ordinary runs
API server load doesn't grow with connections times checks. When the cache
disagrees with what the connection was admitted under, the gateway confirms
with a live read before it closes the connection. Each execution with open
connections also gets a live recheck at least every 10 seconds, so a stalled
watch can't hide a revocation for long.

Managed-session registrations and sandbox Pods are always read live, because a
session move revokes the source through them. For a managed session, API reads
therefore do grow with connections and checks:

| Event | Live API reads |
| --- | --- |
| Each managed session, every second | 2 GETs |
| Each inspected request | 2 more GETs |
| 10-second live recheck | about 0.4 per second more per execution |
| Each new CONNECT | 1 TokenReview and 9 GETs |

Authentication and each authority lookup have a five-second deadline. A
revocation therefore closes open connections:

| Gateway | Bound |
| --- | --- |
| Before the watch cache (and the Docker gateway today) | under 10 seconds: a revalidation that read the old state can take up to the 5-second lookup deadline to return, and the next one then starts at once with its own 5-second deadline; about 1 second plus read latency when the API server answers promptly |
| Kubernetes, watch delivering | the smaller of 1 second plus watch delay plus 5 seconds, and under 16 seconds. For a managed session, the smaller of watch delay plus 10 seconds, and under 16 seconds: its cached pass still reads the registration and Pod live, so a pass that read the old state can take up to the 5-second deadline, like the row above |
| Kubernetes, watch stalled | under 16 seconds (10 + 1 + 5); about 11 seconds plus read latency when the API server answers promptly |

The 10-second live recheck applies whether or not the watch is delivering, so a
slow watch can't make the bound worse than a stalled one.

When the gateway closes a connection on revocation, it logs `Revoked inspected
connection` with `backend`, `namespace`, `execution`, `executionUID` and
`reason`. Further revocations for the same execution within 10 seconds are
collapsed into one `Revoked further inspected connections` line with a `count`.

A timeout doesn't prove that the lookup or the upstream work stopped, and
revoking access can't undo changes an upstream service already made.

Both gateway backends (Kubernetes and Docker) apply these default limits:

| Limit | Default | When exceeded |
| --- | --- | --- |
| TCP connections before TLS and header parsing, shared by all runs | 2048 | Excess connections close. Exhausted CONNECT capacity returns 503. |
| Inspected connections per execution | 16, or the connection limit if lower | The gateway returns 429 with `execution inspected-connection limit reached`. The first refusal logs `Rejected inspected connection at per-execution limit` with `backend`, `namespace`, `execution`, `executionUID` and `limit`; later refusals for that execution within 10 seconds log one `Rejected further inspected connections at per-execution limit` with the same keys and a `count`. |
| Inspected connection lifetime, including CONNECT and TLS setup | 5 minutes | The connection closes. Each request on it other than a model call must finish by the request deadline, 10 seconds earlier, so one such request, such as a watch, has at most 4 minutes 50 seconds from the CONNECT. A model call has its own deadline (next row), and the connection stays open for it until it ends. At the request deadline the gateway turns off keep-alive: an idle connection closes, and a running request's reply carries `Connection: close` if its headers haven't been sent. A request other than a model call still running then gets its handler's reply, and a watch ends; the gateway logs `Ended inspected request at connection lifetime` with `execution`, `host`, `method` and `lifetime`. A request that arrives after the deadline gets a 503 with `Connection: close` and `gateway connection reached its lifetime; open a new connection and retry`. |
| Model call deadline, from when the gateway receives the call | 20 minutes; `SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` on Kubernetes, `gateway.maxModelCallSeconds` at home, both 1–3600 seconds | The stream ends without an error event and the call is charged an estimate ([interrupted calls](../guides/model-provider-auth.md#interrupted-calls)). `AgentPolicy.spec.maxModelCallSeconds` can lower it for a policy's runs. A call still waiting for budget gets a 408 (for OpenAI with code `request_cancelled`) and is not charged. The shared gateway waits up to 4 minutes 50 seconds for a provider's response headers and the home gateway 45 seconds; a call whose headers don't arrive gets a 502 and, because the provider had the request, is charged an estimate. A response that sends nothing for 5 minutes ends and is charged an estimate. |
| Authentication and authority lookup | 5 seconds each | The lookup times out; see above. |

Every admitted CONNECT becomes an inspected connection: the gateway terminates
and checks the TLS of each one, so all 2048 connections can be inspected at
once. Pending authority lookups share the connection limit. A slow lookup keeps its
place until it returns, even after timing out. These limits bound concurrency;
they don't provide rate fairness or a measured capacity guarantee.

On Kubernetes, set the first two limits on the gateway Deployment with
`SPROOZI_GATEWAY_MAX_CONNECTIONS` (1–4096) and
`SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION` (1–128, no more than the
connection limit), and the model call deadline with
`SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` (1–3600, default 1200). The second variable keeps its name but counts inspected
connections. The install sets only the first; left unset, the second defaults
to 16 or the connection limit, whichever is lower. An invalid value stops the gateway at startup, and because the
gateway Deployment uses the `Recreate` strategy
([ADR 0026](../adr/0026-single-gateway-refresh-owner-with-recreate.md)), the
gateway stays down until you correct it. The Docker gateway uses the defaults, apart from the model call deadline, which
the home configuration's `gateway.maxModelCallSeconds` sets. See
[concurrency and gateway capacity](#concurrency-and-gateway-capacity) before
changing them.

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

### Gateway certificates

Rotate gateway certificates before they expire. The Kubernetes gateway and the
local service both serve a renewed certificate to new connections without
restarting; a replacement that misses an inspected host or has expired is
refused. The optional Kubernetes certificate controller holds its CA key in one
Secret. Protect `sproozi-system` Secrets and etcd encryption accordingly. Its
trust bundles are certificate-only and immutable. Treat `replace` as an
emergency that removes every running Pod's trust in the gateway. See
[maintain gateway certificates](gateway-certificates.md).

A [local installation](local-installation.md) keeps its CA key, provider keys
and journals in owner-only files. Its gateway listens on loopback when the
engine forwards there, as Docker Desktop does. Where Docker reaches neither
loopback nor the bridge address, it listens on every interface; it still
requires TLS 1.3 and a per-execution token, but a host firewall that blocks
the gateway port from the network keeps unauthenticated connections off it. The user's
[profile](profile.md) reaches every session of that user and is not isolation between
untrusted users or agents. Anyone controlling the user account or the Docker
engine controls the local installation.

## Concurrency and gateway capacity

> **Warning:** There is no cluster cap by default, and the default install sets
> no policy caps. With no cluster cap (`--max-active-runs`) and no policy cap
> (`maxActiveRuns`), the number of active runs is bounded only by
> ResourceQuotas, node capacity and the gateway's connection limits. Set caps before you share a cluster.

Every active run adds load that Sproozi doesn't cap for you:

- **Gateway memory and CPU.** The gateway inspects TLS on every connection, so
  each open connection holds TLS buffers and certificate state. A streaming
  inspected connection measured about 150–160 KiB of heap and stack across runs
  of `TestInspectedSessionMemory`; the install budgets 256 KiB for each of the
  default 2048 connections. TLS handshakes cost CPU. Model streams keep connections open for minutes, and
  one gateway process carries the traffic of every run
  ([ADR 0026](../adr/0026-single-gateway-refresh-owner-with-recreate.md)).
- **Gateway resources.** The install sets the gateway's limits to 2 CPU and
  2Gi memory as ceilings, while its requests stay at 25m CPU and 128Mi. The
  scheduler reserves only the requests, so on a full node a busy gateway can be
  throttled or evicted. If you need guaranteed capacity, raise the requests. If
  you lower the memory limit, lower `SPROOZI_GATEWAY_MAX_CONNECTIONS` with it.
  The sizing rule is one connection per MiB of `limits.memory`: the 256 KiB
  budget covers one inspected connection's TLS buffers, certificate state, heap
  and stack, so the connection budgets take a quarter of the limit (2048 ×
  256 KiB = 512 MiB of 2Gi). The other three quarters are headroom for model
  bodies, the gateway's own heap and the garbage collector, which runs harder
  as the heap nears the Go memory limit at 90% of `limits.memory`. Model calls
  use that headroom: each one in flight holds its request body, up to 1 MiB,
  and the response event it is parsing, usually a few KiB but up to 8 MiB for
  one large event (see **Model request bodies** below), and inspected
  connections carry one HTTP/1.1 request at a time. A call holds this memory
  for up to the model call deadline, 20 minutes by default. So the headroom
  fits a few hundred concurrent model calls with full-size requests, not one
  on every connection. In the worst case each call holds a 1 MiB request and
  an 8 MiB event at once, about 9 MiB before copies, so the roughly 1.3 GiB
  left under the Go memory limit (90% of 2Gi, less the 512 MiB of connection
  budgets) fits fewer than 150 such calls.
  Realistic events are far smaller, so this worst case needs every call to be
  parsing a near-limit event at the same moment. The gateway sets its Go
  memory limit to 90% of `SPROOZI_GATEWAY_MEMORY_LIMIT`, which the install
  fills from `limits.memory`, unless `GOMEMLIMIT` is set. The variable must be
  at least 64Mi (67108864 bytes), even when `GOMEMLIMIT` is set, or the gateway
  doesn't start. At startup the gateway logs the limit in effect and its source,
  such as `Go memory limit is N bytes, from 90% of SPROOZI_GATEWAY_MEMORY_LIMIT=...`,
  `from GOMEMLIMIT=...`, or `Go memory limit is unlimited`.
  An explicit `GOMEMLIMIT` must be a valid Go value: a byte count with an
  optional `B`, `KiB`, `MiB`, `GiB` or `TiB` suffix, or `off`. Any other value
  makes the Go runtime abort at startup, before Sproozi code runs, with
  ``fatal error: malformed GOMEMLIMIT; see `go doc runtime/debug.SetMemoryLimit` ``.
- **Model request bodies.** Each model call in flight reads its request body,
  up to 1 MiB, into memory outside the per-connection budget, plus allocation
  overhead at peak: the read buffer grows by reallocation and JSON decoding
  makes further copies. Responses stream through: the gateway keeps only the
  event it is parsing, up to 8 MiB, because an OpenAI `response.completed`
  event repeats the whole output. A response that doesn't stream is kept, up
  to 8 MiB, to read its usage. Many concurrent model calls need more memory
  headroom than the connection count suggests.
- **API server load.** The controller lists every AgentRun on each admission
  check, and each run creates a ServiceAccount, NetworkPolicy, ConfigMap and
  Pod. The gateway's watch cache and live rechecks also grow with the number of
  runs, and managed sessions add live reads for every connection and request
  ([figures](#protect-transport-and-identities)).
- **ChatGPT plan limits.** In ChatGPT mode, every run uses the same ChatGPT
  plan, so all of them share its single rate limit. Concurrent runs can exhaust
  it for each other and for the plan's owner.
- **Spend.** Budgets apply per run, so N concurrent runs can spend N budgets at
  once. Model budgets are not hard ceilings either; see
  [Model budgets are not hard spending ceilings](#model-budgets-are-not-hard-spending-ceilings).
- **Burst submissions.** Runs start in parallel, so a script or tool that
  creates many AgentRuns at once starts as many runs.

For shared clusters:

1. Set the controller flag `--max-active-runs`; see
   [Limit concurrent runs](../guides/install-from-checkout.md#limit-concurrent-runs).
2. Set `maxActiveRuns` on each AgentPolicy, especially the policies that
   scripts and tools submit runs under; see [AgentPolicy](agentpolicy.md#concurrent-runs).
3. Put a ResourceQuota on `sproozi-agents` as a Kubernetes-level backstop:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: sproozi-sandboxes
  namespace: sproozi-agents
spec:
  hard:
    pods: "20"
    requests.cpu: "10"
    requests.memory: 20Gi
    limits.cpu: "20"
    limits.memory: 40Gi
```

Size the values from your runtimes' resource requests and limits. A quota on
`requests.*` or `limits.*` makes Kubernetes reject Pods that don't set them, so
check that every `AgentRuntime` sets both.

A cap holds a run in `Queued` before it is admitted. A quota acts later: the
run has already been admitted and moved to `Running` when Kubernetes rejects its
sandbox Pod. The run stays `Running` with `Ready` reason `QuotaExceeded` and
keeps counting against the caps. The controller retries with a backoff from 5
seconds up to one minute, and the wait counts toward the run's execution
deadline. If the quota still rejects the Pod at the deadline, the run ends
`TimedOut`. The controller recognises a quota rejection from the API server's
error, a `Forbidden` response whose message contains `exceeded quota`; other
Pod creation errors are retried without that condition. See the
[AgentRun reasons](agentrun.md#ready-condition-reasons).

## Treat runtime and policy authors as administrators

`AgentRuntime` and `AgentPolicy` are namespaced, but their authority is not.
A runtime's Pod template runs in the shared `sproozi-agents` namespace, and a
policy can grant gateway reads in any namespace it names. Anyone who can write
either object in any namespace is therefore a cluster administrator for
Sproozi. Grant the scaffolded `sproozi-agentruntime-editor-role`,
`sproozi-agentruntime-admin-role`, `sproozi-agentpolicy-editor-role` and
`sproozi-agentpolicy-admin-role` ClusterRoles only to administrators, including
through namespaced RoleBindings. Users who only start work need `AgentRun`
access and read access to templates.

The install also ships `sproozi-budgetledger-editor-role` and
`sproozi-budgetledger-admin-role`. Anyone who can write a
[BudgetLedger](budgetledger.md) can reset or inflate a run's usage, so bind
them to no one, or only to administrators; the viewer role is safe to grant.

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

## Harness sign-in exposes the credential to the agent

A harness that signs in itself ([ADR 0047](../adr/0047-harnesses-sign-in-to-their-own-model-providers.md)) can read its
own provider credential, where a gateway-held key never enters the sandbox.

- The gateway refuses that credential on any route other than its provider's,
  in the URL, headers or body, but only for traffic it inspects. Opaque
  egress destinations and an encoded or split copy get past it, so keep
  egress profiles narrow for runs that use a harness sign-in.
- A stolen sign-in works anywhere until you revoke it with the provider. Prefer
  short-lived or narrowly scoped tokens, and revoke one you suspect.
- On a forwarding route the workload's own headers choose the provider
  account; budgets still apply to every call.
- Locally, a harness's login volume outlives its sessions. `docker volume rm`
  removes the sign-in.
- Whether a provider's terms allow a mechanism is the administrator's call.

## Model budgets are not hard spending ceilings

This section is the reference for how budget limits behave. Field syntax is in
[AgentPolicy budgets](agentpolicy.md#endpoint-budgets).

The deployed gateway stores reservations atomically in Kubernetes, keyed by Run
UID and capability. Before a model call, the gateway reserves capacity:

- A self-contained request with an output bound (`max_tokens`,
  `max_completion_tokens` or `max_output_tokens`) makes a
  [bounded reservation](../../GLOSSARY.md#bounded-reservation): its bytes plus
  the output bound in units and, under a cost limit, its bytes at the highest
  input or cache-write rate plus the output bound at the output rate. Claude
  Code's requests carry `max_tokens`, so they are usually bounded.
- Any other request reserves all remaining units and cost: one without an
  output bound, such as every Codex and ChatGPT-mode call, one that references
  input its bytes don't contain, such as `previous_response_id` or a `file_id`,
  and one that enables a tool the provider runs itself, such as web search,
  including chat completions' `web_search_options`.

Bounded calls from one run run in parallel while their reservations fit. A
call that reserves all remaining capacity runs alone. Cancellation and the
model call deadline still apply while a call waits.

The gateway streams the response to the harness as it arrives, so output
reaches the harness before its usage is settled. When the call completes, the
gateway settles the provider-reported input and output tokens. Input tokens,
hidden reasoning and other output the stream doesn't show can push usage past
the reservation. Usage within the call's own reservation is charged as
reported and is never `exceeded`. When trusted usage exceeds the call's own
reservation, the output has already been delivered: the gateway audits the
call with `endReason` `exceeded` and charges `min(used, max(budget − settled −
otherReserved, reserved))`, where `otherReserved` is what other pending calls
still hold; cost is capped the same way. The account is then charged what the
provider reported or is exhausted, and later requests are refused, so
repeating an oversized request cannot spend without limit. Usage above the
budget is not recorded, so recorded usage can undercount what was spent.
Settlement caps against the budget in force when the call reserved. Lowering
a budget changes the policy, which closes the run's connections, so a call in
flight is cancelled and charged an estimate capped at its reservation. Only a
call that completes within the roughly one-second revocation window settles
against the old budget, so only it can charge above the lowered one. If the
accounting store itself fails, the reservation stays charged, which can
undercount what was spent; the gateway logs `Failed to settle model
reservation`.

A call interrupted after the provider received the whole request, by the
harness, the model call deadline, a provider failure, a header-wait timeout or
a stream that ends without usage, is charged an
[estimated charge](../../GLOSSARY.md#estimated-charge), capped at the
reservation. This includes a call that ends before its response headers: a
call that doesn't stream gets them only when the model has finished. Input is
Anthropic's exact `message_start` usage, or the request bytes ÷ 4. Output is
the larger of the visible output bytes, counted as one token each, and 250
tokens per second since the request was fully written to the provider (since
the headers only if the transport doesn't report the write), but no more than
the output bound; Anthropic's exact output from
`message_delta` replaces it once reported. The estimate can under-charge, for
example when hidden reasoning runs faster than 250 tokens a second, and can
over-charge. A response that doesn't stream and has arrived whole settles from
its usage even if the harness then leaves. A call is free only when it is
refused before inference or while it waits for its budget, when its request
never fully reached the provider, such as after a DNS, connect or TLS failure
or a cancel during the write, or when the provider returns an error response;
see [calls that aren't charged](../guides/model-provider-auth.md#calls-that-arent-charged).

The gateway charges the whole reservation when it can't estimate, or at a
budget cut. It can't estimate an
interrupted request that references input it can't size, a provider event over
the 8 MiB parse limit or, under a cost cap, an Anthropic response that
reports tokens in a category whose rate the administrator didn't set, such as a
cache write. The charge applies only in the parts of the budget that have a
limit. While an unbounded call streams, the gateway also performs a
[budget cut](../../GLOSSARY.md#budget-cut): when the call's observed input plus
its visible output bytes ÷ 4 reaches its reservation, it sends the protocol's error event,
cancels the provider call and charges the whole reservation. Elapsed time
doesn't count towards the cut, and an unbounded call whose input the gateway
can't size is never cut: it settles from provider usage, or is charged its
whole reservation if interrupted. A cost-capped call for a model with no pricing
row is refused before inference and not charged. Each charge is logged
(`Charged estimated model usage` or `Charged model reservation in full`) and
audited with `usageSource` and `endReason`. Under a cost cap, one unbounded
call can spend all remaining cost capacity. Size each cost cap to what you
would accept losing on one call, and set it per run. See
[interrupted calls](../guides/model-provider-auth.md#interrupted-calls) and
[unverifiable calls](../guides/model-provider-auth.md#unverifiable-calls).

Administrator-owned pricing converts verified usage into cost; the
[pricing rules](agentpolicy.md#endpoint-budgets) say which rows a cost limit
needs. A missing price for a category the provider charged charges the whole
reservation after the output was delivered.
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

### Gateway restarts, upgrades and rollback

For the steps of an in-place Kubernetes upgrade, see
[Upgrade](../guides/install-kubernetes.md#upgrade). The release notes name any
object a release no longer ships.

On SIGTERM the shared gateway drains: it stops accepting connections, cancels
every model call with `endReason` `gateway_restart` and waits up to 30 seconds
for them to settle, then exits. A call the provider had received is settled as
an [interrupted call](../guides/model-provider-auth.md#interrupted-calls). A
call still waiting for its budget, one whose request wasn't fully written and
one still waiting for its response headers get a 503 with `the model gateway
is shutting down; retry the call`; only the last is charged. The gateway
Deployment sets `terminationGracePeriodSeconds: 45`. A model call that arrives
during the drain gets the same 503. Its code `gateway_stopping` is in the
OpenAI body only; the Anthropic body has the type `server_error` and no code. The local service drains for up to 10 seconds
when it closes. Settles that collide on one account's ConfigMap retry for up
to 30 seconds, so a drain normally settles every call; one that still fails is
left to lease recovery, with the error `budget: ledger update conflicted
until its deadline: …`.

Every reservation is recorded in the ledger as a lease, which settling the
call removes: model calls and GitHub, package, Kubernetes, egress and MCP
requests alike. A lease outlives its call only when its gateway didn't settle
it: the gateway was killed, such as by SIGKILL or running out of memory, the
drain ran out of time, or the settle failed. A model call's lease expires at
its model call deadline plus 5 minutes, with a recovery charge of the request
bytes ÷ 4 plus the whole model call deadline at 250 output tokens a second,
capped at the output bound and the reservation; a request whose input can't
be sized is charged its whole reservation. Any other request's lease expires
at its request deadline plus 5 minutes and charges the whole reservation.
After the expiry, the next reservation or seal on the account, or a sweep,
settles the lease. A reservation or seal saves that recovery in an update of
its own first, so it persists even when the reservation or seal then fails.
Only once the recovery is saved does it log `Settled abandoned model
reservation`, or `Settled abandoned budget reservation` for a lease that isn't
a model call's, with `account`, `endReason` `gateway_restart`, `usageSource`
`estimated`, `units` and `costMicros`. The shared gateway sweeps every
[BudgetLedger](budgetledger.md) in its `BUDGET_NAMESPACE`, listing 500 at a
time, at start and every minute, which needs `list` on `budgetledgers` in
`sproozi-system`; the local service sweeps its ledger files when it opens them
and every minute. A sweep that fails logs `Failed to recover expired budget
reservations: <error>`. A BudgetLedger the sweep can't decode is skipped
with `Skipped corrupt BudgetLedger`, with `name` and `error`; the sweep
carries on and reports that error. A recovery, settle or sweep that finds the
account deleted stops without recreating it, so a deleted account stays
deleted. A late settle from the original gateway is refused; for a model call, its
audit record has `usageSource` `recovered` and `tokensUsed` 0. Until a lease expires its
reservation still counts, so an abandoned unbounded call's lease leaves the
run's later calls refused with 429, and stopping or moving its session fails
with `budget: pending usage has no finite bound`, for up to the call deadline
plus 5 minutes. An account holds at most 64 leases, so at most 64 of its calls
can be pending at once.

The first upgrade from a gateway that doesn't drain behaves as that older
gateway did when it stops. On SIGTERM it exits at once without settling its
calls in flight, and their reservations have no lease. Every
reservation a current gateway makes has a lease, so a pending reservation
without one can only come from a gateway from before leases. The upgraded
shared gateway runs a legacy pass before it serves, and the upgraded
service makes its first sweep after opening its ledger one: the pass charges
the whole reservation of every pending reservation that has no lease, as the
older gateway would have, and, once that is saved, logs `Settled legacy budget
reservation` with `account`, `endReason` `gateway_restart`, `usageSource`
`reservation`, `reservations`, `units` and `costMicros`. If the gateway's pass
fails, it logs `Failed to recover legacy budget reservations; retrying:
<error>`, serves anyway and retries with backoff from 1 second up to 1 minute
until the pass succeeds once; the local service repeats the pass on each sweep
until one succeeds. An unbounded reservation left that way therefore charges
everything its account had left, so upgrade while no model calls are in
flight.

Rolling back is a risk. Older versions of every binary that reads budget
records, the shared gateway and the remote executor (BudgetLedger resources) and
the local service (its ledger files), refuse a record that holds leases, so that
account's model calls, and the remote executor's seals, fail until a current
binary settles them. Every pending reservation a current binary makes, model
call or not, holds a lease, so a record has leases whenever such a call is in
flight or a lease was abandoned. Before rolling
back, let model calls stop, then keep the current gateway or local service
running for the model call deadline plus 5 minutes, plus a minute for its
next sweep (26 minutes at the default deadline), so the sweep settles every
abandoned lease, including those of idle accounts.

## Cleanup and state retention

Sproozi keeps a run's NetworkPolicy, ServiceAccount and contract until it sees
the Pod gone or terminal. A Pod in `Unknown` phase counts as still running. If
Sproozi can't confirm which Pod belongs to a run, cleanup waits. After an
interrupted run, check the Pod and its containment yourself. Don't remove
Sproozi finalizers by hand. A deleted Pod doesn't mean its usage was recorded.
[ADR 0014](../adr/0014-review-boundaries-and-native-contracts.md#implementation-notes)
records how release recovers ownership.

`retentionTTL` governs the AgentRun record. Budget ledgers are
[BudgetLedger](budgetledger.md) resources in `sproozi-system`. The gateway deletes a ledger once its AgentRun no longer
exists, nothing is reserved on it, and it has not been written for
`BUDGET_LEDGER_RETENTION` (default 7 days, at least 1 hour); a run that no longer exists cannot
authenticate, so its usage is final. A ledger whose account has no run UID
carries no `sproozi.com/run-uid` label and is kept. Session
registrations in run namespaces are not yet removed automatically: list them
with `kubectl get configmaps -A -l sessions.sproozi.com/registry=true` and delete those
whose AgentRun is gone and whose sessions you no longer need to stop or
resume. See
[ADR 0041](../adr/0041-gateway-state-namespace-and-retained-state-owners.md) and
[ADR 0045](../adr/0045-budget-ledgers-are-a-custom-resource.md).

## Restrict trusted services

The installed controller-manager reads Pods, ServiceAccounts, NetworkPolicies
and ConfigMaps across the cluster. It creates and deletes Pods, ServiceAccounts,
NetworkPolicies and ConfigMaps only in `sproozi-agents`, through the Role in
`config/agent-rbac`. It cannot read Secrets through the API, but a Pod it
creates can mount any Secret in `sproozi-agents`, so keep Secrets out of that
namespace except the harness sign-in Secrets that AgentRuntimes name in
`clientConfig.signIn`. Every run of such a runtime receives that sign-in, and
a compromised controller could mount it, so use a token scoped to that use. The optional metrics overlay adds TokenReviews and
SubjectAccessReviews. Restrict exec/debug access
and reuse of trusted service accounts. The gateway holds gateway-held provider
credentials and may update its rotating ChatGPT credential Secret. ChatGPT refresh requires one active gateway process,
even though budget state is shared. See
[Model provider authentication](../guides/model-provider-auth.md).

The remote executor example's Roles are not limited to its own runs. In
`sproozi-agents` it can get, patch and delete any Pod and exec into it, and in
`sproozi-system` it can update any BudgetLedger, though no ConfigMap. A compromised executor therefore
holds the authority of every Kubernetes run in the cluster. Run it only where
all runs share one trust domain, and treat its ServiceAccount like the
controller's ([ADR 0041](../adr/0041-gateway-state-namespace-and-retained-state-owners.md)).

The gateway's only plain HTTP listener is its health port, 8081, for the
kubelet readiness probe. It serves `GET /readyz` with a fixed `ok` body and
nothing else. The `sproozi-gateway` Service exposes only 8443, and no installed
NetworkPolicy admits ingress to 8081, so `default-deny-ingress` keeps other
Pods from reaching it. The kubelet probes it from the Pod's node.
`kubectl port-forward` reaches a Pod directly through the API server and the
kubelet, so NetworkPolicy doesn't apply to it: anyone allowed to create
`pods/portforward` in `sproozi-system` can reach 8081, and every other port of
the gateway and controller. Grant `pods/portforward` there only to
administrators.

The local demo's short-lived CA and gitignored environment file are disposable
evaluation inputs. For deployment, supply managed certificates, least-privilege
credentials, namespace isolation and durable audit collection. The [Home-ops playbook](../guides/home-ops-playbook.md) describes a planned
integration; Sproozi has no event receiver, so its runs are created by hand.


## Persistent assistant permissions and answers

The Hermes coordinator is a separate trusted workload. It keeps its own model
and optional channel credentials and persistent state. It has no Kubernetes
API token; the private task service holds namespaced Run create/observe/patch
permission and fixes the templates and capability subsets exposed to that
installation. Use separate task principals and bearer tokens for assistants
requiring different authority. A shared bot token does not authenticate each
channel user independently.

An agent's final text is untrusted and can contain sensitive scoped tool results. It
is retained in AgentRun status, visible to authorized Run readers, and expires
with Run retention. Do not treat it as operating instructions or grant more
permissions to a later task because of answer text. Coordinator state has
separate retention and spending limits. See [orchestration](orchestration.md)
for the exact retry and result bounds.
