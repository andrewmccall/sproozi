# Cross-harness context handoff

Status: deferred

## Problem

A developer may want to use another agent client for an existing investigation.
The destination may not understand the source client's conversation format.

## Desired behaviour

A handoff copies selected task context and workspace state into a new
destination conversation. It records the source and any omitted information.
This differs from native resume, which restores a compatible conversation in the
same client.

## Proposed parts

- A versioned description of the selected context.
- Size-limited messages and references to saved files.
- Repository, revision and source-session identities.
- A report of unsupported or omitted state.
- Fresh destination permissions.

## Open questions

What context is useful for the first two clients? Which tool outputs can
transfer safely? How does the destination report omissions? Which import methods
do the clients support?

## Scope

A handoff does not preserve the harness conversation ID, hidden client state or
source permissions.
