# 0036: Remote executors through kubeconfig contexts

Status: accepted
Date: 2026-10-20
Implementation: implemented and proved on Kind, including the service's
executor records, the CLI wiring and both mobility fixtures. The `remote` and
`workSources` configuration, `profile.ClientConfig` and the static home token
are removed.

## Problem and constraints

Home reached a [remote executor](../../GLOSSARY.md#remote-executor) through a
hand-written `remote` (or `workSources`) entry: a fixed HTTPS `url`, a
`tokenFile` holding the [home principal token](../../GLOSSARY.md#home-principal-token),
a `caFile` and a `maxCheckpointBytes` that had to equal `local.maxCheckpointBytes`.
The administrator issued a certificate for a reachable address and handed the
token and CA over out of band. A ClusterIP Service is not reachable from a
laptop, so every remote executor also needed a load balancer, ingress or tunnel.

Sproozi is unreleased, so no compatibility is owed. The user decided that
remote work is `sproozi run --remote [--context NAME|PATH] [--namespace NS]`,
that `--context` selects a kubeconfig context and its namespace as kubectl does,
that the default context is one named `sproozi`, that a context may also be
given as a path to a kubeconfig file, and that the CLI signs in as a dedicated
ServiceAccount whose Role is exactly the access it needs. The home daemon must
keep reaching the executor after the CLI exits. The executor's own guarantees
must hold: it owns its Kubernetes credentials, template and ceilings
([ADR 0010](0010-managed-session-authority-and-admission.md)), one principal
represents one home ([ADR 0012](0012-retained-work-and-fresh-session-continuations.md)),
and the protocol's TLS 1.3, bounded bodies and full-duplex terminal stream stay
intact. GitOps users apply manifests themselves; the CLI does not install
anything into a cluster.

## Decision

### Selecting a context

`remote.Select(context, namespace)` resolves the flags in the caller's own
environment with client-go's kubeconfig loading, without contacting a cluster.

- No `--context`: the context named `sproozi` in the default kubeconfig
  (`KUBECONFIG`, else `~/.kube/config`). If it is missing, the error says how to
  create it.
- `--context VALUE`: a context name when the default kubeconfig defines a
  context with exactly that name; otherwise a kubeconfig file when VALUE names
  an existing file, whose current context is used; otherwise an error naming
  both readings and listing the contexts. A context wins over a file of the same
  name, so a stray file in the working directory never hides a context. A
  separator test was rejected because real context names contain `/`
  (EKS ARNs, OpenShift `namespace/server/user`).
- The namespace is `--namespace`, else the context's namespace, else `default`.

The result is a `Selection`: absolute kubeconfig paths in precedence order, the
pinned context name and the namespace. Home records it, so neither home's own
`KUBECONFIG` nor a later `kubectl config use-context` moves a recorded executor
to another cluster.

### Finding and reaching the executor

A namespace holds one remote executor. The administrator labels its Service
`sproozi.com/remote-executor: "true"` with a port named `https`, and publishes
the CA that issued its serving certificate in the ConfigMap
`sproozi-remote-executor` under `ca.crt`. The certificate covers
`SERVICE.NAMESPACE.svc`, the name in-cluster callers such as Hermes already use.

For each connection the client lists the Service's EndpointSlices, picks a ready
Pod and opens a Kubernetes port-forward stream to its HTTPS port, WebSocket
first with SPDY fallback as kubectl does. TLS 1.3 to the executor runs over that
stream, verified against the published CA, and the profile protocol is
unchanged. Each new connection looks up the endpoints again, so a restarted Pod
is found. No local port is opened.

### Identity and authorization

The CLI signs in as the ServiceAccount `sproozi-cli` in the executor's namespace.
Its Role, shipped in `config/cli-access` and the release's `cli-access/`, grants
exactly: list Services, list EndpointSlices, create `pods/portforward`, get the
ConfigMap `sproozi-remote-executor`, and `use` on `remoteexecutors.sproozi.com`.
The last names no API resource; it exists so RBAC can say who may act as the
executor's home principal.

The client sends the context's bearer token, obtained through client-go's own
authentication wrappers so static tokens, token files and exec plugins all
work. A context that signs in only with a client certificate, or impersonates
another user, is refused with guidance, because the executor cannot review it.
The executor runs a TokenReview and a SubjectAccessReview for `use` on `remoteexecutors` in its namespace, named by
its placement name, on every request. An unknown token is 401, a known identity
without the permission is 403, and a review that cannot complete is 503
(unavailable, never absence). The executor's ServiceAccount is bound to
`system:auth-delegator`. Kubernetes RBAC is now the authorization: removing the
RoleBinding revokes access on the next request, without restarting the executor
or rotating a shared secret. Every identity RBAC allows acts as the executor's
one configured principal, exactly as every holder of the token did before; the
one-home-per-principal rule still applies, so one developer's home uses one
executor namespace.

The Hermes work token stays a static token, because Hermes has no Kubernetes
identity of its own; it is checked first and grants only the work tools. The
static home token is gone: Kubernetes review is the executor's only way to
decide the home principal, so an executor always runs in a cluster behind a
labelled Service.

### Naming, the checkpoint limit and home

A new authenticated `describe` action returns the executor's placement name,
harness and `maxCheckpointBytes`. The placement name is the executor's identity:
launches already carry it and the executor rejects any other, session rows show
it, and two developers whose contexts have different names still see the same
executor. A context is only how this developer reaches it.

`run --remote`, `sessions`, `status`, `cancel` and `recover` reach the cluster
from the CLI, which needs no running service for them. `move --remote` and
`pull` send the `Selection` over the control socket; the service discovers the
executor from its own environment, so exec plugins and their `PATH` are the
ones that will refresh credentials later. It records an `Executor` (the
`Selection` plus the described name, harness and limit) keyed by that name in
`executors.json` (mode `0600`) in its directory, registers it as a placement
and a work source, and reopens the records at start without contacting any
cluster. Reaching the same name through another context or file re-points the
record: the executor's name, not the context, identifies it, and a launch to
the wrong cluster is refused by that executor, which knows nothing of it. A
name equal to `local` or a local placement name is refused.

`move ID --local` needs no context. `pull ID` uses the default context unless
`--context` names another. A pull the service already began continues with its
recorded source and operation, whatever the retry names; this replaces
[ADR 0019](0019-pull-operations-and-work-owner-names.md)'s refusal of a retry
with a different operation, which the CLI no longer derives. `sessions` without
`--context` lists this machine and, when the `sproozi` context exists, its
cluster. A session a machine pulled is in phase `pulled` in the executor's catalogue
and in every command's output; the work API's former `home-owned` is gone.

`maxCheckpointBytes` per entry is gone: home compares the described limit with
`local.maxCheckpointBytes` (`Executor.CheckpointProblem`) when it records the
executor, because checkpoints must fit both ways.

`remote.Client` is home's handle on a recorded executor. It implements the
session placement, the terminal and the work transport, and contacts the cluster
only when used, so home starts while the cluster is unreachable. After a
transport failure or a 401 it connects again on the next call, reloading the
kubeconfig, so a refreshed or rotated token is picked up, and it checks that the
executor still describes itself as recorded. Exec plugins run in home's
environment, so their binaries must be on home's `PATH`.

## Alternatives

- **API server service proxy** (`services/https:NAME:8443/proxy`). It can be
  narrowed to one Service by name and needs no client tunnel code. But the API
  server strips the `Authorization` header after authenticating the caller, does
  not verify the backend's certificate, originates traffic that the executor's
  NetworkPolicy blocks unless the control plane's addresses are allowed (they
  differ by provider), and its handling of a long full-duplex terminal request
  is not part of its contract. Port-forward keeps end-to-end TLS and the
  protocol unchanged.
- **A URL from a LoadBalancer or Ingress.** It needs public reachability and a
  certificate for that address, which is the setup this decision removes.
- **`kubectl port-forward`-style local listener.** Supported API, but it opens a
  loopback port other local users can reach and must be restarted when the Pod
  changes. Dialing a stream per connection has neither problem.
- **Reading the home principal token from a Secret.** It keeps the executor
  unchanged, but a token once read stays valid after the RoleBinding is removed
  until the token is rotated and the executor restarted. The user asked for RBAC
  as the authorization, and TokenReview gives revocation and ServiceAccount
  token rotation without a restart.
- **Audience-bound TokenRequest tokens.** They would stop the executor from
  replaying the CLI's token against the API server, at the cost of a
  `serviceaccounts/token` grant and a token request per connection. The
  executor is a trusted service with far broader rights in the namespace than
  `sproozi-cli`, so the narrow token gains it nothing.
- **The kubeconfig's current context as the default.** That is kubectl's
  default, but it silently points remote work at whatever cluster the developer
  last used. A dedicated `sproozi` context names the cluster and identity
  explicitly.
- **A fixed Service name.** It would allow a `get` with `resourceNames`, but the
  in-cluster name `sproozi-work` is already used by Hermes and guides; a label
  lets the administrator keep it.
- **Naming remotes by context or `context/namespace`.** Context names are local
  to one kubeconfig and can be renamed; the executor already has a stable name
  that launches must carry.
- **`sproozi init --kubernetes` installing the cluster side.** Designed and
  dropped by the user: installation stays a GitOps-applied manifest set.
- **Discovery in the CLI, records sent to the service.** It would fail fast in
  the user's own environment, but the service then refreshes credentials with
  plugins it never ran. Discovering in the service proves at the first move or
  pull that it can reach the cluster on its own.
- **Refusing a second selection for a recorded name.** It guards against two
  clusters with executors of the same name, but also refuses the same executor
  reached through a file and through a context, which the user wanted to use
  interchangeably.
- **Keeping the static home token for host-run executors.** Host-run
  executors could not be reached through a context at all, since there is no
  Pod to forward to, and the fixtures moved into the cluster.

## Tradeoffs and consequences

- `pods/portforward` cannot be narrowed to one Pod of a Deployment, so
  `sproozi-cli` can open a stream to any Pod in the namespace. In
  `sproozi-system` that includes the controller and gateway, whose listeners
  authenticate on their own. A dedicated namespace per executor narrows it.
- Each request costs the executor a TokenReview and a SubjectAccessReview.
  Requests are lifecycle and catalogue calls and one long terminal request, so
  this is small; a cache would delay revocation.
- The `sproozi` context uses a long-lived ServiceAccount token Secret, because
  home outlives any short `kubectl create token` lifetime. Deleting the Secret
  rotates it; the developer then writes the new token into the context.
- One executor per namespace. Several executors means several namespaces, each
  a `--namespace` or context.
- The executor reads TLS material once at startup; changing its CA needs a new
  ConfigMap value and a restart, as before.
- Executors must run in a cluster; a host-run evaluation now deploys the
  public example instead.
- Executor names must be unique across the clusters one developer uses;
  reusing a name re-points the record.

## Revisit when

- Clients must reach executors in clusters whose API server they cannot reach.
- The API server proxy gains verified backend TLS and a header-preserving mode.
- Review volume or latency becomes measurable, or several principals per
  executor are required.
- The certificate controller ([ADR 0030](0030-kubernetes-release-assets-and-certificates.md))
  can issue the executor's serving certificate and publish its CA.
- Developers need executors with the same name in several clusters.

## Links

- [Remote executor access reference](../reference/remote-executors.md)
- [Remote executor setup](../../cmd/session-profile/README.md)
- `internal/session/remote/` (selection, discovery, tunnel, reviewer, client),
  `internal/session/profile/` (`describe`, `Authorizer`, `Connect`),
  `internal/session/home/executors.go`, `cmd/sproozi/remote.go`,
  `cmd/session-profile/main.go`, `config/cli-access/`,
  `examples/portable-workflows/kubernetes/owner.yaml`
- Refines [ADR 0010](0010-managed-session-authority-and-admission.md)'s fixed
  remote profile, and replaces [ADR 0019](0019-pull-operations-and-work-owner-names.md)'s
  owner-name rule and its refusal of a retried pull with another operation.
