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
(Replaced by the amendment below: the client now sends only an
audience-bound token it mints for `sproozi-cli`.)

Corrected 2026-11-16: the paragraph above is dead. The client no longer sends
the context's token, and no context is refused, since a certificate or
impersonating context can mint the token too (`internal/session/remote/discover.go`).

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
recorded source and operation, whatever the retry names; this replaces the
earlier refusal of a retry with a different operation. Corrected 2026-11-15:
this text first said the CLI no longer derives the operation, and moved here
from ADR 0019, which was removed: without `--operation` the CLI still derives
it as a name-based UUID of the session ID (`cmd/sproozi/session.go`), so a plain
`pull` retry repeats the same operation and an interrupted pull completes. The
operation is chosen before home has recorded anything, which a lookup in home's
catalogue cannot cover, and the CLI can print it beforehand, which a derivation
in the daemon could not. Two homes sharing one principal credential would
derive the same operation, which is already unsupported. An explicit
`--operation` wins. `sessions` without
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

### Amendment: an audience-bound token (2026-10-21)

A review found that the client sent whatever bearer token its context held,
including an exec plugin's, and that the executor's TokenReview named no
audience. A compromised or impersonated executor could therefore collect a
developer's full cluster credential, for example from an administrator's
context. Now:

- The client uses its context only to call the API server. It mints a token
  for the `sproozi-cli` ServiceAccount with the TokenRequest API, with the
  audience `sproozi-remote-executor` and a one-hour lifetime, and sends only
  that token to the executor. It mints a new one five minutes before expiry.
  The context's own credential (static token, exec plugin, client certificate
  or impersonation) never reaches the executor, so all of them now work.
- The `sproozi-cli` Role gains `create` on `serviceaccounts/token`, limited by
  `resourceNames` to `sproozi-cli` itself, so it grants no new identity.
- The executor's TokenReview requests that audience and refuses a token whose
  review does not confirm it.

Alternatives: keep sending the context's token, which leaks it; or have the
client check with a SelfSubjectReview that the context already is the
`sproozi-cli` ServiceAccount, which still sends an unscoped API token. The cost
is a lockstep upgrade: an older client and a newer executor, or the reverse,
get a 401 with an upgrade hint. Revisit if per-executor audiences are wanted.

### Amendment: a stop outlives its request (2026-11-16)

Every request had one 45-second client timeout, and the executor ran each
action on its request's context. A graceful stop may take minutes: up to two
for the turn ([ADR 0035](0035-bounded-graceful-stops-and-settled-input.md)),
twice for a first visit, then the agent's quit, the container ending and the
capture. A longer stop, or one whose home went away, was cancelled on the
executor while the supervisor's quiesce was in flight. The supervisor finishes
its quiesce whatever happens to the stream that asked, so the container ended
with its reply unread. The retry saw an ended container with no graceful
handoff recorded and stopped it as an exit
([ADR 0038](0038-exited-agents-and-one-recovery-path.md)): a resumed Claude
Code visit kept the checkpoint it started from, losing its turns, and a Codex
visit kept its conversation but recorded an exit that never happened. The
same held for the owner protocol's `stop` and `recover`
([ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md)),
which run the executor's coordinator on the request.

Decision, in two parts:

- **The executor carries a stop on after its request ends.** A placement
  `stop`, and an owner `stop` or `recover`, runs on a context that keeps the
  request's values but not its cancellation, bounded by `StopBound` (9
  minutes 43 seconds), and tracked in `profile.Stops`. Placement stops of
  one visit (allocation and incarnation) with an identical launch, asked
  while one runs, join it and return its answer, so retries never queue
  behind the visit's lock; a stop with a different launch, such as another
  operation, starts its own. A stop that panics answers those who joined it
  with an error, never an empty success. A placement `abort` of that visit
  interrupts its stop first, and a stop that begins while the abort runs
  starts cancelled. An abort naming another incarnation leaves the stop
  alone, but still takes the visit's lock, so it answers `conflict` only once
  the stop ends, which can be after home's two-minute abort wait (home then
  records `unavailable`). Home's own aborts always name the active
  incarnation, so this is not routine. Owner cancellation already interrupts
  the coordinator's graceful stop. A retry after the stop ended, or an owner
  retry, which blocks on the coordinator's session lock, reads the recorded
  outcome, so recovery is the live path.
- **Shutdown drains them.** The executor stops accepting requests, gives
  those in flight `ShutdownRequestGrace` (10 seconds) and closes their
  connections, then waits up to `StopBound` for the stops it carries on, and
  refuses new ones. Any still running are then interrupted and the executor
  waits `DrainTail` (15 seconds) for them to let go of the placement's and
  coordinator's locks before it closes those, and logs that the stops must
  be repeated. A stop still running after that is queued on a lock that does
  not heed cancellation (the placement's journal lock, or the coordinator's
  session lock held by one of the owner's task drivers, which stop only when
  the owner closes); closing would wait for it until the kubelet's kill, so
  the executor logs how many it left and exits without closing. The locks
  are file locks the system releases. Stopping the drivers before the drain
  was rejected: closing the owner also releases its catalogue lock while
  owner stops may still write. The Pod's `terminationGracePeriodSeconds`
  must exceed the sum (608 seconds) for a stop in flight at a rollout to
  finish; the example Deployments set 620, and a manifest test holds them to
  it. With less, the kubelet kills the executor at the grace period:
  crash-safe, since output is written before its record and the writer fence
  keeps the Pod for a recapture, but the reply of a quiesce in flight is
  lost.
