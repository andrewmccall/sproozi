# 0040: Sandboxes reach the gateway without DNS

Status: accepted
Date: 2026-10-21

## Problem and constraints

The glossary defines the gateway as the proxy that every request from a
sandbox to an outside service goes through. Docker sessions kept that promise:
their network anchor allows only the gateway socket and rejects DNS. Kubernetes
sandboxes did not. Each run's NetworkPolicy always allowed UDP and TCP port 53
to kube-dns, even for runs with no capability. A cluster resolver forwards
unknown names upstream, so a hostile harness could send data out, and receive
instructions, in queries for an attacker's domain. The gateway saw none of it,
so none of it was checked, audited or charged. Two independent reviews on
2026-10-21 rated this the main gap in the trust boundary; the
[DNS egress note](../explanations/future-architecture/dns-egress.md) had
recorded it as deferred.

Constraints:

- Sandboxes still need to reach the gateway by the host name in
  `AgentRuntime.spec.gatewayEndpoint`, because the gateway's TLS certificate
  names that host and every harness launch builds its proxy URL from it.
- Every harness reaches outside hosts through the gateway as an HTTPS proxy
  with `NO_PROXY` empty, so clients do not resolve target names themselves. The
  Docker backend already runs every harness this way without DNS.
- The administrator owns `gatewayEndpoint`; nothing the run supplies may choose
  the address.

## Decision

- **No DNS rule.** The run's NetworkPolicy allows only the shared gateway on
  TCP 8443, and only while the run holds an allowed capability. A run with no
  allowed capability has no egress at all.
- **No resolver in the Pod.** The controller sets `dnsPolicy: None` with
  `127.0.0.1` as the only nameserver. Nothing listens there, so a lookup fails
  at once inside the Pod and no query leaves it.
- **The controller resolves the gateway.** When it creates the sandbox Pod, the
  controller resolves the gateway host with its own resolver and writes one
  `hostAliases` entry mapping that host to the address. (An IP literal is
  accepted at this layer, but managed harnesses need a host name; see below.) A failed lookup fails the Pod create and
  the reconcile retries; no Pod is created without the gateway address.
- **The Pod settings are checked.** An existing same-named Pod is accepted only
  if its DNS policy, DNS config, host aliases and `hostUsers` match, with the
  other hardened fields.

Implementation: `internal/runtime/kubernetes/gateway_address.go`,
`internal/runtime/kubernetes/network.go`, `internal/runtime/kubernetes/sandbox.go`.
Tests: `TestEnsureSandboxPinsGatewayAndRemovesDNS`,
`TestEnsureSandboxRejectsExistingPodWithClusterDNS`, `TestNetworkPolicyDeniesDNS`.

## Alternatives

- **Keep DNS and document it.** Cheapest, but the boundary would stay weaker on
  Kubernetes than on Docker, contradicting the glossary.
- **A restricted resolver.** A CoreDNS instance without forwarders, or a policy
  that answers only `*.svc` names, keeps service discovery but adds a component
  the operator must run and secure, and each cluster's DNS differs. Sandboxes
  need one name today.
- **A fixed cluster IP on the gateway Service.** Avoids the lookup, but service
  CIDRs differ between clusters, so the install could not choose one.
- **Read the Service through the API.** Exact, but needs new controller RBAC on
  Services and ties `gatewayEndpoint` to an in-cluster Service name. The
  controller's own lookup handles any name it can resolve.

## Consequences

- DNS is no longer a channel past the gateway on either backend.
- The address is fixed for the Pod's life. When the controller re-checks an
  existing Pod, it reads the Pod first and takes the address from its single
  host alias for the gateway host, without a lookup. A resolver outage or a
  changed answer therefore cannot stall a running run. The controller resolves
  only when it creates a Pod. The cost is that an existing Pod's address is
  trusted as long as it pins exactly the gateway host; the NetworkPolicy and
  the gateway's certificate bound what a wrong address could reach.
- If the gateway Service is recreated
  with a new cluster IP, running sandboxes lose the gateway until their runs
  end. Runs are at most an hour long. `gatewayEndpoint` must stay a host name:
  managed harnesses use it as their control connection's authority, and the
  gateway serves inspection hosts by name only.
- A workload that resolves names itself, for example a tool that ignores
  `HTTPS_PROXY`, now fails fast inside the sandbox. Such a tool could not reach
  its target before either, since the NetworkPolicy allowed only the gateway. The
  `sproozi-access` skill tells the agent that a failed lookup means the tool
  skipped `HTTPS_PROXY`, since `diagnose` sees no gateway record of it.
- Tests and envtest inject a resolver, because the gateway Service name does
  not resolve outside a cluster.

## Revisit when

- A supported harness or tool must resolve names other than the gateway. The
  [DNS egress note](../explanations/future-architecture/dns-egress.md) lists
  the options.
- The gateway runs as several Services or behind an address that changes during
  a run.

## Links

- `internal/runtime/kubernetes/gateway_address.go`, `network.go`, `sandbox.go`
- [Security hardening](../reference/security-hardening.md),
  [system architecture](../explanations/system-architecture.md),
  [AgentRuntime reference](../reference/agentruntime.md),
  [DNS egress note](../explanations/future-architecture/dns-egress.md)
- `hack/verify/orchestration-kind/verify.py` (`namesUnresolved` and
  `proxiedNameReached` probes)
- [ADR 0014](0014-review-boundaries-and-native-contracts.md)
