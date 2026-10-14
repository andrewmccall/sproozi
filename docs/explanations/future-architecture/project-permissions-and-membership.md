# Project permissions and membership

Status: deferred

## Problem

Team access needs finer attribution than the current configuration-based installation principal.

## Desired behaviour

Authenticated people and agents receive explicit project roles and capability constraints. Revocation prevents new reads and admission while trusted lifecycle services still fence existing writers and retain accounting.

## Proposed primitives

Authenticated subject; project membership/role; capability ceiling; source-qualified destination approval; revocation version and audit.

## Open questions

Which workflow first needs individual identities or live membership changes? Who administers roles? What should happen to already admitted executions on revocation? How are shared homes partitioned?

## Non-goals and next evidence

No roles inferred from bot display names, portable grants or authority from project labels. Current configured Projects stay valid until a real team workflow earns new identity and ownership boundaries.
