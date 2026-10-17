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
| `controller-manager` | manage Run status, Pods, ServiceAccounts, NetworkPolicies and RBAC objects; read Pod logs and runtime/policy/template objects; write contract ConfigMaps in `sproozi-agents`. The optional metrics overlay adds TokenReviews and SubjectAccessReviews |
| `shared-gateway` | `tokenreviews`, read ServiceAccounts and run/policy/template objects, bounded Kubernetes reads, namespaced budget state, mounted provider credentials, and get/update of the rotating ChatGPT session Secret |
| `webhook` | read HMAC Secrets, create `AgentRun`, read policy/template objects, and get/create/delete replay ConfigMaps in `sproozi-webhook-state` |

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
