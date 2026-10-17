# Agent communication

Status: deferred

## Problem

An agent may need to ask another agent for help. The caller must keep control of
permissions, cost and cancellation.

## Desired behaviour

An authenticated agent delegates a task to an approved agent. The system saves
the request, gives the child execution separate permissions and records its
result. It defines who can cancel the child.

## Proposed parts

- Authenticated caller and approved destination.
- Saved task request that cannot change on retry.
- Separate permissions and limits for the child execution.
- References to the child status and result.

## Open questions

Can existing MCP delegation meet the first use case? What context can leave the
source? Who pays for the child and cancels it if the parent stops? Does the
protocol need to preserve a conversation between calls?

## Scope

This proposal does not include unrestricted peer discovery or shared native
sessions. Imported files cannot grant permissions.
