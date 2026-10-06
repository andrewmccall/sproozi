# AgentRuntime

`AgentRuntime` defines the administrator-owned execution environment. Its
`podTemplate.spec` is the authoritative source of container images, resources,
security settings and commands. There are no duplicate top-level resource or
security fields. Images must be pinned by digest.

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
Init and ephemeral containers are rejected. All containers in a Pod share the
run's authority; a helper requiring different authority needs its own workload.

The trust ConfigMap must be in `sproozi-agents`, immutable and contain public CA
material only. Runtime, policy, template and run references resolve within the
run's namespace.

## Completion convention

For the current adapter, use one workload container named `agent`. Validation
accepts other names, but completion reads only `agent` after the whole Pod
becomes terminal. Long-running ordinary sidecars prevent completion.

Run the CLI in one-shot mode and replace the shell with `exec`. The container's
actual exit code determines success or failure; a PR is independent evidence.
See [agent contract and completion](agent-contract.md).

The adapter creates disposable Pods. Kubernetes Agent Sandbox integration is
planned; it is not an installed dependency.

## MCP client preparation

Optional `spec.clientConfig.harness: codex` adds a native `codex-mcp.toml` fragment
to the run's immutable contract ConfigMap. The administrator-owned launch command
must copy it into disposable `$HOME/.codex/config.toml` before invoking Codex.
Only requested named MCP capabilities appear; provider credentials remain in the
gateway. The demo runtime contains this launch step. See
[configured MCP capabilities](configured-mcp.md).
