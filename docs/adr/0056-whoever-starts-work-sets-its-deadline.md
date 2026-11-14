# 0056: Whoever starts work sets its deadline

Status: accepted
Date: 2026-11-14

## Problem and constraints

Every execution on Kubernetes was capped at one hour. AgentPolicy's
`maxExecutionDuration` defaulted to `1h` and a CRD validation rule refused
anything longer. The rule arrived in the first commit with no recorded reason.

The owner wants to start interactive Codex and Claude Code sessions on their
cluster while travelling and leave them running. Their rule is that a deadline
caps work they did not start, such as a Hermes workflow, and not work they
started themselves.

Lifting the ceiling alone would not have achieved that:

- **The gateway token expired under the session.** A Kubernetes sandbox's gateway
  credential is a projected ServiceAccount token with a two-hour lifetime
  (`internal/runtime/kubernetes/sandbox.go`). The kubelet refreshes the file,
  but the managed session's supervisor read it once at launch and built it into
  the harness's `HTTPS_PROXY`. `security-hardening.md` relied on the pairing:
  "The projected token lifetime is two hours; policy limits execution to one
  hour." A session past two hours would have lost the gateway.
- **Every session carries its own deadline.** The home's `maxDuration`
  (required, at most 24h), the executor's `maxDuration`, and the deadline that
  a move keeps all limit a session independently of the policy.
- **ADR 0040 accepts a failure window bounded by the hour.** When the gateway
  Service is recreated with a new address, running sandboxes lose the gateway
  until their runs end. Runs were "at most an hour long".

## Decision

**A policy's duration is optional and has no ceiling.**
`maxExecutionDuration` has no default and no upper bound; a set value must be
greater than zero. When it is unset, the policy sets no deadline.

- The controller times a run out only when a deadline exists. Without one, it
  waits for Pod and policy events rather than requeueing on a timer.
- The gateway and the remote executor treat a zero deadline as none.
- A session without its own deadline never removes a policy's deadline
  (`registry.EffectiveDeadline`).
- The executor's admission check uses its own `maxDuration` when the policy has
  none.
- Policies for work started on someone else's behalf set a value: the home-ops
  Hermes workflows keep 15 and 45 minutes.

**A managed session reaches the gateway through a loopback forwarder.** The
supervisor (`cmd/native-session`) starts `internal/nativeproxy` at activation,
when the launch has a projected gateway binding.

- The forwarder listens on an ephemeral `127.0.0.1` port and accepts only
  CONNECT.
- It requires a 256-bit secret made for this launch, sent as the proxy
  password.
- For each tunnel it reads the current projected token, opens TLS to the gateway
  (verified against the sandbox trust bundle), and sends the CONNECT with that
  token.
- The gateway's reply reaches the harness unchanged.
- The harness's `HTTP(S)_PROXY` is `http://sproozi:<secret>@127.0.0.1:<port>`.
  The token never enters the harness's environment.
- The gateway already authenticates each CONNECT separately and ends inspected
  sessions after a fixed lifetime, so clients reconnect regularly and pick up a
  refreshed token on the next tunnel.

Docker sessions keep the literal proxy:

- Their gateway grant does not expire.
- Their network anchor admits only the gateway's address, so loopback traffic
  from the agent's UID is refused.

**A session's own deadline becomes optional when its owner starts it.** This
part is decided but not yet built. `sproozi run` will accept a request for no
deadline, and an executor will need an explicit setting to admit such a launch.
"No deadline" will be the zero `time.Time`, a value every current validator
rejects, so a mixed-version system fails closed. The sites that check a
deadline will share one helper. Three checks would wrongly pass a zero deadline
and must be changed explicitly:

- `internal/session/profile/server.go`'s horizon check;
- `internal/runtime/kubernetes/placement`'s admission check;
- `registry.EffectiveDeadline` (now fixed).

Until then, a session's deadline is at most 24 hours: the home's `maxDuration`
limit.

## Alternatives

- **Raise the ceiling to 24 hours.** It matches the home's limit and avoids an
  unbounded API value. It still caps work the owner started and still breaks at
  the two-hour token. Rejected because the owner decides the deadline per
  policy.
- **A much longer projected token** instead of a forwarder. It is a smaller
  change, but a long-lived bearer token would sit in the harness's environment,
  and every run would still be bounded by whatever lifetime was chosen.
  Revocation is live (the gateway re-checks each connection), so the risk is
  replay outside the cluster, not access the gateway would otherwise refuse.
- **Have each harness re-read the token.** None of the stock clients can do
  this: they read `HTTPS_PROXY` once.
- **A Unix-socket forwarder,** which would need no secret. Node, reqwest and
  curl clients do not accept a Unix-socket proxy.
- **A loopback forwarder without a secret.** The checkpoint helper and any other
  container in the Pod share loopback, and the helper is deliberately given no
  credential.

## Consequences

- Existing AgentPolicies that took the old `1h` default keep `1h` stored. Remove
  the field from a policy to make its runs unbounded.
- A run with no deadline holds its admission slot and budget reservation until
  it ends or is cancelled. `maxActiveRuns` and budgets remain the bounds.
- ADR 0040's window, in which sandboxes lose a recreated gateway Service until
  their runs end, can now last as long as the run. Recreating the gateway Service
  needs running sessions restarted.
- Ordinary AgentRuns whose command reads the token once still lose the gateway
  at two hours. Give them a `maxExecutionDuration` under two hours, or re-read
  the token for each connection. The home-ops SRE runtimes are in this group.
- Managed sessions use a plain-HTTP proxy on loopback. git now sends proxy
  credentials after the forwarder's `407` challenge without
  `http.proxyAuthMethod=basic`. The gateway's own `401` did not prompt git to
  send them.
- Verified with the stock clients:
  - `hack/verify/harness-process -forwarder` runs Claude Code and Copilot
    through the forwarder in front of a real gateway (success, denial and
    `diagnose`).
  - A probe confirmed that Codex 0.167.0, gh and git send the forwarder's
    credentials.
  - Codex is not in that verifier. A Codex model turn through the forwarder is
    not yet proven end to end.

## Revisit conditions

- A need for an administrator-wide ceiling on unattended work. Add it as a
  separate, explicit limit rather than a CRD rule.
- Harnesses that can refresh proxy credentials themselves, which would make the
  forwarder unnecessary.
- Gateway address changes becoming routine, which would make ADR 0040's
  unbounded window matter.

## Links

- Reference: [AgentPolicy](../reference/agentpolicy.md),
  [security hardening](../reference/security-hardening.md),
  [remote executor](../../cmd/session-profile/README.md)
- Implementation: `api/v1alpha1/agentpolicy_types.go`,
  `internal/runtime/kubernetes/registry/binding.go`,
  `internal/controller/reconciler.go`,
  `internal/runtime/kubernetes/placement/placement.go`,
  `internal/nativeproxy`, `cmd/native-session/gateway_forwarder.go`,
  `internal/harness/native_launch.go`, `hack/verify/harness-process`
- Related: [ADR 0040](0040-sandboxes-reach-the-gateway-without-dns.md),
  [ADR 0031](0031-user-profile-for-managed-sessions.md)
