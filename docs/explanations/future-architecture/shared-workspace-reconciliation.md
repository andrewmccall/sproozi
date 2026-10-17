# Shared workspace reconciliation

Status: deferred

## Problem

Agents working on the same Project can propose conflicting Git changes.

## Desired behaviour

Each agent uses a separate workspace. Proposed commits record their base
revision and source execution. A review step detects conflicts and records the
chosen merged result.

## Proposed parts

- Separate Git checkouts.
- Immutable base and proposed commits.
- An owner for each change.
- Saved merge intent and result.
- Separate permission to publish the result.

## Open questions

Who chooses between conflicting proposals? Which checks must pass before
publication? Can ordinary branches and pull requests meet the first use case?
How long should abandoned proposals remain available?

## Scope

This proposal excludes concurrent writes to one directory, a proprietary Git
service and automatic conflict resolution.
