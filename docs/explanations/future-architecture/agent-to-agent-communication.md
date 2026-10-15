# Agent communication

Status: deferred

## Problem

An agent needs to delegate a bounded subtask or consult another agent while retaining attribution and control of cost.

## Desired behaviour

A permitted caller addresses an independently governed agent through a supported protocol. Delegation has a durable intent, bounded authority, observable result and cancellation ownership.

## Proposed primitives

Authenticated caller identity; destination allowlist; immutable delegation intent; separate child execution grant; result and lifecycle references.

## Open questions

Can existing MCP delegation meet the first use case? What context may leave the source? Who pays for and cancels a child when its parent stops? Which protocol requires conversation continuity?

## Non-goals and next evidence

No unrestricted peer discovery, artifact-derived grants or shared native session. Start with one scoped delegation whose denial and cleanup can be reproduced.
