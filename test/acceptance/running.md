# Run acceptance checks

Run commands from the repository root. Cluster tests must use dedicated Kind
clusters. Install the tools and supply credentials listed for the selected check.
Provider-backed tests consume model usage and can create branches and PRs.

The [test requirements](README.md) describe the behaviours to check. These
instructions describe how to run the existing drivers.

## Which command proves what

Every verification target, what a pass establishes, and whether CI runs it.
The `Tests` workflow runs three parallel jobs. `race-1` runs
`make verify-generated`, `make vet` and `make verify-race RACE_SHARD=1`;
`race-2` runs `make verify-race RACE_SHARD=2`; and `checks` runs the remaining
`Tests` rows in table order, taking only the offline verifier from `verify-mcp`. The race run proves everything a plain run does,
so CI does not run `make test`. `Lint` and `E2E` are separate workflows.
`make help` prints the same summaries.

| Target | A pass proves | CI | Needs |
| --- | --- | --- | --- |
| `make test` | After regenerating code and manifests, every non-e2e Go test passes: unit, rendered manifests, envtest API validation, integration and offline acceptance helpers. | No; CI runs the same packages under race | Go; downloads envtest binaries |
| `make verify-generated` | Regenerating code and manifests and running `go fmt` leaves the tree unchanged, so committed generated files are current. Fails on any uncommitted change. | Tests (`race-1`) | Go, Git |
| `make verify-fast` | `go vet` is clean and every non-e2e package passes under the race detector with coverage, without regenerating sources. | Tests (`race-1` and `race-2`, as `vet` and `verify-race`) | Go; downloads envtest binaries |
| `make verify-race` | The race-and-coverage half of `verify-fast`. `RACE_SHARD=1` runs `test/` and `internal/session`, `RACE_SHARD=2` every other package; the two shards together cover every non-e2e package. | Tests (`race-1`, `race-2`) | Go; downloads envtest binaries |
| `make verify-protocol` | Pinned `gh` and `git` clients produce the GitHub protocol the gateway inspects. Zero matched or skipped tests fail. | Tests (`checks`) | Downloads the checksum-pinned `gh` |
| `make verify-mcp` | MCP and gateway packages pass under race, and the live MCP verifier's safety and failure reports pass offline. | Tests: `checks` runs the offline verifier, `python3 hack/verify/mcp-live-test.py`; the race shards cover the packages | Go, Python 3 |
| `make verify-workflow-fixtures` | Session-mobility protocol fixtures, quickstart ownership and CRD checks, Kind bootstrap readiness ordering, Kind image import and build cache selection, demo setup credential preservation and registry wiring, and `make demo` input
checks before cluster creation and cleanup after a setup failure. Stubs replace Docker and Kind. | Tests (`checks`) | Python 3, Bash; fetches the pinned Calico manifest into `.local/cni` on first run |
| `make verify-docs` | Local Markdown links resolve and no private-plan paths are referenced. It does not establish that documentation is current. | Tests (`checks`) | Python 3 |
| `make lint` | golangci-lint reports no issues; `make lint-config` checks its configuration. | Lint | Go; downloads the checksum-pinned golangci-lint release |
| `make test-e2e` | The full stack works in an owned two-node Kind cluster with enforcing CNI, using the pinned clients and Hermes digest. See [Harnesses and Hermes in Kind](#harnesses-and-hermes-in-kind). | E2E | Docker, Kind, kubectl, Calico from `hack/verify/fetch-calico.sh` |
| `make test-e2e-existing` | The same e2e suite passes against a Kind cluster you created (`KIND_CLUSTER`), then removes the controller, CRDs and test namespaces. `make test-e2e` skips that teardown because it deletes the whole cluster. | No | An existing dedicated Kind cluster |
| `make verify-mcp-client` | Generated run-local MCP configuration loads in installed stock Codex. No model request. | No | Codex CLI |
| `make verify-harness-clients` | Generated native MCP configuration loads in installed Codex, Claude Code and OpenCode. No model request. | No | All three CLIs |
| `make verify-native-session` | Installed stock Codex resumes through production checkpoint capture and restore against a local provider. | No | Codex CLI |
| `make verify-harness-process` | Installed Claude Code and OpenCode complete model and tool loops through the inspected-TLS gateway with local providers. | No | Claude Code, OpenCode |
| `make verify-mcp-kind` | Configured MCP providers work with stock Codex in an owned Kind cluster, including denials, budgets and revocation. Real model requests. | No | See [Configured MCP in Kind](#configured-mcp-in-kind) |
| `make verify-local-onboarding` | On this computer's Docker engine, `init --local` provisions Codex and adds Claude Code, `doctor` passes, one stock terminal turn per harness completes through `attach`, both stops bind native UUIDs, shared Codex instructions reach the provider, a renewed certificate reaches the running gateway, and a configured kubeconfig receives no calls. Deterministic local providers. See [Local onboarding](#local-onboarding). | No | Docker with Buildx, Go, Python 3, OpenSSL |
| `make verify-kubernetes-release` | In an owned Kind cluster with enforcing CNI, the rendered release assets install as documented; the provider-free first check passes with permitted, denied and blocked paths; renewal reaches the running gateway; a CA rotation passes through new immutable trust objects; and the certificate controller lacks other rights. See [Kubernetes release assets](#kubernetes-release-assets). | No | Docker with Buildx, Kind, kubectl, Python 3 |
| `make verify-live` | One real AgentRun on a provisioned cluster opens a scoped PR in `SPROOZI_DEMO_REPO` and its denial probes fail closed. | No | Provisioned cluster and credentials; creates a PR |
| `make verify-public-tree` | No private local material is tracked. | No | Git |
| `make verify-release` | Release inputs are pinned, then `test`, `lint`, every `Tests` row, `test-e2e`, `verify-live` and `verify-public-tree` pass and the release image builds. A missing prerequisite fails. No push. | No | Everything above; `RELEASE_BUILD=1`, `SPROOZI_DEMO_REPO` |

`make verify-release` does not run the installed-client targets or
`verify-mcp-kind`; run those when changing client configuration or configured MCP.
The `Latest Clients Canary` workflow is not a target: weekly and on manual
dispatch, it runs the client `--version` checks and `make test-e2e` with the
newest upstream releases and opens or updates a tracking issue when they differ
from the pins or fail. Pull requests never run it. See
[bumping pinned clients](../../hack/clients/README.md#bumping-pinned-clients).
[cases.json](cases.json) is a catalogue of planned scenarios, not a gate.

CI caches Go modules and build output, the golangci-lint plugin binary and
analysis cache, and, in `E2E`, Docker layers for the manager, provider and
clients images. Only caches written by pushes to `main` are shared with pull
requests. `SPROOZI_BUILD_CACHE=gha` selects the Docker layer cache and works
only inside GitHub Actions; leave it unset locally. See the
[CI caching amendment](../../docs/adr/0021-pinned-ci-inputs-and-latest-client-canary.md#amendment-ci-caching)
to ADR 0021.

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

If the demo cannot start the stock Codex image, run `bash hack/demo/image-test.sh`
after `make demo-setup` with `KIND_NODE_IMAGE` set. It runs only that image's tools
in a disposable Kind cluster without CNI, so it isolates image and registry faults.

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
at a time. A retained demo holds its lock until cleanup. A run that finds the
lock held names the owning run and prints the exact `cleanup` command. For ChatGPT mode, use a
newly authorised ChatGPT credential that no other gateway is refreshing. Keep the generated
proof under `.local`; keep the skill and its feature map current with
`/maintain-verification-skill`.

## Harnesses and Hermes in Kind

Install Docker, Python 3, Go 1.26.6, kubectl and Kind. Docker must be running;
the cluster and four stock clients need several GiB of memory and disk. From
the repository root:

```sh
bash hack/verify/fetch-calico.sh .local/verification/calico-v3.30.4.yaml
GOTOOLCHAIN=go1.26.6 \
SPROOZI_CNI_MANIFEST="$PWD/.local/verification/calico-v3.30.4.yaml" \
SPROOZI_CNI_SELECTOR=k8s-app=calico-node \
make test-e2e
```

The command owns a dedicated two-node Kind cluster, builds and loads the release
image, fixture providers and stock clients, and launches the complete
controller, TLS gateway, task service and persistent Hermes coordinator. Native
API chat and a native scheduled job delegate real AgentRuns. The suite checks
all four stock worker loops, denials, budgets, cancellation, deadlines, UID-bound
replay, result retention, PVC restart and direct-network isolation. It removes
its cluster on completion. Set `SPROOZI_PRESERVE_ON_FAILURE=1` to retain an owned
failed cluster for investigation.

`hack/verify/kind.sh` builds the images and compiles the suite while the
cluster starts. Once the cluster is up, both nodes pull the digest-pinned Hermes
image themselves while the built images are loaded into both nodes at once. The
first of these jobs to fail stops the run and prints its log. The suite then
skips its own image step, because `kind.sh` sets `SPROOZI_E2E_IMAGES_LOADED=1`.
`make test-e2e-existing` builds and loads the images inside the suite, using
`build.py` without arguments.

The clients come from the lockfiles in [`hack/clients`](../../hack/clients/README.md),
installed with `npm ci --ignore-scripts`, and Hermes runs by the digest in
`hack/verify/orchestration-kind/build.py`. `fetch-calico.sh` refuses a manifest
whose SHA-256 differs from its pin. Set `SPROOZI_HERMES_IMAGE` to a
`repository@sha256:...` reference to test another Hermes image; the canary uses
it for the latest release. To move a pin, follow
[bumping pinned clients](../../hack/clients/README.md#bumping-pinned-clients).

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
Docker, Kind, kubectl, OpenSSL, PyYAML, the Calico v3.30.4 manifest verified by
`hack/verify/fetch-calico.sh`, a digest-pinned stock Codex tools image and a
dedicated single-owner ChatGPT credential file. The check uses real model requests. Do not share that rotating
credential with another running gateway; the script returns its newest replacement
to the supplied file before deleting the cluster.

```sh
docker build -t controller:mcp-proof .
docker build -f hack/verify/mcp-provider/Dockerfile -t mcp-provider:proof .
export MCP_PROOF_IMAGE=controller:mcp-proof
export MCP_PROVIDER_IMAGE=mcp-provider:proof
export CODEX_IMAGE="<stock-codex-image>@sha256:<digest>"
export CHATGPT_SESSION_FILE="<private-single-owner-chatgpt-credential.json>"
bash hack/verify/fetch-calico.sh .local/verification/calico-v3.30.4.yaml
export SPROOZI_CNI_MANIFEST="$PWD/.local/verification/calico-v3.30.4.yaml"
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

## Local onboarding

`make verify-local-onboarding` builds the native image with the release bake
recipe, unless `python3 hack/verify/local-onboarding.py --image ID` names one.
It uses an isolated `HOME`, a short private root under `/tmp` and the
deterministic providers from the
[session movement fixtures](../../hack/verify/session-mobility/README.md) on
ports 18765 and 18775. It removes its network, root and providers when it
finishes. The report records the tested revision, whether the tree was dirty,
each step's result and the timing of the first `init --local`.

A pass establishes the local flow on the engine you ran it on. It does not
establish real provider authentication, paid-model behaviour or other
platforms; record those separately.

## Kubernetes release assets

`make verify-kubernetes-release` creates a dedicated `sproozi-release-*` Kind
cluster with Calico, imports locally built release images, renders the release
archive with those digests and applies it exactly as the
[Kubernetes guide](../../docs/guides/install-kubernetes.md#install-from-release-assets)
describes. It deletes the cluster when it finishes; pass `--keep` to the script
to inspect it. Read `.local/verification/kubernetes-release/report.json`.

## Native clients and session movement

`make verify-native-session`, `make verify-harness-clients` and
`make verify-harness-process` cover installed clients on the host; see the
[command map](#which-command-proves-what). For Docker-to-Kubernetes movement, use the
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
