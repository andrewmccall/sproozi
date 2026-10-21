# Verify retained remote work

This fixture drives the production remote executor, managed Kubernetes placement,
stock Codex, this machine's service and Docker continuation. It uses the existing
public deterministic TLS provider. The default authenticated MCP caller follows
the stock Hermes harness's protocol. The Kubernetes caller below runs the stock
Hermes itself. Neither mode establishes a paid-model software contribution.
Personal infrastructure is never selected.

First follow the [mobility setup](../session-mobility/README.md) through image
resolution, Codex configuration, TLS Secrets, `sproozi` context and enforcing
two-node Kind installation. Use its explicit private `work`, `codex` and
`cluster` variables. Use `export GOTOOLCHAIN=go1.26.10` for the repository
checks and host builds. Start the provider, but leave this machine's service
stopped. If you already started it, stop its saved PID gracefully and wait for
exit. Check the saved PID command still belongs to this fixture before
signalling it. Retain all journals and original operation IDs when recovering a
failed run.

Give the executor retained work, then forward its Service for the Hermes-style
caller, which uses the work token:

```sh
set -e
python3 hack/verify/portable-workflows/configure.py \
  --fixture "$codex" --manifest "$codex/kubernetes.yaml" \
  --output "$codex/workflows.yaml"
export KUBECONFIG="$work/kubeconfig"
kubectl -n sproozi-sessions create secret generic sproozi-work-credentials \
  --from-file=tls.crt="$codex/gateway.crt" --from-file=tls.key="$codex/gateway.key" \
  --from-file=task-token="$codex/work-token" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --server-side -f "$codex/workflows.yaml"
kubectl -n sproozi-sessions rollout restart deployment/sproozi-work
kubectl -n sproozi-sessions rollout status deployment/sproozi-work --timeout=180s
kubectl -n sproozi-sessions port-forward service/sproozi-work 18444:8443 >"$codex/profile-forward.log" 2>&1 &
profile_forward_pid=$!
unset KUBECONFIG
python3 hack/verify/portable-workflows/verify.py \
  --home-config "$codex/home-config.json" --profile-config "$codex/profile-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --context "$work/sproozi.kubeconfig" --output-directory "$codex/workflow-proof"
```

The configurator creates a separate private work token and points the
executor at a small stopped Git snapshot that its init container seeds. The verifier submits and replays an authenticated MCP task while the
service's socket is absent, waits for genuine native completion and retention with
finalized usage, then starts the service, pulls the checkpoint, resumes the same
harness conversation ID under a
fresh execution and completes a genuine local follow-up. It stops to retained
state without another destination. `report.json` records identities, stages and
limits; private terminal/provider logs retain native evidence. `--request-id`
retries the original submission instead of manufacturing a replacement.

## Stock Hermes and a persistent Kubernetes owner

Prepare the same dedicated mobility cluster, images, gateway and Codex fixture.
Use a fresh fixture directory and do not start this machine's service. Pull the
pinned Hermes 0.22.2 image (the same digest as
`hack/verify/orchestration-kind/build.py`) and include it when importing images:

```sh
HERMES_IMAGE=docker.io/nousresearch/hermes-agent@sha256:36099926da316742423544e055d4734777105ffe6341873af5459b957d0c2919
docker pull "$HERMES_IMAGE"
docker tag "$HERMES_IMAGE" docker.io/sproozi-kind/hermes:acceptance
python3 hack/verify/session-mobility/images.py \
  --cluster "$cluster" --output-directory "$work/images" \
  --hermes-tag docker.io/sproozi-kind/hermes:acceptance
hermes_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["hermes"]["image"])' "$work/images/images.json")
```

Resolve the local native image ID after the import, as in the mobility setup.
The generated certificate now covers `sproozi-work.sproozi-sessions.svc` as well
as the gateway, model and host IP. Older fixture certificates need a new fixture,
not an in-place replacement of retained trust.

Extend and apply the worker resources with `configure.py` as above. Then deploy
the executor and Hermes using the public deployment examples. This writes a private
manifest containing generated fixture Secrets; do not publish it:

