# Architecture decisions

Architecture decision records explain why Sproozi uses a design. Current guides
and references describe how to use it. An accepted decision can remain valid
after its initial implementation is complete.

## When to add a record

Architecturally significant changes MUST include an ADR and updated docs in the
same change. This applies to trust boundaries, public API concepts, integration
patterns, ownership, lifecycle, and runtime or harness responsibilities.

Include the ADR and affected docs in the task's done criteria before
implementation. Record consequential choices as you make them. Before
completion, check that the record describes the actual implementation.

## What to record

Use `NNNN-short-description.md` with the next available number. Include a date
and an accepted or proposed status. Record:

- The problem and constraints.
- The selected decision and reasons.
- Meaningful alternatives, their benefits and why they were rejected.
- Accepted tradeoffs, costs and consequences.
- Conditions that would justify revisiting the decision.
- Links to affected explanations, references and implementation.

Keep operational steps and current support limits in guides and references. The
ADR preserves the reasons for the choice.

## Review before completion

Review the task's retained source evidence, candidate designs, design synthesis,
implementation checks, reviews and TODO records. For each material choice, link
its public decision record or explain in the task evidence why it needs no ADR.
One coherent ADR can cover related choices. Distinguish selected decisions,
rejected proposals, later fixes and deferred work. Preserve the original
evidence.

Update affected explanations, references and examples. Update the main README
when supported capabilities change. Compare claims with code and test evidence,
then run `make verify-docs`. The command checks required documents and links. It
does not check documentation accuracy or decision coverage.

Preserve accepted decision history. To replace a decision, add a new ADR, mark
the old record superseded and link both records.

## Decisions

These records preserve the reasons for each design. A later record can replace
part of an earlier decision. ADR 0015 replaces exact client-version selection
and the combined MCP discovery schema. ADR 0021 replaces ADR 0015's unversioned
client installs with pinned lockfiles and digests for CI and the verifiers.

| Decision | Topic |
| --- | --- |
| [0001](0001-mcp-capabilities-through-shared-gateway.md) | MCP access through the shared gateway. |
| [0002](0002-native-cli-harnesses-and-model-protocols.md) | Native CLI harnesses and model protocols. |
| [0003](0003-persistent-assistants-and-retained-results.md) | Persistent assistants and retained job answers. |
| [0004](0004-portable-sessions-and-bounded-executions.md) | Sessions across bounded executions. |
| [0005](0005-controlled-docker-launch-before-session-adoption.md) | Controlled Docker launch before adopting existing sessions. |
| [0006](0006-native-cli-and-terminal-before-gui-mobility.md) | Native CLI and terminal access before GUI integration. |
| [0007](0007-codex-round-trip-before-harness-expansion.md) | Codex before additional session adapters. |
| [0008](0008-selected-native-and-git-checkpoints.md) | Selected conversation and Git checkpoints. |
| [0009](0009-runtime-backends-and-sealed-accounting.md) | Runtime backends and finalized usage accounting. |
| [0010](0010-managed-session-authority-and-admission.md) | Managed-session permissions and destination admission. |
| [0011](0011-native-session-compatibility-and-completion.md) | Native state compatibility and completion. |
| [0012](0012-retained-work-and-fresh-session-continuations.md) | Retained work and local resume. |
| [0013](0013-configured-projects-and-workspace-revisions.md) | Configured Projects and workspace revisions. |
| [0014](0014-review-boundaries-and-native-contracts.md) | Shared gateway limits, native contracts and CLI identities. |
| [0015](0015-default-clients-and-native-tool-schemas.md) | Default clients, MCP schemas and model requests sharing an allowance. |
| [0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md) | Capability checks at the gateway dispatcher and fresh revalidation reads. |
| [0017](0017-runtime-admission-and-replay-retention.md) | Runtime validation at admission, live slot and sandbox reads, and bounded replay records. |
| [0018](0018-coordinator-owns-every-visit.md) | The home coordinator owns every visit, including the first, and launches never change. |
| [0019](0019-pull-operations-and-work-owner-names.md) | Derived pull operations and one rule for resolving work owner names. |
| [0020](0020-docker-placement-removes-stopped-visits.md) | The Docker placement removes a stopped visit's containers and volume after recording it. |
| [0021](0021-pinned-ci-inputs-and-latest-client-canary.md) | Pull request CI runs only pinned clients, images and manifests; a weekly canary reports upstream drift. |
| [0022](0022-webhook-hmac-secrets-in-own-namespace.md) | Webhook HMAC Secrets live in their own namespace, away from provider credentials. |
| [0023](0023-controller-writes-only-in-sproozi-agents.md) | The controller writes sandbox objects only in sproozi-agents; its cluster role keeps reads. |
| [0024](0024-native-host-platforms-and-cli-releases.md) | Native host security and automated CLI releases with a Homebrew tap. |
| [0025](0025-managed-harness-integration-and-diagnose.md) | Every harness receives the Sproozi integration, and the gateway explains an execution's own denials. |
