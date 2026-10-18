# Local native placement

`OpenPlacement` owns one protected local Docker placement registry. The caller
supplies an explicit `codex` or `claude-code` native kind and an `Engine` bound to an administrator-selected Unix socket and private
Docker CLI configuration, immutable image content IDs, resource and retained
storage limits, approved instructions, and a trusted home gateway `Authority`.
The registry requires an owned private directory, uses a single-owner file lock
and retains atomic records before external mutations. Reopening requires the
same placement configuration.

`Prepare` accepts either a workspace artifact for a first visit, whose
`session.Launch` has empty native thread and checkpoint IDs, or a known native
UUID and an aggregate checkpoint whose digest matches the immutable launch. It
creates an inert native container, a separate checkpoint helper and a network
anchor. After actual container registration, `InitializeAllocation` supplies
fresh native environment, optional model/options and public gateway trust.
`Activate` opens gateway authority before the fixed native activation operation.
Repeated operations use the retained exact container identities and immutable
contracts; exited native containers cannot reopen. Inspection re-drives a
retained activating intent through the same authority admission and native gate,
so lost activation replies recover on the original allocation and incarnation.
Admission errors remain unknown and cannot become prepared or active.
Inspection of a creating record is observation-only and returns `preparing`;
`Prepare` explicitly retries that intent. `AbortPreparation` handles a partial
preparation whose incarnation was never reported by discovering and fencing only
exact already-created fixed roles, including lost create replies. It creates no
container or authority and invents no native UUID or usage receipt.

The home gateway implements these hooks:

| Hook | Required effect |
| --- | --- |
| `InitializeAllocation` | Retain exact launch/allocation/container binding and return fresh native access while inactive |
| `Activate` | Admit only the retained current execution and original deadline |
| `Revoke` | Deny new capability work immediately; retries converge |
| `BindNative` | Bind an initial empty thread exactly once to the observed genuine native UUID |
| `Seal` | Independently confirm actual stopped execution and return trusted usage for every allocated capability |

The agent runs as UID/GID 1000 with a read-only root filesystem, dropped
capabilities, no-new-privileges, bounded PID/memory/CPU and private bounded tmpfs
for its control socket and fresh HOME. Its only shared mount is a named local
driver tmpfs volume, with an explicit size limit and no host bind. The helper
mounts that volume continuously through agent stop and capture, has no network
or gateway credentials, and retains restore metadata in its own private tmpfs.
The helper publishes `/workspace/session/workspace` and the selected native
home, `/workspace/session/codex` or `/workspace/session/claude`, as one restored
pair. Checkpoint kind and pin must match the launch. Codex restore validates the
actual initial approved instructions; Claude receives fresh trusted instructions
at launch because its selected transcript contains no such authority.

The anchor has NET_ADMIN solely to install and verify fixed IPv4/IPv6 firewall
rules. It has no workspace, native tools invocation or authority credentials.
The worker joins the exact anchor network namespace. UID 1000 may connect only
to the administrator-selected home gateway IPv4 address and TCP port; other
IPv4, IPv6 and DNS traffic is rejected. Each activation and attachment rechecks
the retained security, resource, mount and network contracts and the actual
anchor firewall. A running anchor alone does not establish containment.

`Attach` uses the fixed native-session JSON terminal protocol through Docker
exec. One attachment may write input at a time. Client cancellation terminates
the exec client while native PID 1 and bounded replay remain available. The
caller implements terminal rendering and authenticated transport. Inputs are
rejected after the move gate closes.

`Stop` gates input and preserves admitted gateway work while the supported final
native turn completes. After native quiet, it revokes gateway authority and
independently stops the actual container before capture. For the first local
session, it binds and persists the observed native UUID before quitting. The
launch never changes: `Stopped.ThreadID` reports that UUID, and
`Stopped.Launch` and the receipt keep the launch as prepared. The
supervisor socket and journal are workload-accessible diagnostics. Neither a
quit acknowledgement nor PTY process-group exit proves every tool stopped.
Docker must independently report the exact native container as stopped with
`Running=false` and PID zero before the helper can capture portable state or
the authority can settle usage.

The immutable stopped checkpoint and receipt are retained before helper/anchor
cleanup. A lost response replays retained evidence. Missing usage remains
unknown with its allocation encumbered, rather than becoming zero. Failed
capture leaves the actual stopped state retained and cannot authorize a new
activation. Claude capture recovery requires a persisted successful quiescence
acknowledgement; termination alone cannot recreate it. Cancelled receipt replay
rechecks the physical fence and retries sealing existing accounts without
initializing missing/corrupt accounting. `Abort` forces the exact runtime stop without requiring a completed
native turn or a fresh checkpoint, and retains the same accounting uncertainty
when independent sealing is unavailable. Backend uncertainty never becomes an
absent or safely stopped placement.

Once the stop, sealed receipt and any checkpoint are retained, `Stop` and
`Abort` remove the visit's agent, helper and anchor containers and its
workspace volume. A cancelled preparation removes whatever it created. The
placement confirms each container stopped and records `removing` before the
first removal, then `removed` after the last. Only that record lets a missing
container count as removed; without it, a missing container is still an error.
After `removed`, retried `Stop`, `Abort` and `Inspect` calls answer from the
record without Docker. A removal failure leaves the recorded stop intact, and
the next `Stop` or `Abort` for the visit retries it. A visit whose usage is
still unknown keeps its containers until an `Abort` retry seals it. See
[ADR 0020](../../../docs/adr/0020-docker-placement-removes-stopped-visits.md).

Targeted behavior tests run with:

```sh
go test -race ./internal/runtime/docker ./cmd/native-session ./internal/harness
```

These tests exercise durable retries and failure boundaries through deterministic
Docker observations. They do not establish actual container isolation. The live
Docker verification must separately drive stock native turns, prove gateway-only
egress and quotas, observe actual whole-container stop, and restore/resume the
same UUID through this placement. The standalone stock TUI verifier exercises
production PTY behavior and native continuity without proving those boundaries.
