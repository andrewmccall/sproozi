# Architecture decisions

Architecturally significant changes MUST include an ADR and update the current
documentation in the same change. This applies to changes in authority or trust
boundaries, public API concepts, integration patterns, ownership and lifecycle,
and runtime or harness responsibilities.

Include the ADR and affected documentation in the task's done criteria before
implementation. Capture consequential choices as they are made, then reconcile
them with the implementation before completion.

Record the problem and constraints, selected decision, reasons, meaningful
alternatives and their benefits and rejection reasons, accepted tradeoffs and
consequences, and conditions that would justify revisiting the choice. Link the
affected architecture/reference docs and implementation. Keep operational
instructions and current support limits in those docs; the ADR preserves why the
choice was made. Use `NNNN-short-description.md`, incrementing the highest number,
and include an accepted/proposed status and date.

When completing or revising an ADR, inventory the task's retained grounding,
alternative designs, synthesis, implementation reconciliation, review and TODO
records. Account for each material choice in the task evidence with its public
record or a reason it does not need an ADR. One coherent ADR can cover related
choices. Separate selected decisions, rejected proposals, later fixes and deferred
directions; check candidate claims against the actual implementation. Preserve
the original records so the reasoning remains reviewable.

Before declaring the change complete, update the affected explanations, references
and examples. Update the main README when supported capabilities change. Check
claims against code and evidence, then run `make verify-docs`. This gate checks
required documents and links; review documentation currency and decision coverage
separately. Passing the command alone does not complete that review.

Preserve accepted decision history. To replace a decision, add a new ADR, mark the
old record superseded and link both records.

## Decisions

Decision status and implementation progress are separate. An implemented choice
remains accepted while it governs the system. A fulfilled sequencing decision
remains part of the history. Use superseded only when a later decision replaces
the choice, with links in both records.

Reviewed on 13 October 2026 against the current checkout and retained evidence.
All decisions below remain accepted; none is wholly superseded. Fixture acceptance
is scoped to the versions and scenarios recorded in each ADR and verified demo.

| Decision | Implementation and remaining scope |
| --- | --- |
| [0001: MCP through the shared gateway](0001-mcp-capabilities-through-shared-gateway.md) | Implemented. Managed stdio, OAuth and upstream conversation continuity remain deferred. |
| [0002: Native CLI harnesses and model protocols](0002-native-cli-harnesses-and-model-protocols.md) | Implemented; later four-client Kind acceptance extends the original evidence. |
| [0003: Persistent assistants and retained answers](0003-persistent-assistants-and-retained-results.md) | Implemented for disposable delegated jobs. ADR 0012 adds retained native work. |
| [0004: Portable sessions across bounded executions](0004-portable-sessions-and-bounded-executions.md) | Controlled Codex/Claude round trips verified. ADRs 0008–0011 supply the mechanisms; 0012 adds fresh continuations. |
| [0005: Controlled Docker launch before adoption](0005-controlled-docker-launch-before-session-adoption.md) | Controlled launch implemented. Its round-trip prerequisite is met; unmanaged adoption still needs its own proof. |
| [0006: Native CLI and terminal before GUI mobility](0006-native-cli-and-terminal-before-gui-mobility.md) | Terminal experience implemented. GUI/IDE integration remains deferred. |
| [0007: Codex round trip before harness expansion](0007-codex-round-trip-before-harness-expansion.md) | Sequencing fulfilled by Codex followed by Claude. Further harnesses still require independent evidence. |
| [0008: Selected native and Git checkpoints](0008-selected-native-and-git-checkpoints.md) | Implemented for supported foreground histories and Git layouts. |
| [0009: Runtime backends and sealed accounting](0009-runtime-backends-and-sealed-accounting.md) | Implemented and exercised by both native round trips. Unknown usage remains conservative. |
| [0010: Managed session authority and fresh admission](0010-managed-session-authority-and-admission.md) | Implemented within the recorded single-owner placement and failure scope. |
| [0011: Native compatibility and completion](0011-native-session-compatibility-and-completion.md) | Implemented for pinned Codex/Claude. Claude's full held placement move/cancel matrix remains unassessed. |
| [0012: Retained work and fresh continuations](0012-retained-work-and-fresh-session-continuations.md) | Implemented for Codex; stock Hermes dispatch and local continuation verified with deterministic providers. |
| [0013: Configured Projects and workspace revisions](0013-configured-projects-and-workspace-revisions.md) | Implemented. Native development acceptance proves Project association/filtering, independent home approval and fresh conversation at exact retained Git revision. |
| [0014: Bound shared gateways and unify trusted native contracts](0014-review-boundaries-and-native-contracts.md) | Implemented. Shared native/transport contracts and review hardening; DNS and external ACP/AHP host integration remain deferred. |