```sh
python3 hack/verify/portable-workflows/deploy.py \
  --fixture "$codex" --images "$work/images/images.json" \
  --hermes-image "$hermes_ref" --output "$codex/stack.yaml"
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$codex/stack.yaml"
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-sessions \
  rollout status deployment/sproozi-work --timeout=180s
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-coordinators \
  rollout status deployment/hermes-coordinator --timeout=180s
go build -o "$work/bin/coordinator-provider" ./hack/verify/orchestration-kind/provider
TLS_CERT_FILE="$codex/gateway.crt" TLS_KEY_FILE="$codex/gateway.key" \
SPROOZI_FIXTURE_LISTEN=0.0.0.0:19865 "$work/bin/coordinator-provider" \
  >"$codex/coordinator-provider.log" 2>&1 &
coordinator_provider_pid=$!
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-sessions \
  port-forward service/sproozi-work 18444:8443 >"$codex/profile-forward.log" 2>&1 &
profile_forward_pid=$!
python3 hack/verify/portable-workflows/verify.py \
  --home-config "$codex/home-config.json" --profile-config "$codex/profile-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --hermes-deployment hermes-coordinator --hermes-kubeconfig "$work/kubeconfig" \
  --context "$work/sproozi.kubeconfig" --output-directory "$codex/hermes-proof"
```

Start the native provider on port 18765 from the mobility setup as well. Hermes
uses a separately authenticated model fixture on 19865, discovers the actual
`sproozi_run` tool, dispatches the managed Codex task, and later reads
`sproozi_status` through its native MCP client. The verifier then exercises the
same pull/resume/stop path. It records its submission before starting Hermes,
preserves failed reports, and accepts `--request-id` for exact submission replay.

The executor has its own ServiceAccount and PVC; Hermes has no Kubernetes
token, and the verifier's pull signs in as `sproozi-cli`.
Worker/provider credentials are not copied into checkpoints. Keep journals and
both PVCs when investigating a failed run. Stop the two saved provider PIDs and
`profile_forward_pid`, then delete only the dedicated Kind cluster after retaining
the private proof. The verifier owns its temporary service and stops it.

## Development and governed home-ops Projects

Use a fresh private fixture directory (called `codex` by the mobility walkthrough)
and the same isolated Kind kubeconfig. Before the final native image import, add
the public offline Go toolchain/module cache to that image:

```sh
docker build -f examples/portable-workflows/sproozi.Dockerfile \
  --build-arg NATIVE_IMAGE=docker.io/sproozi-session/native:fixture \
  -t docker.io/sproozi-session/native:sproozi-build .
docker tag docker.io/sproozi-session/native:sproozi-build docker.io/sproozi-session/native:fixture
```

Repeat the image import and `host.py prepare` steps using this image's resolved
local ID for agent/helper/anchor. Generate the worker manifest as in the mobility
walkthrough. Then configure the two distinct Projects, independent approval by this machine's service
and explicit compiler limits before deploying the owner:

```sh
python3 hack/verify/portable-workflows/configure.py \
  --fixture "$codex" --manifest "$codex/kubernetes.yaml" \
  --output "$codex/workflows.yaml" --projects --build-sproozi \
  --home-ops-kubeconfig "$work/kubeconfig"
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$codex/workflows.yaml"
python3 hack/verify/portable-workflows/deploy.py \
  --fixture "$codex" --images "$work/images/images.json" \
  --hermes-image "$hermes_ref" --build-sproozi --output "$codex/stack.yaml"
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$codex/stack.yaml"
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-sessions \
  rollout status deployment/sproozi-work --timeout=180s
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-coordinators \
  rollout status deployment/hermes-coordinator --timeout=180s
python3 hack/verify/portable-workflows/seed.py \
  --fixture "$codex" --kubeconfig "$work/kubeconfig"
```

The seed exports only public `api`, `cmd`, `internal`, `go.mod` and `go.sum`
compiler sources, including current changes, into an ordinary committed Git
snapshot. It excludes private configuration, credentials, logs and build caches.
Its commit identifies the exported snapshot; it does not claim upstream commit
provenance. The separately owned PVC source is `/state/source-sproozi`.
The remote owner then captures it immutably for each submission.

Start both model providers and the owner port-forward from the preceding section,
using this fixture's TLS files, before running either proof. Leave the service stopped:

