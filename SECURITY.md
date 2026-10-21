# Security policy

Sproozi is a preview for evaluation in a disposable cluster. Effective network
containment depends on an enforcing CNI; operators also provide durable audit
storage and review proposed changes before merging them.

Known limits include model reservations that can underestimate total
consumption, cleanup that can miss resources after lost provisioning status
writes, and session registrations that are not yet removed automatically.
See [security hardening](docs/reference/security-hardening.md) for current
controls.

## Report a vulnerability

Report privately through
[GitHub private vulnerability reporting](https://github.com/andrewmccall/sproozi/security/advisories/new).
Or email
[sproozi-security@andrewmccall.com](mailto:sproozi-security@andrewmccall.com).
Do not open a public issue.

Include a minimal reproduction, the affected version or commit, and whether
credentials or external resources were exposed. Do not include live secrets or
private repository contents.

Expect an acknowledgement within 7 days. The maintainer will agree a disclosure
date with you, normally within 90 days of the report, and credit you in the
advisory unless you ask otherwise.

## Supported versions

Sproozi is pre-1.0 and its API is alpha. Only the latest release receives security fixes, and a fix
ships as a new release rather than a patch to an older one. See
[versioning and compatibility](docs/contributing/releases.md#versioning-and-compatibility).

