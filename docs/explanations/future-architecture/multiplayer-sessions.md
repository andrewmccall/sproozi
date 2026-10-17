# Multiplayer sessions

Status: deferred

## Problem

Several people may need to inspect an investigation and take turns controlling
the agent.

## Desired behaviour

Authenticated participants can find permitted sessions, read size-limited output
and request control. One trusted owner controls agent input and workspace writes
at a time.

## Proposed parts

- Participant identity and role.
- Separate permissions to view and control a session.
- A control lease and version to reject stale requests.
- Saved control-transfer requests and audit records.

## Open questions

How do we identify a person separately from a bot installation? Which clients
support multiple viewers? How should disconnects, stale terminals and
conflicting control requests work?

## Scope

Membership alone does not grant control. This proposal includes neither shared
terminal input by default nor a collaborative editor.