```sh
python3 hack/verify/portable-workflows/verify.py \
  --home-config "$codex/home-config.json" --profile-config "$codex/profile-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --hermes-deployment hermes-coordinator --hermes-kubeconfig "$work/kubeconfig" \
  --project sproozi-dev --build-sproozi --replay-revision \
  --context "$work/sproozi.kubeconfig" --output-directory "$codex/sproozi-dev-proof"
python3 hack/verify/portable-workflows/verify.py \
  --home-config "$codex/home-config.json" --profile-config "$codex/profile-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --hermes-deployment hermes-coordinator --hermes-kubeconfig "$work/kubeconfig" \
  --project home-ops --home-ops --context "$work/sproozi.kubeconfig" \
  --output-directory "$codex/home-ops-proof"
```

The development proof runs actual offline Go package tests in the stock native
client remotely and after same-conversation local resume. It starts a second,
fresh native conversation at the exact retained Git HEAD and proves independent
execution identity and unchanged source checkpoint/ownership. MCP and local CLI
Project filters and persistent repository association are checked.

Home-ops uses a one-hour read-only ServiceAccount credential created only in the
explicit dedicated Kind cluster. The harness observes the approved Running
Pod, attempts a prohibited Secrets read and reports the actual denial in both
placements. The verifier checks execution-bound gateway audit events and scans
decoded selected native/workspace artifacts and audit logs for the generated
work token and Kubernetes credential. Tokens stay in protected host/gateway state.
If the read token expires, refresh it through the same scoped ServiceAccount and
restart the local gateway before a fresh task; keep existing receipts unchanged.

Both models are deterministic fixtures directing genuine tool work. These proofs
do not assess model reasoning, autonomous software contribution or personal
home-network diagnosis. Actual operator setup is in the
[deployment guide](../../../docs/guides/retained-work-kubernetes.md) and
[Project reference](../../../docs/reference/projects-and-revisions.md).

A resource-policy denial before writer preparation retains `unknown` status.
Inspect the exact cause, correct the reviewed policy, then pass the original
`report.json` request UUID via `--request-id` into a **fresh** output directory.
The verifier uses the public CLI recovery path when necessary and preserves the
original session, visit, deadline and grant. An exhausted allowance is not
refunded by recovery; use exact cancellation and a deliberately new task.
Preserve failed reports rather than editing their `passed` value.

## Real external MCP

For the keyless public DeepWiki integration, gracefully stop the remote executor and the service,
then repeat configuration with `--deepwiki`. The configurator uses the same
[public example registry](../../../examples/mcp/servers.json)
for a private local copy and the remote ConfigMap, and adds the example's
[bounded tool policy](../../../examples/mcp/agentpolicy.yaml).
Apply the output and restart the gateway to snapshot the changed registry:

```sh
python3 hack/verify/portable-workflows/configure.py \
  --fixture "$codex" --manifest "$codex/kubernetes.yaml" \
  --output "$codex/deepwiki.yaml" --deepwiki
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$codex/deepwiki.yaml"
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-system rollout restart deployment/sproozi-gateway
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-system rollout status deployment/sproozi-gateway --timeout=180s
```

Restart the remote executor and the service with the same protected directories, then resume a
retained session with `sproozi resume SESSION_ID --config FILE --detach`.
The fresh grant has ten `mcp.deepwiki` calls. Run `mcp-probe.js` through the exact
fixture's active agent, first with `docker exec -i CONTAINER_ID node`, then with
`kubectl --kubeconfig FILE -n sproozi-agents exec -i POD -c agent -- node` after a
live move to `remote`. Feed the script on stdin. Select the container/Pod from the
trusted execution record, never from an unrelated context or name match.

The administrator probe uses the actual native process's proxy environment
without printing it. It calls the real `read_wiki_structure` tool for
`modelcontextprotocol/go-sdk`, then verifies wrong-repository, wrong-tool and
direct-provider denials. The calls are transport probes under real worker
identity; they do not establish autonomous model selection of that tool. Move
back locally and stop. Inspect the retained finalized-usage receipts for actual admitted
call units and immutable remaining allowance. Duplicate allowed probes consume
additional units; denied calls do not.

Stop saved fixture process PIDs and delete only this dedicated Kind cluster using
the mobility cleanup instructions. Keep reports and protected journals until you
have reviewed failed-operation stop and accounting evidence. A missing Pod or
expired deadline does not establish a refund.
