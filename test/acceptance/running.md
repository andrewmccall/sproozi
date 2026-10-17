# Run acceptance checks

Run commands from the repository root. Cluster tests must use dedicated Kind
clusters. Install the tools and supply credentials listed for the selected check.
Provider-backed tests consume model usage and can create branches and PRs.

The [test requirements](README.md) describe the behaviours to check. These
instructions describe how to run the existing drivers.

## Getting-started guide

The repository includes the
[`verify-sproozi` skill](../../.agents/skills/verify-sproozi/SKILL.md) for checking
this Kind guide. After configuring your demo environment, run:

```sh
python3 hack/verify/quickstart.py doctor --env-file "$PWD/.local/demo.env"
python3 hack/verify/quickstart.py demo --env-file "$PWD/.local/demo.env"
```

The helper runs the same `make demo` path against an isolated copy of the current
checkout, so deployment-time manifest edits stay out of your working tree. It
retains the command log, source and guide hashes, observed CRDs and live report,
then removes its owned cluster and scratch files. This command consumes model
usage and creates a real branch and PR. Interactive App creation and sign-in
remain the setup steps above.

Add `--keep-cluster` to inspect a passing demo. Set
`SPROOZI_QUICKSTART_EVIDENCE` to the exact directory printed by the helper, then:

```sh
export KUBECONFIG="$SPROOZI_QUICKSTART_EVIDENCE/cluster.kubeconfig"
python3 hack/verify/quickstart.py doctor --evidence "$SPROOZI_QUICKSTART_EVIDENCE"
```

