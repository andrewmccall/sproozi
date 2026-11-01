# 0051: Interactive local sessions may run without a model budget

Status: accepted
Date: 2026-10-30

Partly supersedes [ADR 0010](0010-managed-session-authority-and-admission.md)'s
fixed finite local ceilings, for model inference only.

## Problem and constraints

Local setup gave every session a 200,000-unit model allowance. Claude Code
spends about 11,000 units a call, so a session ran out after some 18 calls.
The agent then saw only refused calls and proxy errors, and the person at the
terminal could not tell why. A local session is interactive: the person
running it watches it, and pays the provider directly. The allowance protected
them from little and stopped ordinary work.

Every allowance key also grants its capability, so leaving `model.inference`
out turns model access off rather than lifting its limit. Usage must still be
counted and finalized, because checkpoints, moves and the session's status
report it.

## Decision

An allowance limit is either a positive bound or none: `"bounded": false` with
`"value": 0`, the form `costMicros` already used. `model.inference` may have no
unit limit. Every other capability, such as `mcp.NAME` or a native gateway,
still needs a positive unit bound, because its units count calls the
administrator approved.

`sproozi init` writes `"model.inference": {}` with neither limit. An existing
configuration still holding the earlier default of exactly 200,000 bounded
units with no cost cap becomes no limit when setup runs again; any other value
is the operator's choice and stays.

The gateway accounts an unlimited session as before: each call reserves what
its plan can bound, settles to trusted usage, and adds that to the session's
ledger. With neither limit the gateway reserves nothing, so no call needs to
run alone; calls that would otherwise reserve everything remaining run
alongside each other. No stream is cut, because no reservation is held.

The Kubernetes placement keeps requiring bounded units. A cluster's ceilings
are its administrator's policy, and an AgentRun's budget is finite. A session
without a model limit cannot move to a cluster.

## Alternatives and accepted tradeoffs

- **A much larger default**, such as 5,000,000 units. It keeps every allowance
  finite, but any fixed number either stops long sessions or protects no one.
- **Treating a missing `model.inference` key as unlimited.** It would make the
  key's absence mean two things, a grant and no limit, while for every other
  capability absence means no grant.
- **Unlimited allowances for every capability.** MCP and native calls have no
  provider bill to fall back on, so their bounds are the only check on them.

The cost is that a runaway agent can spend without a Sproozi limit. The
provider's own limits and the person watching the terminal are the check.
Set a bound in `allowances.model.inference.units` to restore one.

## Revisit when

- Local sessions run unattended, such as scheduled or detached work that no
  one watches, or local sessions serve more than one person.
- Moving a session to a cluster becomes common enough that an unlimited
  session should take the cluster's ceiling on the way.

## Implementation

- `session.Limit.Valid` and `session.Allowance.Valid` in
  `internal/session/types.go`.
- Configuration and policy: `AllowanceProblems` in
  `internal/session/home/settings.go`, `approvedPolicy` in
  `internal/session/home/compose.go`.
- Gateway: `validateAllocation` and `Open` in
  `internal/gateway/docker/owner.go`; `CeilingValid` in
  `internal/gateway/grant.go`.
- Accounting: `Meter.ReserveRequest` in `internal/budget/requests.go`.
- Setup: `renderConfig` and `mergeConfig` in `internal/localsetup/install.go`.
- [Local installation reference](../reference/local-installation.md#defaults).
