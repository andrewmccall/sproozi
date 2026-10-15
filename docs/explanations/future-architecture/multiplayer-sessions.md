# Multiplayer sessions

Status: deferred

## Problem

Multiple people may need to inspect an investigation and deliberately take control of its next turn.

## Desired behaviour

Authenticated participants can discover permitted sessions, read bounded observations and request explicit control transfer. At every moment one trusted owner controls native input and workspace writes.

## Proposed primitives

Participant identity and role; viewer versus writer permission; control lease/epoch; retained transfer intent; bounded observation and audit.

## Open questions

What authentication identifies a person rather than a bot installation? Which native clients support multiple viewers? How are disconnects, stale terminals and conflicting transfer requests resolved?

## Non-goals and next evidence

No shared stdin by default, implied control from membership or collaborative editor. Demonstrate two authenticated viewers and one explicit writer transfer before adding broad participation.
