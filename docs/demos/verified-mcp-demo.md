# Recorded Kubernetes MCP acceptance

Two provider-backed runs passed on 6 October 2026 in a dedicated two-node Kind
cluster with Calico v3.30.4. Stock Codex 0.154.0 discovered the required native
MCP connection and completed `kubernetes_list_pods` for `sproozi-demo`. Its tool
result contained the real `mcp-observed-workload-675c78f688-bcntm` Pod. Only
`kubernetes.read` and `model.inference` were granted. No GitHub request, mutation
or deployed correction was part of this proof.

| Recorded result | Initial acceptance | Fixture/gate repeat |
| --- | --- | --- |
| Run | `mcp-live-20261006131916` | `mcp-live-20261006132248` |
| Run UID | `5bfbbdb6-cf2c-446c-ab4b-9ab0e967f2f6` | `2bd19d01-0ae8-45f8-9379-b48d93ff5f31` |
| Successful model calls | 3 | 3 |
| Observed total tokens | 38,814 | 38,811 |
| Terminal phase | `Cancelled` | `Cancelled` |
| Local redacted report | `.local/verification/mcp-20261006.json` | `.local/verification/mcp-repeat-20261006.json` |

Both runs used the stock tools image
`localhost:5001/sproozi-codex@sha256:ea3c6f5d8607d4cd4e2f844aa7824b14e12521d4501a8d4fa3d6e702ec514def`.
The observed controller and gateway container image ID was
`sha256:aaf6c2780d14c59dc71c38a3b6eeff251b63fff7e35ca5589ca6feb3315d8dc0`,
built from a dirty tree over `61334e24414a01e1d8c5d1fa92cb4ded01d9bf7c`.
The base commit alone does not reproduce that image. Kind used Kubernetes 1.33.1.

## What passed

The gate read stock Codex's JSON tool events and required CLI exit code zero, a
completed call to the configured tool and a PodList containing the real workload.
It also verified:

- MCP Pod reads outside `sproozi-demo` were denied;
- an invented `kubernetes_delete_pods` tool was rejected;
- direct Kubernetes service and control-plane node API connections timed out
  from the worker-hosted sandbox with the proxy disabled;
- the sandbox had no Secret environment references or Secret volumes;
- cancellation reached `Cancelled`, and a still-running verifier witness using
  the old run identity received 401 or 403 through the gateway;
- run-labelled Pods, ServiceAccounts, contracts, NetworkPolicies and invocation
  RBAC resources were absent after controller cleanup;
- semantic Kubernetes audit decisions and successful provider usage were
  correlated with the Run UID.

The witness was independently network-isolated. Its own NetworkPolicy remained
in place while the controller removed the original run's containment. The gate
removed the witness after the stale-identity probe.

Model inference used the Sproozi ChatGPT-plan session in the trusted gateway.
The sandbox kept the ordinary projected run token and harmless provider-key
placeholder. An idle earlier gateway was paused during the proof to keep one
refresh owner. The latest rotating credentials were then returned to its Secret
and private session file before that gateway was restored.

## Repeat the bounded check

