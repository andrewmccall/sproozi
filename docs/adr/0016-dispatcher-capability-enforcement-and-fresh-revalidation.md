# 0016: Enforce route capabilities at the dispatcher and revalidate from live reads

Status: accepted
Date: 2026-10-17

## Problem and constraints

Every gateway route declared a capability and an enforcement tier, but the
dispatcher used only the capability, to select a budget meter. Each handler
repeated the capability check:

```go
slices.Contains(identity.Capabilities, X) && slices.Contains(identity.Policy.Spec.AllowedCapabilities, X)
```

Each handler also wrote its tier into audit events by hand, sometimes as the bare
string `semantic`. Nothing read `Route.Tier`. A new handler that forgot the check
would fail open, and the route and audit output could disagree about the tier.

The Kubernetes gateway backend resolves a projected ServiceAccount token to its
active AgentRun. Revalidation repeats that resolution against the API server.
The review asked whether that load is justified.

The workload-facing boundary is fixed: `internal/proxytransport` authenticates,
inspects TLS and strips workload credential headers before dispatch. The MCP
route chooses its capability only after the handler validates the server name in
the path.

## Decision

The route is the single authority for its capability and enforcement tier.
`Dispatcher.ServeHTTP` stores the route in the request context. For a route with
a fixed capability, it calls `ExecutionGrant.Permits` before selecting a meter
or calling the handler. A request that the run or current policy does not grant
receives 403. The dispatcher writes an audit event with the route's capability
and tier, and the handler never runs. The MCP handler calls the same `Permits`
once it has selected the named capability. Handlers create audit events with
`gateway.AuditEvent`, which copies the route's capability and tier, and set only
operation-specific fields.

Execution grants are validated once, in `proxytransport`, after authentication.
The dispatcher no longer repeats the transport's credential-header strip.
Handlers still filter the headers they forward upstream.

The Kubernetes backend is one concrete `Authenticator` with `Authenticate` and
`Revalidate`, mirroring the Docker `Owner`. Revalidation keeps reading the
current objects directly from the API server, without a cache. Fresh reads make
revocation prompt: cancelling a run, changing its policy or template, stopping a
managed worker or deleting its ServiceAccount closes inspected sessions within
the revalidation interval.

## Alternatives and tradeoffs

Deleting `Route.Tier` and documenting that handlers own the tier would remove a
field. However, it would keep seven copies of the capability check, each of
which could drift. Enforcing at the dispatcher puts the denial before any
handler code. The cost is that this denial is a plain-text 403. Previously, the
model handler returned an error in the provider's JSON format. Budget allocation
denials at the dispatcher already used plain text.

Each revalidation makes four uncached GET requests for an ordinary run: the
ServiceAccount, AgentRun, AgentTemplate and AgentPolicy. A managed execution
adds its registry ConfigMap and worker Pod, for six. The transport revalidates
every open session once per second and before each inspected request. With the
default bounds of 128 connections and eight sessions per execution, the ticker
alone can therefore produce up to about 770 GET requests per second. Request
traffic adds to that.

- An informer cache would remove most of those reads, but revocation would wait
  for watch delivery. The gateway would also hold every watched ServiceAccount,
  run, template, policy, registry ConfigMap and Pod in memory, and a stale
  cache could keep a revoked session open.
- Revalidating only on each request would leave idle and streaming sessions open
  after revocation until their next request or the five-minute session lifetime.
- A longer interval would reduce load in proportion to the interval, but would
  delay revocation by the same amount.

The selected design accepts the API server load in exchange for revocation
within about one second plus the five-second lookup deadline.

## Revisit conditions

Revisit the revalidation reads if API server load from the gateway is observed
in a deployment, or if the connection or per-execution session limits are
raised. Revisit dispatcher enforcement if a route needs a capability that is
selected after the dispatcher, as MCP already does. That route should then call
`Permits` itself.

## Links

- [Capabilities and gateways](../explanations/capabilities-and-gateways.md)
- [Security hardening](../reference/security-hardening.md)
- `internal/gateway/dispatch.go`, `internal/gateway/grant.go`, `internal/gateway/audit.go`
- `internal/gateway/kubernetes/auth.go`, `internal/proxytransport/gateway.go`
- [ADR 0014](0014-review-boundaries-and-native-contracts.md), which set the
  authority deadlines and admission bounds used in the estimate
