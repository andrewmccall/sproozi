# 0026: Kubernetes release assets and a self-managed certificate controller

Status: accepted

Date: 2026-10-20

## Problem and constraints

Installing Sproozi on Kubernetes meant building and publishing images, copying
digests into manifests, and creating a private CA, a 30-day gateway certificate
and an immutable trust ConfigMap with OpenSSL. Renewal was the operator's job,
and the gateway read its certificate only at startup.

The [onboarding proposal](../explanations/future-architecture/simplified-local-client-onboarding.md)
keeps Kubernetes installation an explicit operator action. It asks for release
assets, a provider-free first check, and an optional self-managed CA that needs
no cert-manager. The proposal also requires that the certificate helper hold
only narrow, named rights, that trust bundles stay immutable and versioned, and
that external certificates remain supported without the helper.

## Decision

### Release assets

Each release publishes `sproozi_VERSION_kubernetes.tar.gz` beside the CLI. It
extracts to `sproozi-release/` with `crds.yaml`, a digest-pinned `install/`
bundle, an optional `self-managed-ca/` component, two quickstart overlays and a
provider-free `first-check/`. The CRDs are separate so the operator applies them
and waits for `Established` first. The control and native images are the same
multi-architecture digests the CLI release records in `images.json`.

The first check uses the native image, which already contains `kubectl` and
`curl`. It reads Pods through the gateway, then proves a policy-denied Secret
read and a blocked direct API path.

### A small controller owns the certificate lifecycle

`/certificates`, in the control image, reconciles three kinds of object:

- a precreated authority Secret holding the same certificate store the local
  home uses (`internal/pki`);
- the gateway's precreated serving Secret;
- immutable trust ConfigMaps in `sproozi-agents`, named by trust generation:
  `sproozi-sandbox-trust`, then `sproozi-sandbox-trust-1-2`, then
  `sproozi-sandbox-trust-2`.

Each reconcile loads the authority Secret into private memory, applies any
operator request, ensures a current leaf, and commits the Secret with
optimistic concurrency. Only then does it publish the serving Secret and trust
object derived from the committed state. One replica with a `Recreate`
strategy is the single writer; a conflicting write fails and retries from the
winner's state.

The controller renews the leaf automatically. Rotation advances only on an
explicit annotation (`prepare`, `switch`, `retire`, `rollback`, `replace` or
`renew`). The operator switches runtime references between those steps and
confirms that older Pods have stopped. Existing trust objects are never
patched.

The Kubernetes gateway polls its mounted certificate files and atomically
replaces the serving certificate when they change. It rejects a replacement
that does not cover every inspected host or has expired.

### Narrow RBAC

The controller has `get` and `update` on its two Secrets by name, and `get` and
`create` on ConfigMaps in `sproozi-agents`. It has no cluster role, no list or
watch, no other Secret, and no Sproozi API group. Both Secrets are precreated
with no data, so reapplying the install never resets them. A rendered-manifest
test enforces these rules.

### External certificates

The external quickstart omits the controller. The operator supplies
`sproozi-gateway-tls` and `sproozi-sandbox-trust`, for example from
cert-manager or an enterprise issuer. Harness arguments, capabilities and the
four CRDs are the same in both modes.

## Alternatives

- **cert-manager by default.** It is mature, but the proposal makes it
  optional. External mode still accepts it.
- **A one-shot bootstrap Job.** It cannot renew.
- **A signer sidecar in the gateway Pod.** It would put signing keys beside
  the workload-facing proxy and give the gateway write access to its own
  identity.
- **A Kubernetes-native rewrite of the certificate store.** It would duplicate
  the renewal and rotation rules. Loading the Secret into the existing store
  reuses them, at the cost of private scratch memory.
- **Automatic rotation.** The local home can see every consumer. In Kubernetes
  that needs Pod and runtime visibility beyond the controller's narrow rights,
  so the operator confirms distribution instead.
- **The controller updating AgentRuntime trust references.** It would need
  write access to runtimes, which the proposal excludes.
- **Content-hash trust names.** They are stable, but the quickstart could not
  name the first bundle before installing.

## Tradeoffs and consequences

- Rotation needs four operator steps; the guide gives the checks between them.
- Retired trust ConfigMaps remain until the operator deletes them.
- Until the controller first writes the serving Secret, the gateway restarts
  while its certificate files are absent.
- Gateways enabling model, GitHub or MCP authorities need extra `--dns`
  arguments on the controller.
- The release workflow pushes images before the release publishes, so a failed
  release can leave published images behind; the CLI pins digests, so a retry
  is safe.

## Revisit conditions

- Operators need hands-free rotation and accept the controller reading Pods and
  runtimes.
- A gateway needs certificates for authorities it learns at runtime.
- cert-manager becomes a required dependency for another reason.

## Links

- [Install on Kubernetes](../guides/install-kubernetes.md)
- `cmd/certificates`, `internal/certificates`, `config/certificates`,
  `cmd/gateway/certificate.go`, `hack/release/kubernetes.py`,
  `hack/verify/kubernetes-release.py`
- [ADR 0025](0025-local-provisioning-and-lifecycle.md) for the shared certificate
  store and release images
