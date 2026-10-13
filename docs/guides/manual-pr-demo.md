# Manual AgentRun to pull request

For a walkthrough from a fresh checkout, start with
[Getting started](getting-started.md). To deploy on an existing cluster and
submit a provider-free first run, use [Install on Kubernetes](install-kubernetes.md).

The demonstrated path starts with a human-created `AgentRun` on a disposable
Kind cluster. The current adapter creates Pods in `sproozi-agents`.
It needs Sproozi's four CRDs and an enforcing CNI. The default Calico setup
installs its own networking CRDs. External Kubernetes Agent Sandbox CRDs and
controllers are not dependencies of this adapter. cert-manager, Prometheus
Operator, gVisor and Kata are also unnecessary for the base demo. See the
[dependency table](getting-started.md#cluster-dependencies-and-external-crds).

## Prerequisites

Install Docker, Kind, `kubectl`, Git, `gh`, Go, Make, OpenSSL and Python 3.
Authenticate the host's `gh` client, prepare a disposable GitHub repository with
a minimally scoped GitHub App installation, and choose API-key or ChatGPT-plan
model authentication. Setup supplies the pinned CNI and Codex tools image unless
you already selected them.

## Configure and run

```sh
export IMG=controller:sproozi-demo
make demo-setup
bash hack/demo/configure.sh
make demo-check
make demo IMG="$IMG" KIND_CLUSTER=sproozi-demo
export KUBECONFIG="$PWD/.local/demo/sproozi-demo.kubeconfig"
ls .local/verification/live-*.json
```

`make demo-setup` configures the non-secret local environment without prompts.
It downloads and checksum-verifies a pinned Calico manifest for Kind, selects
`<authenticated GitHub user>/sproozi-demo`, and builds a stock Codex CLI tools
image pinned to its manifest digest. A loopback-only Docker registry named
`sproozi-demo-registry` listens on `127.0.0.1:5001`; the demo configures the Kind
nodes to pull the exact digest through the registry's Docker-network alias,
following [Kind's local registry guide](https://kind.sigs.k8s.io/docs/user/local-registry/).
No registry is installed in Kubernetes. The image is not published to
an external registry. No Sproozi runner or completion wrapper is added.
The pinned Kind configuration explicitly enables containerd's registry hosts
directory; this avoids relying on image-archive imports preserving digest
references.
Existing repository, image, CNI and credential values are preserved. To use
another enforcing CNI, configure both `SPROOZI_CNI_MANIFEST` and
`SPROOZI_CNI_SELECTOR` in the local environment before setup.

The setup wizard selects ChatGPT-plan OAuth or API-key billing and asks only
for missing GitHub App credentials. Follow
[Save the credentials](getting-started.md#save-the-credentials) for the exact
prompts, private-key location and saved files. If the GitHub App is already configured,
`make demo-chatgpt-login` reuses or authorizes a session and verifies model access, preserving
the existing API key and App settings. See
[model provider authentication](model-provider-auth.md) for storage, refresh,
token-only subscription budgets and the single-gateway limitation.
It saves settings immediately in gitignored `.local/demo.env` with mode
`0600`; private key contents remain in the file you select. Setup also runs
automatically before `make demo-check` and when opening the credential wizard.
Automatic repository selection does not create a GitHub repository or overwrite
its contents. Create the disposable repository if it does not already exist.
Seed the disposable repository's `main` branch with
`examples/sre-demo/demo-workload.yaml` at the repository root as
`demo-workload.yaml`; the GitHub App must be installed on that repository.
Authenticate the host's `gh` client to read the resulting PR. This host
credential is used by the evaluator and is never passed to the sandbox.

The demo creates an isolated Kind cluster, installs the CNI, generates a
short-lived local gateway CA, creates the trusted Secrets and sandbox trust
bundle, applies the runtime, policy, template and broken workload, and submits
one manual run through the live acceptance gate. Codex uses ordinary `kubectl`, `git`, and `gh` clients via one
shared authenticated gateway. The controller marks a run successful only when
the agent container exits with status zero. A PR may be a useful demo artefact,
but it is not the AgentRun success condition. The separate demo acceptance gate
requires exactly one real PR from `sproozi/<run UID>/fix-demo` to `main`, a valid
commit and a change limited to `demo-workload.yaml`.

The sandbox requests only `kubernetes.read`, `github.pull_request` and
`model.inference`. Generic egress and package installation are unnecessary for
this workflow. The runtime reads the mounted run contract to obtain trusted
instructions, untrusted task evidence and its branch identity.

While the run is active, evaluator probes use the same run identity and network
policy. They first prove an allowed Kubernetes read, then require exact gateway
denials for reading Secrets and cloning an unapproved repository. Separate
probes verify a wrong token audience returns 401 and a direct connection to the
Kubernetes API times out. A failed client, missing binary, broken trust bundle
or provider error does not count as a policy denial.

`make demo` waits up to 20 minutes; use `SPROOZI_LIVE_TIMEOUT=30m` to change it.
It writes a redacted report under `.local/verification/` with the PR URL and
commit, run UID, policy decisions, model usage, tool versions, image digests and
cleanup result. The run and its disposable resources are deleted after evidence
capture. The PR and Kind cluster remain available for inspection; delete the
cluster explicitly when finished with `kind delete cluster --name sproozi-demo`.
Keep the local registry running while the demo pulls its pinned image; afterwards
it can be stopped with `docker stop sproozi-demo-registry`. Restart it with
`docker start sproozi-demo-registry` before the next run.

## What the architecture demonstrates

The broker understands Kubernetes resources and GitHub actions, so these are
semantic capabilities even though the agent uses ordinary CLI clients over an
HTTP proxy. The three tiers describe the broker's understanding of each request:
service actions, a bounded service protocol, or a permitted destination. They do
not describe three sandbox modes or stages of authority escalation.

The recorded runs exercised the semantic paths and their shared containment
boundary. Destination and package handlers exist but were not exercised by this
demo. One [native Kubernetes MCP tool](../reference/kubernetes-mcp.md) is available
through the opt-in demo renderer. A [separate bounded acceptance](../demos/verified-mcp-demo.md)
verified its use by stock Codex in Kind. The recorded PR demo did not exercise it.
[Configured remote MCP mediation](../reference/configured-mcp.md) also has a
[separate two-provider Kind proof](../demos/verified-mcp-demo.md#configured-remote-mcp-acceptance)
with Protocol enforcement. [Additional CLI harness preparation](../reference/harnesses.md)
is implemented with local tests; this recorded demo remains Codex-specific.
Dynamic escalation and managed MCP servers remain planned.

[Recorded acceptance](../demos/verified-sre-demo.md) passed on 4 October 2026.
A new checkout or environment still needs its own live report. The model budget
is not a hard total-token ceiling, cleanup has an interrupted-provisioning gap,
and the supported completion convention is one container named `agent`.
See [security hardening](../reference/security-hardening.md).

## Example resources

The bundle under `examples/sre-demo/` contains namespace definitions, one runtime,
policy and template, a deliberately failing workload, and a sample manual run.
The runtime invokes `codex exec` directly; there is no repository-owned runner.
Do not apply the bundle directly. Its illustrative image digest and repository
must be replaced by `hack/demo/render.py`, which `make demo` invokes after
`hack/demo/prepare.sh` creates credentials and trust material.

## Provider-neutral cluster check

Before configuring providers, test deployment and NetworkPolicy enforcement:

```sh
export SPROOZI_CNI_MANIFEST=/absolute/path/to/pinned-enforcing-cni.yaml
export SPROOZI_CNI_SELECTOR='<your CNI agent label selector>'
CERT_MANAGER_INSTALL_SKIP=true make verify-kind
```

This gate creates and deletes a disposable Kind cluster. It verifies that a
working connection is blocked by deny-egress and restored after policy removal.
It does not verify provider inference or PR publication.

## Optional monitoring

If Prometheus Operator CRDs and a monitoring stack already exist:

```sh
kubectl apply -k config/overlays/prometheus
kubectl apply -k examples/overlays/sre-demo-prometheus
```

These overlays expose metrics and label the conventional `monitoring` namespace
with `metrics: enabled`. They do not install Prometheus. Label your actual
monitoring namespace instead if it has a different name.

The base install includes a webhook service, but the manual example configures
no receiver or HMAC Secret. Event integration requires those inputs and a sender
that signs the [webhook contract](../reference/webhook-contract.md). The
[home-ops playbook](home-ops-playbook.md) retains that planned integration.
