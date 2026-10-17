# DNS egress containment

Status: deferred.

Kubernetes sandbox policies allow UDP and TCP port 53 to kube-dns, including
runs without endpoint capabilities. A recursive resolver can resolve external
names. These queries bypass gateway permission checks and accounting. An
enforcing CNI does not remove this DNS exception.

Controlled Docker sessions deny DNS and allow only the fixed gateway TCP
destination. Their DNS restrictions differ from those of Kubernetes runs.

A future design must establish which workloads need DNS, which names they need
and who approves resolution. Options include:

- Use fixed gateway addresses and deny workload DNS.
- Allow only approved internal names through a resolver with recursion disabled.
- Add an explicit DNS capability and a resolver gateway with usage limits.

Each option affects availability, address changes and service discovery.
Routing queries to the cluster resolver alone keeps the external-name channel.

Before adding DNS controls, test allowed internal queries and refused external
queries over UDP and TCP in an isolated environment. Include resolver failure,
policy changes and restart. Preserve gateway trust and workload identity checks.
Permission to call an HTTP destination does not grant DNS permission.

See [security hardening](../../reference/security-hardening.md) and
[ADR 0014](../../adr/0014-review-boundaries-and-native-contracts.md).
