# AgentRuntime

`AgentRuntime` defines how the agent runs. An administrator sets container
images, resources, commands and security settings in `podTemplate.spec`. Images
must be pinned by digest.

Ordinary runs use the configured command. For managed native sessions, the
controller sets fixed supervisor and checkpoint-helper commands. Resources and
security settings live in `podTemplate.spec`; there are no separate top-level
fields for them.

```yaml
spec:
  podTemplate:
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: agent
        image: registry.example/agent@sha256:<digest>
        resources:
          limits:
            cpu: "1"
            memory: 1Gi
            ephemeral-storage: 2Gi
        securityContext:
          runAsNonRoot: true
          readOnlyRootFilesystem: true
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
          seccompProfile:
            type: RuntimeDefault
  workloadContainers: [agent]
  gatewayEndpoint: https://sproozi-gateway.sproozi-system.svc:8443
  clientConfig:
    trustBundleConfigMap:
      name: sproozi-sandbox-trust
      key: ca-bundle.pem
  ephemeralWorkspace:
    sizeLimit: 2Gi
```

Admission sums the actual limits across every container, including sidecars,
and compares the total with `AgentPolicy.spec.resourceBounds.max`. Each container
must declare a positive limit for every resource bounded by policy. The
workspace size is also checked against the ephemeral-storage bound.

The controller copies this template, binds the run identity, and injects the
contract, public trust material and one `sproozi-gateway` token into the selected
workload containers. Every capability uses that identity and HTTPS proxy
endpoint. Client-specific destination URLs belong in ordinary client settings.
No additional egress token or local proxy process is injected.

`spec.gatewayEndpoint` names the gateway as an `https` URL whose host the
gateway's TLS certificate covers. Sandboxes resolve no names, so the controller
resolves that host itself when it creates each Pod and writes the address into
the Pod's hosts file. The host must therefore resolve from the controller, not
only from Pods. Keep it a host name: managed harnesses also use it as the
authority of their control connection, which the gateway serves only by name.
The address is fixed for the Pod's life; a gateway Service recreated with a new
cluster IP reaches only new runs. See [ADR 0040](../adr/0040-sandboxes-reach-the-gateway-without-dns.md).

`AgentRun` cannot change the template, image, environment or security settings.
Containers share the Pod's network boundary; a helper that needs different permissions needs its own
workload. The managed checkpoint helper below receives no workload token and
has a separate private control volume.

## Container validation rules

Admission rejects a runtime unless every container, including helpers:

- drops `ALL` Linux capabilities and adds none;
- uses RuntimeDefault seccomp;
- disables privilege escalation;
- uses a read-only root filesystem.

Admission also rejects:

- init and ephemeral containers;
- shared process namespaces;
- `envFrom` and environment `valueFrom.secretKeyRef`;
- template volumes other than one `emptyDir` with a positive `sizeLimit`, one
  `configMap` or one `downwardAPI` source. Secret, projected-token, persistent,
  host and other sources are rejected.

A literal environment value or an explicit public ConfigMap key reference is
allowed. Keep credentials out of both. Only the controller adds the gateway
token and managed-session volumes.

If a custom runtime used a Secret mount or Secret environment reference, move
that credential into the gateway's configuration. Replace the mount with public
configuration or bounded scratch space. Don't paste the credential in as a
literal value.

## Runtime class

Optional `spec.runtimeClassName` sets the Pod's `RuntimeClass`, for example a
gVisor or Kata Containers class that your cluster provides. Empty uses the
cluster default.

The trust ConfigMap must be in `sproozi-agents`, immutable and contain public CA
material only. Runtime, policy, template and run references resolve within the
run's namespace.

These rules are checked when a run is admitted, not when the `AgentRuntime` is
created, and again on every later reconcile of an active run. A runtime that breaks
any of them fails the run with reason `RuntimeInvalid` before it is admitted,
so it never counts against a concurrency cap. The `Ready` condition message names the runtime and the rule, for
example `AgentRuntime codex: cannot launch a sandbox: container "agent" image
must be digest pinned`. Fix the runtime, then create a new run.

