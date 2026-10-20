# Project permissions and membership

Status: deferred

## Problem

Current Project membership identifies an installation. Team access may need to
identify individual people and agents.

## Desired behaviour

Authenticated people and agents receive explicit Project roles and capability
limits. Removing access prevents new reads and executions. Trusted services must
still be able to stop existing sandboxes and finish usage accounting.

## Proposed parts

- Authenticated person or agent identity.
- Project membership, role and capability limits.
- Destination approval tied to the source installation.
- Versioned revocation records and audit history.

## Open questions

Which workflow first needs individual identities or live membership changes? Who
administers roles? What happens to active executions when access is removed? How
should shared home daemons separate access?

## Scope

Bot display names and Project labels do not grant permissions. Checkpoints
cannot carry grants to another installation. Keep current configured Projects
until a team workflow establishes the need for different identity and ownership
rules.
