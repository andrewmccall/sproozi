# Shared workspace reconciliation

Status: deferred

## Problem

Independent agents working on a project can propose incompatible changes to its ordinary Git repository.

## Desired behaviour

Each agent works in its own workspace. Proposed commits retain source revision and execution lineage. A deliberate reconciliation step detects conflicts and records the selected merged result.

## Proposed primitives

Independent Git checkout; immutable base and proposed commit; change owner; reconciliation intent/result; separately authorised publication.

## Open questions

Who chooses between conflicting proposals? Which checks must pass before publication? Can ordinary branches and pull requests contain the first workflow? How are abandoned proposals retained?

## Non-goals and next evidence

No concurrent writes to one directory, proprietary Git service or automatic conflict resolution. First reconcile two isolated proposals with a visible conflict.