Use the [exploration commands](../../docs/guides/getting-started.md#8-inspect-the-result-and-explore). Finish with:

```sh
python3 hack/verify/quickstart.py cleanup "$SPROOZI_QUICKSTART_EVIDENCE"
unset KUBECONFIG
```

Only one helper invocation can drive the shared local Docker images and registry
at a time. A retained demo holds its lock until cleanup. For ChatGPT mode, use a
newly authorised session that no other gateway is refreshing. Keep the generated
proof under `.local`; keep the skill and its feature map current with
`/maintain-verification-skill`.

## Harnesses and Hermes in Kind

Install Docker, Python 3, Go 1.26.6, kubectl and Kind. Docker must be running;
the cluster and four stock clients need several GiB of memory and disk. From
the repository root:

```sh
mkdir -p .local/verification
curl --fail --location --silent --show-error \
  https://raw.githubusercontent.com/projectcalico/calico/v3.30.4/manifests/calico.yaml \
  --output .local/verification/calico-v3.30.4.yaml
GOTOOLCHAIN=go1.26.6 \
SPROOZI_CNI_MANIFEST="$PWD/.local/verification/calico-v3.30.4.yaml" \
SPROOZI_CNI_SELECTOR=k8s-app=calico-node \
make test-e2e
```

The command owns a dedicated two-node Kind cluster, builds and loads the release
image, fixture providers and pinned stock clients, and launches the complete
controller, TLS gateway, task service and persistent Hermes coordinator. Native
API chat and a native scheduled job delegate real AgentRuns. The suite checks
all four stock worker loops, denials, budgets, cancellation, deadlines, UID-bound
replay, result retention, PVC restart and direct-network isolation. It removes
its cluster on completion. Set `SPROOZI_PRESERVE_ON_FAILURE=1` to retain an owned
failed cluster for investigation.

The acceptance runtime sets Claude Code's supported retry count to zero for
intentional denials. Stock OpenCode retries exhausted-budget responses, so its
budget case uses a short run deadline and requires `TimedOut` with sandbox
cleanup. The public runtime examples retain administrator-owned retry behavior.

Evidence and image provenance remain in
`.local/verification/orchestration-kind/`. Local fixture responses replace paid
inference. This tests native protocols, deployment and policy behavior; it does
not measure model quality, real billing or external bot delivery. No provider
credentials or existing bot configuration are loaded.

## Kubernetes MCP

```sh
make verify-mcp
```

The tests exercise discovery, allowed calls, forged arguments and tools,
namespace/resource/capability denials, shared REST/MCP budgeting, separate run
budgets, oversized inputs and outputs, redacted upstream errors, and revocation
of both existing clients and in-flight upstream work. The same gate runs offline
verifier checks for partial creation, ambiguous create timeouts, cleanup failures,
redacted reports and Secret references in every Pod container and volume source.
These checks make no model requests.

For a caller-provisioned, dedicated multi-node Kind cluster, render the bounded
fixture and run the live gate. The fixture renderer requires PyYAML. Supply a
digest-pinned stock Codex image with curl and the selected model-auth mode.
The cluster needs the current controller/gateway, `config/agent-rbac`, Calico,
gateway TLS and controller/sandbox trust ConfigMaps, and valid model credentials
in the trusted namespace. See the [manual setup](../../docs/guides/manual-pr-demo.md)
and [model authentication](../../docs/guides/model-provider-auth.md) for those inputs.
The gate expects no competing active runs. It does not provision a cluster or
credentials and does not create a GitHub PR.

```sh
python3 hack/verify/render-mcp-fixture.py \
  --codex-image "$CODEX_IMAGE" --model-auth chatgpt > /tmp/sproozi-mcp-proof.yaml
kubectl --kubeconfig "$MCP_KUBECONFIG" apply -f /tmp/sproozi-mcp-proof.yaml
python3 hack/verify/mcp-live.py \
  --kubeconfig "$MCP_KUBECONFIG" --cluster "$MCP_KIND_CLUSTER" \
  --report .local/verification/mcp-live.json
```

The fixture grants only `kubernetes.read` and `model.inference` and scopes Pod
reads to `sproozi-demo`. Its stock CLI prints JSON tool events and an exit marker,
then holds the Pod for verifier probes. The gate requires successful CLI exit
and a completed tool result containing the real demo Pod. It then verifies
namespace and unknown-tool denials, direct service/node API timeouts, cancellation,
stale-token denial from an independently isolated witness and removal of
run-labelled invocation resources. It deletes its own Run and witness and
writes a redacted report. The fixture resources and cluster remain caller-owned.

## Configured MCP in Kind

`make verify-mcp-kind` creates and removes its own two-node Kind cluster. Supply
Docker, Kind, kubectl, OpenSSL, PyYAML, the checksum-pinned Calico v3.30.4 manifest,
a digest-pinned stock Codex tools image and a dedicated single-owner ChatGPT
session file. The check uses real model requests. Do not share that rotating
session with another running gateway; the script returns its newest replacement
to the supplied file before deleting the cluster.

```sh
docker build -t controller:mcp-proof .
docker build -f hack/verify/mcp-provider/Dockerfile -t mcp-provider:proof .
export MCP_PROOF_IMAGE=controller:mcp-proof
export MCP_PROVIDER_IMAGE=mcp-provider:proof
export CODEX_IMAGE="<stock-codex-image>@sha256:<digest>"
export CHATGPT_SESSION_FILE="<private-single-owner-session.json>"
export SPROOZI_CNI_MANIFEST="<pinned-calico-v3.30.4.yaml>"
export MCP_PROOF_REPORT=.local/verification/configured-mcp-live.json
make verify-mcp-kind
```

The retained local-registry stock image can use `hack/demo/load-image.sh`;
other images must already be available to Docker. The gate deploys two fixture
providers with different schemas and gateway-only bearer credentials. It checks
actual Codex tool events, scope/schema/tool/server denials, cross-connection
budget consumption, direct network denial, cancellation, correlated audit and
controller cleanup. Exact provider invocation counts establish that denied calls
never execute tools. An independently contained witness tests stale authority
after the original Pod stops. The setup enforces immutable sandbox trust and
uses server-side apply for generated CRDs.

Normal exit removes the owned cluster and private fixture material. If rotating
session recovery fails, the script retains its named cluster and private directory
for recovery instead of losing the newest credential.

## Native clients and session movement

Use these targets from the repository root:

| Target | Purpose |
| --- | --- |
| `make verify-native-session` | Check native checkpoint capture and resume with stock Codex. |
| `make verify-harness-clients` | Load generated configuration in installed stock clients without a model request. |
| `make verify-harness-process` | Run model and tool loops through the inspected-TLS proxy with local provider fixtures. |
| `make test-e2e` | Deploy the clients, gateway, task service and persistent Hermes in isolated Kind. |

For Docker-to-Kubernetes movement, use the
[session movement driver](../../hack/verify/session-mobility/README.md).
It covers both selected clients and provides Codex held-turn and cancellation
scenarios. Run the complete placement failure matrix for each client when changing
movement or completion behaviour.

## Retained work and Projects in Kind

Use the [complete mobility setup](../../hack/verify/session-mobility/README.md)
to create a dedicated two-node Kind cluster with enforcing Calico, build the
approved native/helper images, prepare TLS and install the controller/gateway.
The [stock Hermes walkthrough](../../hack/verify/portable-workflows/README.md#stock-hermes-and-a-persistent-kubernetes-owner)
then installs this owner and coordinator and runs the complete native round trip.
It needs Docker, Kind, kubectl, Git, Go 1.26.6, OpenSSL, Python 3 and PyYAML.
It selects no personal credentials or home-network resources. The development
fixture seeds only public Sproozi compiler sources. Follow the
[Project dogfooding walkthrough](../../hack/verify/portable-workflows/README.md#development-and-governed-home-ops-projects)
for actual package checks, exact revision replay and scoped Pod-read/Secrets-denial
checks in both placements.

Its report separates native client/lifecycle proof from model quality. The model
fixtures direct real stock Hermes MCP calls and stock Codex execution; they do
not establish a paid-model Sproozi contribution or a personal home-ops diagnosis.
Keep the report and private logs before deleting the dedicated cluster.

## Local MCP checks

Run these from the repository root. The client check needs installed stock Codex
and makes no model request. `make test` uses envtest for API-server validation.

```sh
make verify-mcp
make verify-mcp-client  # installed stock Codex; no model request
make test              # includes real API-server validation through envtest
```
