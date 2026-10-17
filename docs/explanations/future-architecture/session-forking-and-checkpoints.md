# Session forks and conversation checkpoints

Status: deferred

## Problem

A developer may want to try another approach while keeping the source
conversation and unfinished workspace unchanged.

## Desired behaviour

An authorised caller selects a checkpoint from a stopped session and its
workspace revision. The fork gets a new Sproozi session ID and fresh
permissions. It records its source, which remains available.

## Proposed parts

- An immutable source-checkpoint digest.
- References to the parent session and execution.
- An explicit choice of unfinished workspace or committed revision.
- Independent permission checks at the destination.

## Open questions

Which selected clients can import history into a new native conversation safely?
How long must the parent remain available? How should callers distinguish a
conversation fork from the current workspace-only revision launch?

## Scope

This proposal excludes cloning active workers, inherited permissions, automatic
merges and cross-harness native resume.
