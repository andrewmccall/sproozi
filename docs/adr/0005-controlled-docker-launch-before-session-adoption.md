# 0005: Controlled Docker launch before existing-session adoption

Status: accepted
Date: 2026-10-10
Reconciled: 2026-10-12
Implementation: controlled Docker launch implemented; unmanaged adoption deferred

## Problem and constraints

The accepted product outcome is a native conversation and workspace moving from
local execution to Kubernetes and back. Sessions already running directly on a
developer's laptop can contain uncontrolled configuration, credentials, hooks,
plugins and active subprocesses. Importing their history does not establish
control of the original writer or its external authority.

The first complete experience needs a known source environment so that failures
in checkpoint/restore can be distinguished from adoption failures. At the initial
decision, workers were Kubernetes-only and kept native state in
disposable homes. That baseline motivated a controlled source rather than an
adoption interface as the first implementation.

## Decision

Require the first mobility session to start through Sproozi in local Docker.
Sproozi prepares the approved environment and launch configuration from the start.
Preserve the stock harness and its native conversation semantics. The destination
remains the full local-to-Kubernetes-to-local round trip.

Defer adoption of sessions launched directly with native clients. Keep adoption
as an explicit future feature, including selected-state import, compatibility
checks and exclusive control of the source writer. A controlled launch is the
initial entry path, not a permanent requirement for all future sessions.

Workspace contents remain untrusted. Starting through Sproozi does not make
repository configuration authoritative or justify copying a user's home and
credentials. The implemented launcher receives a stopped ordinary Git workspace,
captures only the supported state, and restores it in bounded container storage.
It generates native configuration and gateway access separately. Exact container
identity, policy ceilings and stop/accounting evidence remain placement-owned
under [ADR 0010](0010-managed-session-authority-and-admission.md).

## Alternatives and tradeoffs

Immediate adoption fits an existing developer workflow and avoids requiring a
launcher before work starts. It also combines native-state import, source-process
control and removal of ambient authority with the then-unproved round trip.
Controlled mobility now isolates those compatibility questions; adoption still
needs its own original-writer and import proof.

A Sproozi-launched native process on the host could retain familiar paths and
configuration. It would require a different containment and authority design
from Docker and the existing Pod workers. Local Docker is the selected first
environment; a host-process backend would need its own explicit decision.

Remote-only startup simplifies source ownership but does not deliver the accepted
local-to-remote-to-local outcome. It can be a diagnostic proof, not completion.

## Accepted costs and consequences

Developers must launch through Sproozi before beginning a movable session and
have a supported local container engine. Their existing unmanaged sessions are
not movable in the first version. Some host tools or workspace arrangements may
be unavailable until explicitly supported. These costs narrow the first entry
path while retaining conversation and workspace continuity as acceptance criteria.

## Evidence and reconciliation

The user selected the full round trip and then accepted the controlled Docker
launch recommendation during the 10 October 2026 grilling. The selected
[home launcher](../../internal/session/home/service.go) and
[Docker placement](../../internal/runtime/docker/placement.go) now implement it.
The daemon owns the Docker socket and provider/profile credentials; workers
receive neither a host repository bind nor administrator infrastructure access.
Local start does not require a cluster or invent an AgentRun. The actual Codex
core and extended proofs and the production Claude Code 2.1.27 round trip started
through this path. Claude preserved the same native UUID, unfinished Git workspace,
foreground Bash output, deadline and accounting across both moves. These results
establish controlled launch, not adoption of arbitrary sessions.

Related design: [session adoption](../explanations/future-architecture/session-mobility.md#adopt-directly-started-sessions)
and [execution runtimes](../explanations/future-architecture/execution-runtimes.md).
[ADR 0004](0004-portable-sessions-and-bounded-executions.md) records the wider
accepted outcome; ADR 0010 records the concrete ownership and move mechanisms.

## Revisit conditions

Revisit adoption when the controlled round trip is proved and native interfaces
can establish selected-state import and exclusive writer ownership. Revisit
earlier if requiring a launcher prevents the intended developers from using the
feature. Consider host-process execution only with a concrete user requirement
and evidence that its containment and authority bounds can be enforced.
