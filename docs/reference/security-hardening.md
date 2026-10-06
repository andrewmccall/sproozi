# Security hardening

Sproozi separates agent execution from trusted policy and credentials. The
recorded semantic demo passed in an isolated Kind environment; production
hardening and the implementation limits below remain relevant.

## Protect transport and identities

Use the shared inspected-TLS gateway with a dedicated trust bundle. Treat
`AgentRuntime.spec.gatewayEndpoint` as administrator-owned configuration. Deny
direct API/provider access and proxy bypasses through an enforcing CNI, and
verify those denials from the actual sandbox. Standard NetworkPolicy permits
traffic involving the Pod's own node. Do not infer API isolation from policy
alone when the API server is on that node. See
[Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

The gateway validates the ServiceAccount namespace, name, UID, audience and
active Run state. It checks new requests against live policy and closes existing
sessions through bounded revalidation. This does not reverse upstream side effects.

The projected token lifetime is two hours; policy limits execution to one hour.
The demo reads the token once. Revocation depends on live gateway checks rather
than waiting for token expiry.

Keep `sproozi-agents` dedicated to managed sandboxes. Restrict who can create
workloads, ServiceAccounts or runtime templates. Runtime validation rejects init
and ephemeral containers. All normal containers in a Pod share one run principal.
For working completion today, use one container named `agent` and no long-running
ordinary sidecar.

## Retain GitHub protections

Install the GitHub App only on approved repositories and grant the minimum
permissions for branch push and PR creation. Keep branch protection or rulesets
enabled. The gateway authorizes bounded operations; it does not replace GitHub's
protected-branch rules. Review proposed PRs before merging or deploying them.

## Model budgets are not hard spending ceilings

The deployed gateway stores reservations atomically in Kubernetes, keyed by Run
UID and capability. Requests with an output-token limit reserve that limit,
while provider settlement counts total input and output tokens. Input usage can
therefore exceed the reservation. Rejecting a response after settlement fails
does not undo the provider call, and retained reservations can undercount the
consumption already incurred.

Administrator-owned pricing converts verified usage into cost. Missing prices
are rejected after the upstream call, so an unpriced model can still consume
provider resources. Keep the pricing table aligned with approved models and
use provider-side monitoring and limits. Do not rely on Sproozi token or dollar
ceilings as hard provider-spending bounds today.

## Cleanup and state retention

Normal terminal handling deletes the recorded sandbox, contract, NetworkPolicy
and ServiceAccount. A lost provisioning status write can leave resources outside
that path. In particular, missing sandbox status can let identity/network cleanup
proceed while the Pod remains. Cleanup does not yet recover every partially
provisioned run. Inspect Run UID ownership and containment after interrupted runs.

`retentionTTL` governs the AgentRun record. Budget ConfigMaps have no automatic
retention/deletion path. Webhook replay keys are stored atomically in ConfigMaps
in `sproozi-webhook-state`; they survive restarts and are shared by replicas.
Expired replay entries are removed only when the same event key is checked again,
so unique records accumulate. Operators must account for this retained state.

## Restrict trusted services

The installed controller-manager can manage Pods, identities, NetworkPolicies
and RBAC objects. Its binding does not grant Secret reads; the optional metrics
overlay adds TokenReviews and SubjectAccessReviews. Restrict exec/debug access
and reuse of trusted service accounts. The gateway holds provider credentials
and may update its rotating ChatGPT session Secret. ChatGPT refresh requires one active gateway process,
even though budget and replay state are shared. See
[model provider authentication](../guides/model-provider-auth.md).

The local demo's short-lived CA and gitignored environment file are disposable
evaluation inputs. For deployment, supply managed certificates, least-privilege
credentials, namespace isolation and durable audit collection. The planned
[home-ops playbook](../guides/home-ops-playbook.md) has not passed full acceptance.