[Kubernetes MCP delivery](../reference/kubernetes-mcp.md#verification) describes
the caller-provisioned cluster prerequisites and commands. The repository retains
`hack/verify/render-mcp-fixture.py` and `hack/verify/mcp-live.py`. The fixture
uses the actual opt-in demo renderer, narrows its grants and holds the completed
stock CLI's Pod for verifier probes. The second recorded run exercised these
scripts together. The task-owned cluster was removed after verification.

## Scope of the evidence

The reports are local, gitignored artifacts. They retain tool names, Pod names,
denial/check outcomes and selected audit fields; they omit raw workload logs,
tool-result bodies and credential data. These are dirty-tree observations,
not a clean release or general MCP-client compatibility promise.

Cancellation was deliberate so the gate could test revocation. The zero CLI
exit establishes successful agent work before cancellation; it does not record
an AgentRun `Succeeded` transition. Cleanup covers normal recorded provisioning
and run-labelled invocation resources. It does not cover lost provisioning
status or deletion of durable budget ConfigMaps. Observed usage does not prove
a hard total-token or spending ceiling. See
[security hardening](../reference/security-hardening.md).

Local integration tests separately cover capability/resource denials, shared
REST/MCP budgeting, forged arguments/session IDs, input/output limits and
in-flight cancellation against a fake Kubernetes upstream. Configured remote MCP providers have separate evidence below. Managed stdio
servers and Docker execution remain future work. [Additional CLI adapters](../reference/harnesses.md)
have local tests; this recorded Kind proof remains Codex-specific.

## Configured remote MCP acceptance

A separate model-backed run passed on 6 October 2026 against two deployed HTTPS
MCP fixture providers. Stock Codex loaded the connections from the immutable run
contract and completed `docs.search` and `inventory.list_assets`. The providers
had different argument shapes. Registration, selected tools and argument scope
were configured through the gateway registry and AgentPolicy; no provider adapter
was added to production code.

| Recorded result | Configured provider proof |
| --- | --- |
| Run | `mcp-live-0cb625f8f4e541ec` |
| Run UID | `0cc97d42-6bfd-4c0c-b425-f23ee484482e` |
| Capabilities | `model.inference`, `mcp.docs`, `mcp.inventory` |
| Successful model calls | 3 |
| Observed total tokens | 36,751 |
| Terminal phase | `Cancelled` |
| Redacted report | `.local/verification/configured-mcp-20261006.json` |

Codex returned the literal fixture results `operations-runbook` and `home-router`.
The gate rejected both out-of-scope argument sets, missing required provider
arguments, an unlisted tool and a registered server absent from the run grant.
It exhausted the three-unit docs budget across separate connections. Provider
logs recorded exactly three approved docs calls and one inventory call: the two
Codex calls and two allowed docs witness probes. Denied requests executed no
provider tools.

Direct provider, Kubernetes service and control-plane API connections timed out
with the proxy disabled. The full Pod specification contained no Secret references,
including auxiliary containers, `envFrom`, projected volumes and image pull
credentials. Cancellation revoked the witness identity; run-labelled invocation
resources disappeared. Protocol-tier MCP decisions and successful model usage
were correlated with the Run UID. All 15 report checks passed.

The two-node Kind Kubernetes 1.33.1 cluster used checksum-pinned Calico v3.30.4 and
the same pinned stock Codex 0.154.0 image recorded above. The controller/gateway
container image ID was
`sha256:681568b0ac8687f876a4d9734d0e0b0a93d736f9108fce7674b115ad141429de`;
fixture providers used
`sha256:a99f2ed65f817c66041a317f011aa0110d66f78f5289575c98b76d7f70442824`.
These were fresh builds from the dirty tree over the same base commit. The
fixture server command is under `hack/verify/mcp-provider/` and is verification
code, not an integration adapter.

[Configured MCP verification](../reference/configured-mcp.md#deployed-verification)
describes `make verify-mcp-kind`. The first setup needed its sandbox trust
ConfigMap marked immutable; this correction is retained in the setup script.
The gate, including its corrected trust requirement, passed against the real
artifact. The task-owned cluster, temporary credentials, TLS keys and kubeconfigs
were removed. The latest rotating model session was returned to its original
Secret, and the original idle gateway was restored to one Ready replica.

This proves two deployed fixture providers through the configured HTTPS path.
It does not establish compatibility with every ecosystem server or provider
account. Normal cancellation and run-owned cleanup are covered; interrupted
provisioning recovery and hard model-spending ceilings are not. The live gate
retains budget state until its surrounding owned cluster is deleted.

Repeated local verification also exposed an SDK cancellation/teardown race.
After separating the reader lifetime from call cancellation and pinning the SDK
version with bounded synchronous notification, 100 consecutive race-enabled
revocation tests passed. `make verify-mcp` includes nine offline verifier checks
for Secret references, partial creation, cleanup failures and redacted reports.
