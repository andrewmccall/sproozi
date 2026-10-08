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

- [0001: Use MCP through the shared capability gateway](0001-mcp-capabilities-through-shared-gateway.md)
- [0002: Native CLI harnesses and model protocols](0002-native-cli-harnesses-and-model-protocols.md)
