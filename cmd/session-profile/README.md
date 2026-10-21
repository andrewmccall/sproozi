# Remote executor (`session-profile`)

`session-profile` serves one administrator-approved Kubernetes placement over
authenticated TLS. In a cluster, the developer reaches it through a kubeconfig
[context](../../GLOSSARY.md#context), signed in as the `sproozi-cli`
ServiceAccount; see [remote executor access](../../docs/reference/remote-executors.md).
A host-run executor instead gives the developer its fixed HTTPS URL, CA
certificate and [home principal token](../../GLOSSARY.md#home-principal-token)
file. Kubernetes credentials for the placement remain with the remote executor.
See [Move a live session between Docker and Kubernetes](../../docs/guides/session-mobility.md) for the
local/remote command sequence. Both supported harnesses have passed production
Docker-to-Kubernetes-to-Docker round trips through this profile. Codex's extended
proof also covers held-turn movement, forced cancellation and denial probes;
Claude's full round trip establishes foreground Bash, workspace continuity and
accounting. See the [repeatable fixture setup](../../hack/verify/session-mobility/README.md)
for the separate verification commands and their scope.

Build from the repository root:

```sh
go build -o bin/session-profile ./cmd/session-profile
```

## Prepare the cluster

Install the current generated CRDs, controller and shared gateway using
[Install on Kubernetes](../../docs/guides/install-kubernetes.md). For an isolated
Kind evaluation, use the enforcing CNI setup in
[the investigation-to-PR demo](../../docs/guides/pr-demo.md). Managed sessions require
the controller and gateway from this change; an older ordinary-run deployment
does not provide managed registration or active-allocation checks.

The administrator creates an `AgentRuntime`, `AgentPolicy` and `AgentTemplate`
in the profile's fixed namespace. The worker namespace is `sproozi-agents`.
The runtime selects an explicit native client: stock Codex (`codex`) or
Claude Code (`claude-code`), with Git, `native-session` and `checkpoint`
from the approved [native image](../../internal/runtime/docker/image/Dockerfile).
Use an immutable `repository@sha256:...` image reference available on cluster
nodes. Configure exactly two containers named `agent` and `checkpoint`, with
`workloadContainers: [agent]`, and `clientConfig.harness` matching the profile
configuration below. The administrator selects `clientConfig.nativeSession.model`.
Codex also accepts its validated `options`; Claude rejects that field. A missing
harness or another client kind is rejected. The
checkpoint retains the selected kind, and both placements must use it throughout
the session.
This two-client contract is separate from the four ordinary run harness selectors.
OpenCode and Hermes runtimes cannot become managed destinations through their
ordinary launch support. Activation uses the installed executable;
checkpoint metadata does not replace that observation.

Set `automountServiceAccountToken: false`, non-root UID/GID 1000, RuntimeDefault
seccomp, a read-only root filesystem and dropped capabilities. Give both
containers explicit CPU, memory and ephemeral-storage requests and limits.
Select a bounded `ephemeralWorkspace.sizeLimit`. The managed controller uses
memory-backed workspace and helper temporary volumes to bound extraction storage.
It sets the fixed supervisor/helper commands itself. The helper cannot have
environment variables, credential mounts, volume devices, lifecycle hooks or
ports; it receives only the workspace and its private temporary mount.

The runtime's `gatewayEndpoint` selects the shared HTTPS gateway. Its
`clientConfig.trustBundleConfigMap` selects an immutable certificate-only
ConfigMap in `sproozi-agents`. The gateway certificate must cover its Service DNS
and approved inspected model authority, including `api.openai.com` for Codex or `api.anthropic.com` for Claude. Provider
credentials belong to the shared gateway; configure them through
[Model provider authentication](../../docs/guides/model-provider-auth.md).
Give changed trust bytes a new immutable ConfigMap name and update the gateway
and runtime references. The fixture renderers derive that name from the CA hash.
Deleting and recreating an immutable ConfigMap at the same name can leave a
cached mounted certificate; API contents alone do not prove a process loaded it.

The template must have `sessions.sproozi.com/managed: "true"` and the exact same
approved instruction bytes as the local placement. Compute their SHA-256 for
`instructionDigest`. The policy allows the selected capabilities, finite positive
unit budgets, compatible cost budgets, runtime resource limits and an execution
duration covering the session's original deadline. Current Kubernetes policy
duration is at most one hour. A destination rejects an incompatible allowance or
deadline; it never silently rewrites immutable move intent.

The source schemas are
[AgentRuntime](../../api/v1alpha1/agentruntime_types.go),
[AgentPolicy](../../api/v1alpha1/agentpolicy_types.go) and
[AgentTemplate](../../api/v1alpha1/agenttemplate_types.go).
The credential-free evaluation
[renderer](../../hack/verify/session-mobility/kubernetes.py) supplies a complete
managed example from the private hints produced by
[host.py](../../hack/verify/session-mobility/host.py). It fixes a public placeholder
provider token and deterministic fixture upstream. Its YAML contains public CA
material; create the gateway TLS Secret separately from private files. It does
not deploy the cluster or establish completed acceptance. The separate
[Claude renderer](../../hack/verify/session-mobility/claude.py) creates its selected
runtime/template/trust resources and updates the gateway in the same dedicated
cluster after the Codex evaluation has stopped.

## Give the service its own authority

The [retained-work Kubernetes example](../../examples/portable-workflows/kubernetes/owner.yaml)
implements these namespace-scoped roles with a dedicated identity, PVC and TLS
Service. [Hermes with remote work](../../docs/guides/retained-work-kubernetes.md)
includes the managed runtime, distinct task/home credentials and Hermes setup.

For a host-run profile, select an administrator-owned kubeconfig through
`KUBECONFIG`. For an in-cluster profile, use its dedicated ServiceAccount and
the normal in-cluster client configuration. Required backend permissions are:

| Namespace | API resources | Verbs |
| --- | --- | --- |
| Configured Run namespace | `sproozi.com/agentruns` | `get`, `create`, `patch` |
| Configured Run namespace | `sproozi.com/agenttemplates`, `agentpolicies`, `agentruntimes` | `get` |
| Configured Run namespace | Core `configmaps` for registrations, accounts and stored profiles | `get`, `create`, `update` |
| `sproozi-agents` | Core `pods` | `get`, `patch`, `delete` |
| `sproozi-agents` | Core `pods/exec` | `create` |

With `"homeAuthorization": "kubernetes"`, also bind the profile's identity to
the `system:auth-delegator` ClusterRole, so it can create TokenReviews and
SubjectAccessReviews.

Bind these as namespace-scoped roles to the profile's identity. The existing
controller and gateway retain their own deployment RBAC. In particular, the
gateway needs registration/account ConfigMap access in the Run namespace,
as provided by the shared-gateway budget Role. Worker ServiceAccounts receive
neither profile credentials nor Kubernetes API authority.

## Create private configuration

Create an owned private configuration directory outside all worker workspaces.
Use mode `0700` for directories and `0600` for files, including the token, TLS key
and kubeconfig. Generate the home principal token directly into a protected file:

```sh
umask 077
export SPROOZI_PROFILE_DIR=/absolute/private/profile
mkdir -p "$SPROOZI_PROFILE_DIR"
chmod 700 "$SPROOZI_PROFILE_DIR"
openssl rand -hex 32 > "$SPROOZI_PROFILE_DIR/home-principal-token"
```

Issue a TLS server certificate covering the host/IP in the developer's configured
URL, with its chain and key available to this service. Give the developer the
trusted CA and token through your credential distribution process. Do not paste
provider credentials or private keys into chat or place them in the repository.
The endpoint uses CA-verified TLS 1.3, without redirects, environment proxies or
cookies. It exposes fixed lifecycle and terminal operations, with no arbitrary
execution or resource-configuration endpoint.

Save the following strict JSON as `profile.json`. Replace the example paths,
template, instruction digest and approved ceilings. The 30-minute duration and
unit ceiling match the [home example](../sproozi/README.md); cost is explicitly
unbounded here. A bounded positive cost requires corresponding gateway pricing.

```json
{
  "harness": "codex",
  "listen": "127.0.0.1:18444",
  "certificateFile": "/absolute/private/profile/server.crt",
  "keyFile": "/absolute/private/profile/server.key",
  "tokenFile": "/absolute/private/profile/home-principal-token",
  "principal": "developer",
  "placementName": "engineering",
  "namespace": "sproozi-system",
  "templateName": "mobility-native",
  "directory": "/absolute/private/profile/state",
  "instructionDigest": "REPLACE_WITH_64_LOWERCASE_HEX",
  "allowances": {
    "model.inference": {
      "units": {"bounded": true, "value": 200000},
      "costMicros": {"bounded": false, "value": 0}
    }
  },
  "maxDuration": "30m",
  "maxCheckpointBytes": 8388608
}
```

`homeAuthorization` selects who the home principal is. `"token"`, the default,
accepts the one token in `tokenFile`. `"kubernetes"` requires `tokenFile` to be
omitted and accepts any bearer token that a TokenReview authenticates and that
Kubernetes RBAC allows to `use` `remoteexecutors.sproozi.com` named
`placementName` in `namespace`, the rule `config/cli-access` grants the
`sproozi-cli` ServiceAccount. Removing that RoleBinding revokes access on the
next request.

For an in-cluster executor reached through a context, also label its Service
`sproozi.com/remote-executor: "true"` with a port named `https`, issue its
serving certificate for `SERVICE.NAMESPACE.svc`, and publish the issuing CA in
the ConfigMap `sproozi-remote-executor` under `ca.crt`. One executor per
namespace. Home learns the executor's `placementName`, harness and
`maxCheckpointBytes` from its authenticated `describe` request; the checkpoint
limit must equal home's `local.maxCheckpointBytes`.

Use `127.0.0.1:18444` for a same-host evaluation. A hosted remote service needs a
reachable fixed listener, a matching certificate and host firewall rules. Its
`placementName` must match the alias in the home configuration, and both sides
must use the same checkpoint byte limit. Run one process for this principal and
state directory; the owner lock rejects competing profile instances.

```sh
export KUBECONFIG=/absolute/private/profile/cluster.kubeconfig
bin/session-profile --config "$SPROOZI_PROFILE_DIR/profile.json"
```

Keep this process under a service manager. Restart with the same private directory
and fixed configuration to recover retained registrations; do not delete them to
make an uncertain move proceed.

## Stop, recover and cancel

The local service drives this profile; use the `sproozi` CLI instead of hand-crafted
HTTP calls. Each visit gets a fresh Run and Pod. Restore is inert until exact
admission succeeds and initialized budget accounts exist. Actual checkpoint
UUID, artifact kind and digest must match trusted intent. The
registration retains the approved instruction digest independently of native
history; Claude transcripts do not themselves establish which instructions were
approved. A checkpoint never selects a provider credential or runtime profile.
Fresh managed Claude settings fix foreground Bash, permission mode and completion
hooks independently of history. Codex's original developer instructions must
match the approved digest. Native kind and instruction digest remain invariant
through every visit; model configuration belongs to the trusted current launch.

The Claude compatibility subset includes the observed bounded recovered 429
record followed by a successful native assistant response. An unresolved retry
cannot reuse an earlier assistant leaf; other native error shapes, background
tasks, subagents and compaction are unsupported. The transcript is continuity
data, and its error text supplies neither authority nor accounting. See
[ADR 0011](../../docs/adr/0011-native-session-compatibility-and-completion.md).

Graceful stop gates input and waits for the active completed turn and supported
tools while gateway access remains admitted. It then revokes access, observes the
exact native container terminated, seals trusted usage and captures the pair
through the surviving helper. The trusted receipt and checkpoint commit precede
the next activation. Cancel revokes first and forces the exact Pod stop/deletion;
it preserves the previous checkpoint and leaves unresolved usage encumbered.

Native write/resize attachments use the fixed terminal protocol. A disconnect
ends the attachment, leaving the native writer running; reconnect after an
attachment byte limit. Network errors, lost replies or incomplete accounting do
not establish absence, zero usage or a stopped writer. Recover the same operation
through the home journal. See the guide's
[recovery rules](../../docs/guides/session-mobility.md#recover-or-cancel) and
[ADR 0010](../../docs/adr/0010-managed-session-authority-and-admission.md).

## Retained remote work

To accept new Codex tasks independently of the local service, add `workTokenFile`
and `repositories` to the configuration. The work token must be distinct from
`tokenFile`, at least 32 characters, and stored privately. It is checked first;
any other bearer token is decided by `homeAuthorization`. Repository entries
select either an administrator-owned absolute stopped `snapshot` path or an
approved GitHub `url` with an optional `ref`. URL fetching uses the worker's
separately scoped GitHub capability; the trusted profile never imports a worker's
credential configuration.

`/work/mcp` exposes `sproozi_run`, `sproozi_status`, `sproozi_cancel` and
`sproozi_sessions` to Hermes. Fixed JSON routes also support recovery. The home principal additionally permits checkpoint export and the
exclusive one-way claim; the work token cannot claim ownership. TLS and bearer
validation happen before request decoding.

The protected `directory/work` retains original request/execution identities,
source receipts, bounded native/Git checkpoints and untrusted final text. Run one
remote executor per state directory, with durable storage. Restart with the same principal and directory;
changing principals cannot reinterpret retained records. A lost preparation or
stop reply requires recovery of that exact allocation. Missing stop/accounting
evidence blocks export, and changing a request's intent on replay is rejected.

The native image includes `/usr/local/bin/session-profile` and Git for this
trusted service; the scratch controller image does not. A trusted Pod deployment
needs private persistent storage, TLS/token files and the profile RBAC above.
See [the full workflow](../../docs/guides/portable-workflows.md) and
[its isolated fixture](../../hack/verify/portable-workflows/README.md).

## Projects and exact revisions

See the [configuration and caller reference](../../docs/reference/projects-and-revisions.md) for optional
Project grouping, installation membership, independent local approval and fresh
workspace-only execution at a retained Git revision. Standalone work remains
valid. Native resume still restores the complete supported unfinished workspace.