- **Each request waits as long as its action may take.** The client derives
  each wait from the executor's bounds (`profile/protocol.go`) and adds a
  minute: stop 10 minutes 43 seconds, prepare 8 minutes, abort and
  abort-preparation 2 minutes, everything else 45 seconds as before. The stop bound is derived in code
  from the supervisor's own bounds, which `internal/harness` publishes and a
  test holds the supervisor to: two `NativeQuiesceWait`s (2 minutes each),
  `NativeQuitWait` (30 seconds, [ADR 0035's correction of
  2026-11-15](0035-bounded-graceful-stops-and-settled-input.md#correction-the-wait-after-the-quit-2026-11-15)),
  `NativeForcedStopWait` (3 seconds) and `NativeQuiesceReplyGrace` (10
  seconds), then 5 minutes to settle. The placement's wait for a stopped
  container, unbounded without a deadline, now ends after a minute.
  `stop --remote` asks again after a dropped connection, up to three in a
  row, through a new connection. The
  [reference](../reference/remote-executors.md#request-timeouts-and-dropped-connections)
  has the table.

The invariant this relies on: once a stop has sent its quiesce, only
cancellation, the stop's own bound or the executor's exit ends it before the
reply is read; a caller leaving never does. Every later step is recorded
before the next (`NativeQuiet`, the registry's `Stopping`, `WriterFenced`, the
output digest, `Stopped`), so a stop interrupted after the reply resumes from
the record and converges on the same checkpoint. Nothing new reports a writer
stopped or captures before the fence: the same placement code runs, only
longer. The wire format and stored records are unchanged.

Compatibility. A newer home with an older executor waits longer for the same
answers, so stops between 45 seconds and the old executor's own limits now
succeed instead of timing out; a home that goes away still loses the reply as
before. An older home with a newer executor still times out at 45 seconds and
records `unknown-stop`, but the executor finishes the stop, and the next
`stop` or `recover` returns its checkpoint.

Alternatives rejected:

- **Longer timeouts alone.** Smaller, but a laptop that closes or a
  port-forward that drops still abandons the quiesce, and intermediaries in
  front of the API server may cut a silent request well before ten minutes.
- **An asynchronous stop**: answer at once with a new outcome such as
  `stop_started`, and have home poll `inspect`. It survives any transport, but
  changes the protocol and the coordinator's stop rules, needs a version
  handshake, and the placement already records the progress a retry needs.
- **Detaching inside the placement.** It would also cover in-process callers,
  but the executor's coordinator holds the session lock through its stop and
  interrupts it only by cancelling the context, so cancellation would wait
  for the turn.
- **Reading a graceful conclusion from the ended container**, such as the
  absence of an exit report. Absence is not proof, and ADR 0011 forbids
  claiming a Claude Code turn from a stopped container.

Limits:

- An executor killed during a stop, by a grace period shorter than the
  drain or a crash, loses the reply of a quiesce in flight, as a dropped
  connection did before.
- The handoff bound holds because the supervisor bounds the wait after the
  quit at `NativeQuitWait` and a kill's confirmation at
  `NativeForcedStopWait`; a supervisor older than that correction can wait
  for ever after a quit, so a stop on it reaches `StopBound` and loses the
  reply. The capture's 5-minute share is an allowance, not a bound the
  helper enforces: a capture past it is cut, which is safe, since the
  output is recorded only once whole and the retry captures again.
- Retries from a home older than this change time out after 45 seconds.
  Concurrent ones join the running stop, but one that arrives after a stop
  answered turn pending starts a new one, which nobody may read; each runs
  at most `StopBound`, and any home the executor authenticates can start
  one per visit at a time this way. Harmless and convergent.
- Something in front of the API server that cuts idle connections, such as
  a load balancer's four-minute idle timeout, can cut a stop's silent
  request before it is answered. The stop still finishes on the executor
  and the retry returns it: a spurious error, not a lost checkpoint.

Revisit when connections through the API server are cut while a stop is
silent, when an executor restart during a stop matters, when one home
starting stops for many visits at once becomes a concern, or when a stop's
bounds change (`harness.NativeQuiesceWait` and the supervisor's other
bounds, checkpoint size): then the asynchronous stop above becomes the
better shape.

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

  Corrected 2026-11-16: the amendment of 2026-10-21 adopted this alternative,
  for the reason it gives: the context's own credential never reaches the
  executor.
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
  remote profile, and replaces the owner-name rule of ADR 0019
  ([removed](README.md#removed)) and its refusal of a retried pull with another
  operation.
