# Workspace memory and indexing

Status: deferred

## Problem

Durable repository artifacts and task history can be difficult to search when beginning related work.

## Desired behaviour

An optional index helps an authorised execution find relevant context and cite its source. Ordinary Git, artifacts and retained records remain authoritative; deleting the index loses no unique work.

## Proposed primitives

Repository/revision identity; selected artifact provenance; derived searchable index; access filter; invalidation and deletion records.

## Open questions

Which concrete retrieval task benefits from indexing? What files must be excluded? How does revoked access remove derived context? How is an outdated hit distinguished from current source?

## Non-goals and next evidence

No required memory database, credential indexing or implicit trust in retrieved instructions. Prove one useful query with reproducible source attribution before choosing storage.
