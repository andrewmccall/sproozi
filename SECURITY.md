# Security policy

Sproozi is a preview for evaluation in a disposable cluster. Effective network
containment depends on an enforcing CNI; operators also provide durable audit
storage and review proposed changes before merging them.

Known limits include model reservations that can underestimate total consumption,
cleanup that can miss resources after lost provisioning status writes, and budget
state without automatic retention cleanup. See
[security hardening](docs/reference/security-hardening.md) for current controls.

Report suspected vulnerabilities privately to the repository owner. Include a
minimal reproduction, affected version or commit, and whether credentials or
external resources were exposed. Do not include live secrets or private
repository contents.
