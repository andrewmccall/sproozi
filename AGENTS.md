# sproozi - AI Agent Guide

Sproozi runs existing AI coding agents (Codex, Claude Code, OpenCode, Hermes) in
isolated Docker containers or Kubernetes Pods. A trusted gateway holds provider
credentials and checks every outside request against policy, and supported
sessions can move between Docker and Kubernetes. It is a Kubebuilder operator
plus a CLI and several trusted helper binaries, in one Go module.

Use the terms in `GLOSSARY.md` (session, execution, visit, placement, harness,
sandbox, gateway, finalized usage and the rest). One word per concept: when
the glossary lists "also seen as", write the chosen word.

## Layout

```
api/v1alpha1/                 CRD types: AgentRuntime, AgentPolicy, AgentTemplate, AgentRun, BudgetLedger
cmd/main.go                   Controller manager entry point
cmd/gateway/                  Shared authenticated gateway (the only workload-facing proxy)
cmd/sproozi/                  Public CLI and the local service (`sproozi service run`)
cmd/session-profile/          Remote executor: one Kubernetes placement and remote work catalogue
cmd/tasks/                    Task service for persistent assistants over MCP
cmd/native-session/           Supervisor for the harness inside a managed sandbox
cmd/checkpoint/               Checkpoint helper container
cmd/sproozi-network/          Docker network namespace owner
cmd/certificates/             Optional controller for the gateway's self-managed certificate authority and TLS
cmd/chatgpt-login/            Local administrator sign-in helper
internal/controller/          AgentRun reconciliation, queue and lifecycle
internal/gateway/, internal/proxytransport/   Gateway dispatch, TLS inspection, authentication
internal/endpoints/<service>/ Per-service handlers: kubernetes, github, model, packages, mcp, destination
internal/runtime/{kubernetes,docker}/         Execution backends
internal/budget/              Usage accounting (Kubernetes store in internal/budget/kubernetes)
internal/session/, internal/work/, internal/checkpoint/, internal/workspace/   Sessions, remote work, checkpoints
internal/harness/, internal/nativeconfig/, internal/nativepty/                 Harness registry (registry.go), configuration and terminal
internal/policy/, internal/mcppolicy/, internal/modelauth/, internal/pki/, internal/privatefs/   Policy evaluation, provider auth, CA and host-private files (`ls internal` for the rest)
config/                       Kustomize: crd, rbac, manager, network-policy, default; install (the release install);
                              agent-rbac (sandbox namespace), certificates, cli-access (remote executor CLI) and overlays
examples/                     Runnable example resources (not config/samples)
docs/                         Guides, explanations, references and ADRs
test/                         envtest, e2e, manifests, integration and acceptance tests
hack/verify/                  Verification scripts behind the make targets
```

Generated, never edited by hand: `config/crd/bases/*.yaml` and
`config/rbac/role.yaml` (from `make manifests`), `api/**/zz_generated.*.go`
(from `make generate`) and `PROJECT` (from `kubebuilder`).

## Gates

Run the gates that match the change before declaring it done:

| Change | Run |
| --- | --- |
| `*_types.go` or kubebuilder markers | `make manifests generate`, then `make test` |
| Any Go code | `make lint-fix`, `make test`, `make verify-fast` |
| `go.mod`, `go.sum` or the Go toolchain pin | also `make verify-deps` (tidiness and `govulncheck`) |
| Controller, gateway, network policy or install manifests | also `make test-e2e` (creates and removes its own Kind cluster) |
| Documentation | `make verify-docs` |
| A new harness | follow `docs/contributing/add-a-harness.md`; `go test ./internal/harness/ ./cmd/native-session/ ./internal/checkpoint/` guards the registry, CRD enum, rendered `sproozi` connection, MCP loader and bridge hook, examples, reference row and managed wiring; client loading and launch behaviour need review and `make verify-harness-clients`, `make verify-harness-process` (see the guide's "What the guards check") |
| MCP registry, policy or examples | `make verify-mcp` |
| Release candidate | `make verify-release` |

`make help` lists every target, and
[which command proves what](test/acceptance/running.md#which-command-proves-what)
maps each one to what it checks, whether CI runs it and what it needs.
`test/acceptance/README.md` defines the behaviours that tests must preserve.
Run cluster tests only in a dedicated Kind cluster, never against a real dev or
prod context.

## Documentation and decisions

Documentation is part of done. Include affected docs and examples in the task's
done criteria, update them in the same change, and update `README.md` when
supported capabilities change. Reference pages hold fields, defaults, limits
and user actions; rationale belongs in an ADR.

Architecturally significant choices MUST follow `docs/adr/README.md`: trust
boundaries, API concepts, integration patterns, ownership and lifecycle, and
runtime or harness responsibilities. Record the decision, alternatives,
tradeoffs and revisit conditions, and check the ADR against the actual
implementation before completion.

When changing API concepts or names, read `docs/reference/` and
`docs/explanations/system-architecture.md` and use their public names. Add new
terms to `GLOSSARY.md`.

`make verify-docs` checks files and links only. It does not check that docs are
current or that decisions are recorded.

## Kubebuilder conventions

Keep every `// +kubebuilder:scaffold:*` marker; the CLI injects code there.
Keep files where the CLI expects them. Scaffold new APIs and webhooks with the
CLI rather than by hand:

```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation
```

The project uses the single-group layout (`api/v1alpha1`, group `sproozi`,
domain `sproozi.com`).

### API design

- Use `metav1.Condition` for status and `metav1.Time` for timestamps.
- Follow Kubernetes API conventions: `spec`, `status`, `metadata`.
- Express validation with markers such as `+kubebuilder:validation:Required`,
  `Minimum`, `MaxLength`, `Pattern` and `+kubebuilder:default`.

### Controller design

- Declare RBAC with `+kubebuilder:rbac` markers on the controller, then run
  `make manifests`. The generated `config/rbac/role.yaml` is not installed:
  also change the hand-maintained role the binary actually uses
  (`config/rbac/controller_manager_role.yaml`, the gateway's
  `config/rbac/shared_gateway_*.yaml` or `config/agent-rbac/`) and the manifest test in `test/manifests/`
  that guards it.
- Make reconciliation idempotent. Re-fetch an object before updating it.
- Set owner references for garbage collection, and watch secondary resources
  with `.Owns()` or `.Watches()` rather than only `RequeueAfter`.
- Use finalizers to clean up external state.

### Logging

Follow the Kubernetes
[message style](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines):
start with a capital letter, no trailing period, past tense, name the object
type and balance key-value pairs.

```go
log := log.FromContext(ctx)
log.Info("Created Pod", "name", pod.Name)
log.Error(err, "Failed to create NetworkPolicy", "name", name)
```

## References

- Kubebuilder Book: https://book.kubebuilder.io
- controller-runtime FAQ: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md
- Kubernetes API conventions: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- Markers reference: https://book.kubebuilder.io/reference/markers.html
