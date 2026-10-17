# AgentRuntime

`AgentRuntime` defines how the agent runs. An administrator sets container
images, resources, commands and security settings in `podTemplate.spec`. Images
must be pinned by digest.

Ordinary runs use the configured command. For managed native sessions, the
controller sets fixed supervisor and checkpoint-helper commands. There are no
separate top-level resource or security fields.

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
and compares the total with `AgentPolicy.resourceBounds.max`. Each container
must declare a positive limit for every resource bounded by policy. The
workspace size is also checked against the ephemeral-storage bound.

The controller copies this template, binds the run identity, and injects the
contract, public trust material and one `sproozi-gateway` token into the selected
workload containers. Every capability uses that identity and HTTPS proxy
endpoint. Client-specific destination URLs belong in ordinary client settings.
No additional egress token or local proxy process is injected.

`AgentRun` cannot change the template, image, environment or security settings.
Init and ephemeral containers are rejected. Containers share the Pod's network
boundary; an ordinary helper requiring independent authority needs its own
workload. The managed checkpoint helper below receives no workload token and
has a separate private control volume.

The trust ConfigMap must be in `sproozi-agents`, immutable and contain public CA
material only. Runtime, policy, template and run references resolve within the
run's namespace.

## Ordinary run completion

For the current adapter, use one workload container named `agent`. Validation
accepts other names, but completion reads only `agent` after the whole Pod
becomes terminal. Long-running ordinary sidecars prevent completion.

Run the CLI in one-shot mode and replace the shell with `exec`. The container's
actual exit code determines success or failure; review the proposed PR separately.
See [agent contract and completion](agent-contract.md).

The adapter creates disposable Pods. Kubernetes Agent Sandbox integration is
planned; it is not an installed dependency.
The current adapter uses no external `Sandbox`, `SandboxTemplate`, `SandboxClaim`
or `SandboxWarmPool` custom resources or controllers. The default Kind demo
also selects no custom `RuntimeClass`. See the
[quickstart dependencies](../guides/getting-started.md#cluster-dependencies-and-external-crds).

## MCP client preparation

Optional `spec.clientConfig.harness` selects `codex`, `claude-code`, `opencode` or `hermes`
native MCP configuration in the immutable contract. The administrator-owned
launch command loads only that configuration into disposable client state.
Only requested named MCP capabilities appear; credentials remain in the gateway.
The [harness reference](harnesses.md) describes formats, trusted launch examples,
model-provider setup and supported configurations.

## Managed native sessions

Managed sessions require explicit `clientConfig.harness: codex` or `claude-code`;
the ordinary OpenCode/Hermes selectors do not make those clients movable. Client installs use upstream defaults. Sproozi checks supported native state and
completion records without requiring an exact client version. A move keeps the
same harness kind.

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

This fragment supplements a complete managed runtime. Use exactly two containers
named `agent` and `checkpoint`, `workloadContainers: [agent]`, non-root UID/GID
1000, read-only root filesystems, dropped capabilities, explicit resources and a
bounded memory-backed workspace. The controller sets the fixed supervisor and
helper commands. The helper mounts the workspace and its private temporary
volume, with no provider credentials, token mounts or native client environment.
Managed visits require additional stop, usage-accounting and restore checks.
Ordinary Pod terminal completion does not replace them.

The managed template carries `sessions.sproozi.com/managed: "true"`; each visit
gets fresh admission and gateway access. Trusted launch configuration supplies
the same approved instructions independently. Codex's retained initial developer
block is checked against that digest. Claude receives fresh approved instructions,
foreground Bash, fixed completion hooks, permission mode and settings; its
transcript cannot assert any of those permissions. Both clients rely on the
outer container and enforced gateway network boundary.

Checkpoint version 2 records the harness kind, selected native state and Git
workspace. Restoring without starting an agent creates `workspace` plus `codex`
or `claude` under `/workspace/session`; it transfers no gateway credentials,
launch configuration or personal native home. The legacy version 1 decoder
recognizes Codex only. A native completion hint does not prove that all writers
have stopped. The placement must save the accepted acknowledgement that the turn
and tools have finished. It must revoke access, confirm that the actual
container stopped and finalize usage accounting. These checks must finish before
capture or destination activation. A lost Claude completion acknowledgement
remains unconfirmed even when the container is terminated and its transcript
appears settled.

The supported state subset supports foreground tools and excludes background/subagent
state and compaction. The observed recovered Claude 429 history record needs a
later genuine successful assistant leaf; unresolved retries cannot reuse an old
answer or grant authority. See the [profile setup](../../cmd/session-profile/README.md),
[supervisor contract](../../cmd/native-session/README.md),
[checkpoint helper](../../cmd/checkpoint/README.md),
[ADR 0011](../adr/0011-native-session-compatibility-and-completion.md).
