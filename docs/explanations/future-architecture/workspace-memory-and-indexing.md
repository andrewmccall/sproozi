# Workspace memory and indexing

Status: deferred

## Problem

Saved repository files and task history can be hard to search when starting
related work.

## Desired behaviour

An optional index helps an authorised execution find context and cite its
source. Git, saved files and retained records remain the sources of truth.
Deleting the index loses no unique work.

## Proposed parts

- Repository and revision identities.
- Source references for selected files.
- A searchable index derived from those sources.
- Access filters, invalidation rules and deletion records.

## Open questions

Which search task would benefit? What files must be excluded? How does access
removal affect indexed context? How should results identify outdated sources?

## Scope

No memory database is required. Credentials must not be indexed, and retrieved
instructions remain untrusted.
