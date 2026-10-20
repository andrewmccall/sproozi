# Permissions and capabilities

A run requests capabilities. Policy sets their scope, and the gateway checks
each operation. Kubernetes RBAC limits the trusted services separately. See
[capabilities and gateways](../explanations/capabilities-and-gateways.md) for
the model behind these checks.

## Capability matrix

| Capability | Requested by | Also gated by | Enforced by | Result |
| --- | --- | --- | --- | --- |
| `kubernetes.read` | `AgentRun.spec.capabilities` | `AgentPolicy.spec.kubernetesRead` | shared gateway Kubernetes module using its bounded read identity | read-only cluster investigation |
| `github.pull_request` | `AgentRun.spec.capabilities` | `AgentPolicy.spec.githubPullRequest` | shared gateway GitHub module with GitHub App credentials | PR publication against approved repos |
| `model.inference` | `AgentRun.spec.capabilities` | optional `AgentPolicy.spec.budgets[model.inference]` | shared gateway model module | model access with reservation and usage accounting; current ceiling limitations apply |
| `packages.install` | `AgentRun.spec.capabilities` | `AgentPolicy.spec.packagesInstall` ecosystem, artifacts, SHA-256 digests and dependencies | shared gateway PyPI module | exact locked wheel downloads |
| `mcp.<server>` | `AgentRun.spec.capabilities` | `AgentPolicy.spec.mcpServers[server].tools` and optional argument schemas | configured remote MCP bridge, with gateway-only provider credentials | approved tools and bounded argument scope, Protocol tier |
| `network.egress` | `AgentRun.spec.capabilities` | `AgentPolicy.spec.egressProfiles` and `AgentTemplate.spec.egressProfiles` | shared gateway egress module | exact outbound destinations only |

## Kubernetes read scope

The workload ServiceAccount has no Kubernetes API grant. The shared gateway's
resource-read permissions for agent investigation are:

- `get`, `list`, `watch`
- core/v1 resources: `pods`, `pods/log`, `events`, `services`
- apps/v1 resources: `deployments`, `replicasets`, `statefulsets`

The group/version/resource combination is part of authorization. A matching
resource name under another API group or version is denied. Only `pods/log` is
an allowed subresource. Discovery is limited to the supported core/apps v1 paths.
The handler removes all `Impersonate-*` headers and refuses redirects so the
forwarded operation stays bound to the authorized target.

Query parameters are bounded:

| Parameter | Allowed | Denial body (HTTP 403) |
| --- | --- | --- |
| `timeoutSeconds`, for example on a watch | 1 to 300 | `kubernetes request denied: timeoutSeconds must be an integer from 1 to 300` |
| `limit` on a list | 1 to 5000 | `kubernetes request denied: limit must be an integer from 1 to 5000` |
| `continue` | at most 2048 characters | `kubernetes request denied` |
| `labelSelector`, `fieldSelector` | at most 2048 characters each | `kubernetes request denied` |

Named watches are not permitted; watch a collection with a field selector
instead.

Each forwarded read has a time limit. Every read ends at the request
deadline, 10 seconds before the
[inspected connection](../../GLOSSARY.md#inspected-connection) that carries it
closes, so no read lasts more than 4 minutes 50 seconds (290 seconds) from
the CONNECT, and less on a connection the harness opened earlier. A watch
therefore lasts its `timeoutSeconds` (1 to 300) or until the request deadline,
whichever comes first, so in practice at most about 290 seconds. A watch
without `timeoutSeconds` is treated as 300 seconds and so also ends at the
request deadline. The API server normally ends a watch at its
`timeoutSeconds`; if it hasn't, the gateway closes it 5 seconds later. A get,
list or log read has 120 seconds to complete, so follow logs with repeated
reads rather than one long stream. When the request deadline ends a watch, the
client sees it end, as it would at `timeoutSeconds`.

The gateway forwards at most 8 MiB of each response body. It has already sent
the upstream status and headers by then, so it can't send a denial. A chunked
response, which includes every watch, then ends at 8 MiB as if it were
complete: the client sees a truncated list body, which fails to decode, or a
watch that ended, not an HTTP error. A response with a `Content-Length`
instead closes the connection early, which the client reports as an
unexpected end of file. Use `limit` and `continue` to page large lists, and
`timeoutSeconds` to end watches before they reach the cap.

The investigation route does not authorize:

- `secrets`
- `configmaps`
- `pods/exec`
- `pods/attach`
- `pods/portforward`

The shared gateway additionally authorizes each normalized `kubectl` operation
against the run's resolved namespace and resource scope; direct API-server
access is not a supported path.

## Trusted service identities

| ServiceAccount | Main permissions |
| --- | --- |
| `controller-manager` | cluster-wide: manage Run status; read runtime/policy/template objects; get, list and watch Pods, ServiceAccounts and NetworkPolicies; get ConfigMaps. In `sproozi-agents` only (`config/agent-rbac`): create and delete Pods, ServiceAccounts and contract ConfigMaps, and create, update and delete NetworkPolicies. The optional metrics overlay adds TokenReviews and SubjectAccessReviews |
| `shared-gateway` | `tokenreviews`, read ServiceAccounts and run/policy/template objects, bounded Kubernetes reads, get, list, create and update of budget ConfigMaps in `sproozi-system` (`list` for the minute-by-minute sweep of expired leases), mounted provider credentials, and get/update of the rotating ChatGPT credential Secret |
| `webhook` | create `AgentRun` and read policy/template objects in `sproozi-system`, with no Secret access there; get Secrets only in `sproozi-webhooks`; and get/create/list/delete replay ConfigMaps in `sproozi-webhook-state` |

## Current enforcement caveats

- each run has a dedicated ServiceAccount and gateway-audience token; the gateway must bind namespace, name, and UID to the active run
- `github.pull_request` should be paired with GitHub branch protection or rulesets; the proxy should not be treated as a generic protected-branch engine
- `model.inference` reservations survive gateway restarts, but output-only admission estimates can undercount total consumption; token and cost limits are not hard spending ceilings
- for operator compensating controls, see [Security hardening](security-hardening.md)

## Capabilities and credentials

A capability names an operation the run requests. It does not contain provider
credentials or bypass other checks. The sandbox asks the gateway to perform
approved work. The gateway checks run identity, capabilities, current policy,
lifecycle and operation scope. Network and upstream restrictions also apply.
