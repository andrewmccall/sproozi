# Session forks and conversation checkpoints

Status: deferred

## Problem

A developer needs to explore a second approach without changing the source conversation or unfinished workspace.

## Desired behaviour

An authorised caller selects a stopped conversation checkpoint and workspace revision. Each branch receives a new product identity, native history lineage and fresh execution authority; the source remains retrievable.

## Proposed primitives

Immutable source checkpoint digest; parent session/execution references; separately selected dirty workspace or committed revision; independent destination admission.

## Open questions

Which pinned clients can safely import history into a new native conversation? What retention contract keeps a parent available? How should a caller distinguish a conversation fork from today's workspace-only revision launch?

## Non-goals and next evidence

No active-writer cloning, inherited grants, automated merge or cross-harness native resume. First prove one supported client fork against retained stopped state.
