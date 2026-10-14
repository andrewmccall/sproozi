# Cross-harness context handoff

Status: deferred

## Problem

A developer may want a different native client to continue an investigation whose history has a proprietary shape.

## Desired behaviour

A handoff explicitly transfers selected task context and workspace state to a fresh destination conversation. It records source lineage and losses, while same-client compatible native resume remains the stronger continuity contract.

## Proposed primitives

Versioned context manifest; bounded selected messages and artifact references; repository/revision identity; source lineage; compatibility and omission report; fresh grant.

## Open questions

What minimum context is useful across the first two clients? Which tool outputs can be represented safely? How does the destination confirm what was omitted? Which client import mechanisms are supported?

## Non-goals and next evidence

No claim of preserving native UUID, hidden client state or source authority. Demonstrate a useful bounded context transfer before standardising a format.