## Ordinary run completion

For the current adapter, use one workload container named `agent`. Validation
accepts other names, but completion reads only `agent` after the whole Pod
becomes terminal. Long-running ordinary sidecars prevent completion.

Run the CLI in one-shot mode and replace the shell with `exec`. The container's
actual exit code determines success or failure; review the proposed PR separately.
See [agent contract and completion](agent-contract.md).

The adapter creates ordinary disposable Pods. See
[dependencies](dependencies.md) for what a cluster needs.

## MCP client preparation

Optional `spec.clientConfig.harness` selects one of the [harness selectors](harnesses.md#harness-selectors)
native MCP configuration in the immutable contract. The administrator-owned
launch command loads only that configuration into disposable client state.
Only requested named MCP capabilities appear; credentials remain in the gateway.
The [harness reference](harnesses.md) describes formats, trusted launch examples,
model-provider setup and supported configurations.

## Harness sign-in

Optional `spec.clientConfig.signIn` gives the harness its own provider sign-in,
for a gateway that forwards it (`MODEL_AUTH_MODE=workload` with no key or
token for that provider; see
[ADR 0047](../adr/0047-harnesses-sign-in-to-their-own-model-providers.md)).

Set exactly one source:

| Field | Meaning |
| --- | --- |
| `secretName` | A Secret in `sproozi-agents` holding the sign-in. You create it from a credential the provider issued you, such as the output of `claude setup-token`. |
| `key` | Required with `secretName`. The controller mounts that key read-only at `/etc/sproozi/sign-in/<key>` in each workload container. For a managed session, name it after the harness's login file (`.credentials.json` or `auth.json`). |
| `env` | Optional, with `secretName` only. Also sets the sign-in as this environment variable, such as `CLAUDE_CODE_OAUTH_TOKEN`. Variables the controller sets itself, such as `SSL_CERT_FILE` or `HTTPS_PROXY`, are refused. |
| `claimName` | A PersistentVolumeClaim in `sproozi-agents` that keeps the harness's own login across managed sessions. The controller mounts it read-write at `/etc/sproozi/sign-in`. |

In a managed session the supervisor copies the harness's login file
(`.credentials.json` for Claude Code, `auth.json` for Codex) from
`/etc/sproozi/sign-in` into its native home before launch. With no file there
yet, sign in inside the session as you would locally (`/login`, or Codex's
device-code sign-in). When the harness exits, the supervisor writes the
refreshed file back to a claim; a Secret stays as it is, so a Secret suits a
token that doesn't refresh. The login never enters a checkpoint. Give the claim
a filesystem the agent user can write, for example through the Pod's
`fsGroup`, and keep one claim per person: two sessions sharing one refreshing
login can invalidate each other.

With `signIn`, workload containers don't receive the `OPENAI_API_KEY`
placeholder, which most clients would prefer over their own sign-in. A client
that needs a writable credential file, such as Codex's `auth.json`, copies the
mounted file into its home in the runtime's command. The gateway still
inspects and counts every model call, and refuses the forwarded credential on
any other inspected route. The agent can read its own sign-in.

```yaml
spec:
  clientConfig:
    harness: claude-code
    signIn:
      secretName: claude-sign-in
      key: token
      env: CLAUDE_CODE_OAUTH_TOKEN
```

For a managed Codex or Claude Code session that keeps its own sign-in:

```yaml
spec:
  clientConfig:
    harness: codex
    signIn:
      claimName: alice-codex-login
```

## Model selection

Each AgentRuntime chooses its own model; there is no cluster-wide model
setting, and AgentPolicy budgets limit usage rather than naming a model. An
ordinary run's trusted launch command sets the model, for example
`codex exec --model` in the [SRE demo runtime](../../examples/sre-demo/agentruntime.yaml)
or the `SPROOZI_MODEL` environment variable in the
[stock harness examples](../../examples/harnesses/README.md). A managed native
session uses `clientConfig.nativeSession.model`, described below. A policy with
a positive `maxCostMicros` also needs a row for that exact model in the gateway's
[pricing table](required-secrets-and-config.md#model-pricing-format).

The examples use different models on purpose. The SRE demo pins `gpt-6.1-sol`
because its ChatGPT sign-in helper checks access to that exact model before
switching the demo to ChatGPT mode, with no fallback; see
[Model provider authentication](../guides/model-provider-auth.md). The other OpenAI examples use `gpt-5.3-codex` as a starting value,
and the Claude Code example uses `claude-sonnet-4-5-20250929`. The installed
pricing table has rows for all three. Replace any of them with a model your
provider account can use, and add a pricing row for it if a policy caps cost.

## Managed native sessions

Managed sessions require explicit `clientConfig.harness: codex` or `claude-code`.
Selecting OpenCode or Hermes does not make those harnesses movable. The
repository's client images pin their releases in
[`hack/clients`](../../hack/clients/README.md). Sproozi checks supported native state and
completion records without requiring an exact client version. A move keeps the
same harness.

`clientConfig.nativeSession.model` selects the administrator's native model and
is bounded to 128 bytes without NUL or line breaks. Empty uses the stock default.
Codex also accepts `nativeSession.options`: at most 32 single-line
`key=TOMLvalue` entries, each at most 4096 bytes. Keys use validated dotted bare
or quoted segments; approved instructions and the update gate remain reserved.
Claude rejects Codex options. These settings come from the approved current
runtime, never a portable checkpoint. For example:

```yaml
spec:
  clientConfig:
    harness: claude-code
    nativeSession:
      model: claude-sonnet-4-5
    trustBundleConfigMap:
      name: managed-native-trust
      key: ca-bundle.pem
```

This fragment supplements a complete managed runtime, which must have:

- exactly two containers, named `agent` and `checkpoint`, with
  `workloadContainers: [agent]`;
- non-root UID and GID 1000, read-only root filesystems and dropped capabilities;
- explicit resources and a bounded memory-backed workspace.

The controller sets the supervisor and checkpoint-helper commands. The helper
mounts the workspace and a private temporary volume. It gets no provider
credentials, token mounts or harness environment. Pod completion alone
does not end a managed [visit](../../GLOSSARY.md#visit); Sproozi also checks
stop, usage and restore.

The managed template carries `sessions.sproozi.com/managed: "true"`. Each visit
gets fresh admission and gateway access. Trusted launch configuration supplies
the approved instructions on every visit. Sproozi checks the initial developer
instructions saved in the Codex conversation against the approved digest. Claude Code receives fresh
instructions, foreground Bash, fixed completion hooks, permission mode and
settings; its transcript cannot grant any of them. Both harnesses rely on the
container and the enforced gateway network for isolation.

### Checkpoints

A checkpoint records the harness, its selected native state and the Git
workspace. Restoring one without starting an agent creates
`/workspace/session/workspace` and `/workspace/session/codex` or
`/workspace/session/claude`. It carries no gateway credentials, launch
configuration or personal native home.

Before Sproozi captures a checkpoint or starts the destination, the placement
must have:

1. the harness's acknowledgement that the turn and its tools finished;
2. revoked the visit's gateway access;
3. confirmed that the container stopped;
4. recorded finalized usage.

A native completion hint is not enough. If a Claude Code acknowledgement is
lost, a graceful move stays unavailable even when the container has stopped and
the transcript looks complete; `cancel` still works.

Supported native state covers foreground tools. Background tasks, subagents and
compaction are not supported. [ADR 0008](../adr/0008-selected-native-and-git-checkpoints.md)
and [ADR 0011](../adr/0011-native-session-compatibility-and-completion.md)
record the checkpoint format, legacy decoding and transcript edge cases.

See also the [profile setup](../../cmd/session-profile/README.md),
[supervisor contract](../../cmd/native-session/README.md),
[native launch and control](native-control.md) and
[checkpoint helper](../../cmd/checkpoint/README.md).
