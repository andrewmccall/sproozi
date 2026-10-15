# DNS egress containment

Status: deferred direction, 14 October 2026. DNS implementation is explicitly
excluded from the review-hardening change.

Kubernetes sandbox NetworkPolicies currently allow UDP and TCP port 53 to
kube-dns, including executions without endpoint capabilities. A recursive
resolver can resolve external names. Those queries therefore form an egress
channel outside capability-gateway request authorization and accounting. An
enforcing CNI does not change that exception. This is a source-level limitation;
the review did not demonstrate data exfiltration or inspect a deployed resolver.
Docker controlled sessions currently deny DNS and allow only the fixed gateway
TCP destination. Do not describe the two runtime policies as equivalent DNS
containment.

A future design should decide whether workloads need DNS at all, which internal
names are necessary, and who authorizes and observes resolution. Meaningful
alternatives include fixed pre-resolved gateway addresses with no workload DNS,
a resolver restricted to approved internal names with recursion disabled, or an
explicit DNS capability and bounded resolver gateway. Each changes operational
availability, address rotation and service discovery. Merely routing to the
cluster resolver retains the current external-name channel.

Choose an alternative only with a concrete workload need and isolated acceptance
covering both allowed resolution and external-name refusal, UDP/TCP paths,
resolver failure, policy changes and restart behavior. Preserve current gateway
trust and workload-token boundaries. DNS authorization must not be inferred from
HTTP destination permissions.

See [security hardening](../../reference/security-hardening.md) and
[the review-hardening decision](../../adr/0013-review-boundaries-and-native-contracts.md).
