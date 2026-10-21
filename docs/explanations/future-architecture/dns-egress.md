# DNS egress containment

Status: implemented for the gateway; workload name resolution remains open.

Sandboxes on both backends now resolve no names. Docker sessions reach the
gateway by a fixed address. Kubernetes sandboxes have `dnsPolicy: None`, a
loopback nameserver with nothing behind it and a hosts-file entry for the
gateway host that the controller resolves. Their NetworkPolicy has no DNS rule.
[ADR 0040](../../adr/0040-sandboxes-reach-the-gateway-without-dns.md) records
the decision.

The open question is a workload that needs to resolve other names itself, for
example a tool that connects without honouring `HTTPS_PROXY`. Options include:

- Allow only approved internal names through a resolver with recursion disabled.
- Add an explicit DNS capability and a resolver gateway with usage limits.

Either must keep queries for external names inside the gateway's policy, audit
and accounting. Permission to call an HTTP destination does not grant DNS
permission.

See [security hardening](../../reference/security-hardening.md) and
[ADR 0014](../../adr/0014-review-boundaries-and-native-contracts.md).
