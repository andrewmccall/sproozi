# Permissions and capabilities

**Audience:** operators, contributors

Sproozi permissions are easiest to understand from the perspective of a **capability request**, not from raw RBAC alone.

## Capability matrix

| Capability | Requested by | Also gated by | Enforced by | Result |
| --- | --- | --- | --- | --- |
| `kubernetes.read` | `AgentRun.spec.capabilities` | `AgentPolicy.kubernetesRead` | shared gateway Kubernetes module using its bounded read identity | read-only cluster investigation |
| `github.pull_request` | `AgentRun.spec.capabilities` | `AgentPolicy.githubPullRequest` | shared gateway GitHub module with GitHub App credentials | PR publication against approved repos |
| `model.inference` | `AgentRun.spec.capabilities` | optional `AgentPolicy.budgets[model.inference]` | shared gateway model module | model access with reservation and usage accounting; current ceiling limitations apply |
| `packages.install` | `AgentRun.spec.capabilities` | `AgentPolicy.packagesInstall` ecosystem, artifacts, SHA-256 digests and dependencies | shared gateway PyPI module | exact locked wheel downloads |
| `mcp.<server>` | `AgentRun.spec.capabilities` | `AgentPolicy.mcpServers[server].tools` and optional argument schemas | configured remote MCP bridge, with gateway-only provider credentials | approved tools and bounded argument scope, Protocol tier |
| `network.egress` | `AgentRun.spec.capabilities` | `AgentPolicy.egressProfiles` and `AgentTemplate.egressProfiles` | shared gateway egress module | exact outbound destinations only |

## Kubernetes read scope

The workload ServiceAccount has no Kubernetes API grant. The shared gateway's
resource-read permissions for agent investigation are:

- `get`, `list`, `watch`
- core resources: `pods`, `pods/log`, `events`, `services`
- apps resources: `deployments`, `replicasets`, `statefulsets`

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

## Why capabilities are not just credentials

Capabilities describe **authorized intent**, layered through containment, run
identity, immutable requested capabilities, live policy/lifecycle checks, semantic
authorization, and upstream-native restrictions. The sandbox does not receive
provider credentials directly. It asks the shared gateway to perform bounded
work on its behalf.
