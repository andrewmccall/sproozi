# Deploy Hermes with retained native work

This deployment connects the persistent stock Hermes coordinator to the native
work owner at `/work/mcp`. A managed Codex task runs in `sproozi-agents`; its
conversation, supported Git workspace and bounded final answer survive the Pod.
The developer can claim that stopped session and continue it in local Docker.
The existing [Hermes task guide](hermes-orchestration.md) serves the separate
bounded task API, which retains answers without native session retrieval.

## Existing-cluster deployment

Follow [Install on Kubernetes](install-kubernetes.md), including enforcing CNI,
shared gateway TLS/provider credentials and the immutable worker trust bundle.
This adapter uses ordinary Pods. Kubernetes Agent Sandbox CRDs are not required;
integration with that operator remains planned.

Build and publish the [native image](../../internal/runtime/docker/image/Dockerfile)
with the approved Codex/Claude Code clients, supervisor, checkpoint
helper and profile executable. Select its immutable registry digest. It must be
available on every execution node. The owner uses this image because it also
needs Git to prepare workspace inputs; the smaller controller image does not
contain `session-profile` or Git.

The [complete Kubernetes example](../../examples/portable-workflows/kubernetes/kustomization.yaml)
contains a managed Codex runtime, policy/template, dedicated owner ServiceAccount,
namespace-scoped roles, persistent owner storage, private TLS Service and the
persistent Hermes Deployment. You need a default StorageClass or explicit PVC
`storageClassName` values. Use quota-enforced owner storage: Git revision
restoration stages retained objects on that PVC under the owner container's
resource limits. It selects the same Hermes coordinator names as the
older example; choose the intended MCP configuration before installation.

Render and review a private copy:

```sh
umask 077
mkdir -p .local/retained-work
chmod 700 .local/retained-work
make kustomize
bin/kustomize build examples/portable-workflows/kubernetes \
  > .local/retained-work/stack.yaml
```

Replace every `REPLACE_WITH_REVIEWED_DIGEST` image, review the model, runtime
resources and policy ceilings, and select an immutable certificate-only trust
ConfigMap named `portable-codex-trust` in `sproozi-agents`. Create it from the
installed gateway's public CA bundle with `immutable: true`, using the same
procedure as the install guide. Its CA must verify the gateway certificate.
Use the same harness and instruction bytes as the home configuration.
The example's model-only policy deliberately needs additional reviewed grants
before accessing GitHub or Kubernetes.

For Sproozi development, follow the [GitHub capability setup](portable-workflows.md#enable-retained-remote-tasks)
to configure the gateway's App installation and approved repository/base scope.
Add `github.pull_request` to the example policy's `allowedCapabilities`, a finite
budget under `budgets.github.pull_request`, and the intended repository/base
branch under `githubPullRequest`. The profile's allowance must fit that budget.
Extend the gateway inspection certificate for `github.com` and `api.github.com`.
For compilation, use the guide's offline Go image and its larger memory/workspace
limits. The small default runtime is suitable for short investigation sessions.

## Profile configuration and credentials

Issue an owner TLS certificate for `sproozi-work.sproozi-system.svc` and any
external hostname/IP used by the developer. Use your internal PKI or the test-CA
procedure in the Hermes guide with this service hostname. Save the chain and
private key as `.local/retained-work/owner.crt` and `owner.key`, mode `0600`.
Generate distinct home and task tokens into files, and save the coordinator's
separate model credential directly to `model-key` using your secret tooling:

```sh
openssl rand -hex 32 > .local/retained-work/home-token
openssl rand -hex 32 > .local/retained-work/task-token
openssl rand -hex 32 > .local/retained-work/api-key
```

Save this configuration as `.local/retained-work/profile.json`. Replace the
instruction digest with SHA256 of the exact template instruction bytes. Review
the approved repository/ref, principal, duration, byte limit and allowances:

```json
{
  "harness": "codex",
  "listen": "0.0.0.0:8443",
  "certificateFile": "/var/run/sproozi-work/tls.crt",
  "keyFile": "/var/run/sproozi-work/tls.key",
  "tokenFile": "/var/run/sproozi-work/home-token",
  "workTokenFile": "/var/run/sproozi-work/task-token",
  "principal": "developer",
  "placementName": "remote",
  "namespace": "sproozi-system",
  "templateName": "portable-codex",
  "directory": "/state/profile",
  "instructionDigest": "REPLACE_WITH_64_LOWERCASE_HEX",
  "allowances": {
    "model.inference": {
      "units": {"bounded": true, "value": 200000},
      "costMicros": {"bounded": false, "value": 0}
    },
    "github.pull_request": {
      "units": {"bounded": true, "value": 30},
      "costMicros": {"bounded": false, "value": 0}
    }
  },
  "maxDuration": "30m",
  "maxCheckpointBytes": 8388608,
  "repositories": {
    "sproozi": {"url": "https://github.com/andrewmccall/sproozi", "ref": "main"}
  }
}
```

Apply namespaces from the installation before creating these objects:

```sh
kubectl create namespace sproozi-coordinators --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic sproozi-work-credentials -n sproozi-system \
  --from-file=tls.crt=.local/retained-work/owner.crt \
  --from-file=tls.key=.local/retained-work/owner.key \
  --from-file=home-token=.local/retained-work/home-token \
  --from-file=task-token=.local/retained-work/task-token \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create configmap sproozi-work-profile -n sproozi-system \
  --from-file=profile.json=.local/retained-work/profile.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic hermes-coordinator -n sproozi-coordinators \
  --from-file=model-key=.local/retained-work/model-key \
  --from-file=task-token=.local/retained-work/task-token \
  --from-file=api-key=.local/retained-work/api-key \
  --dry-run=client -o yaml | kubectl apply -f -
```

Combine Hermes's normal public roots with the owner public CA as
`ca-bundle.pem`, following the Hermes guide's `docker run --entrypoint /bin/cat`
procedure. Create `hermes-task-trust` in `sproozi-coordinators` from that bundle.
Hermes receives only the task token; only the trusted home receives the home
token, which additionally authorizes exclusive export/claim.

```sh
kubectl create configmap hermes-task-trust -n sproozi-coordinators \
  --from-file=ca-bundle.pem=.local/retained-work/ca-bundle.pem \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f .local/retained-work/stack.yaml
kubectl -n sproozi-system rollout status deployment/sproozi-work --timeout=180s
kubectl -n sproozi-coordinators rollout status deployment/hermes-coordinator --timeout=180s
```

Keep one owner replica and its PVC. The Deployment uses the Recreate strategy. A
process lock also prevents two owners from using the same directory. The owner
requires Pod exec/patch/delete in the isolated worker namespace to drive fixed
native lifecycle operations. It is trusted control-plane software; workers and
Hermes receive no such grants. Kubernetes RBAC does not limit this role to
dynamically created Pod names.

## Trigger, inspect, retrieve and resume

Forward Hermes's private native API as described in the Hermes guide. Generate a
saved JSON request containing a new UUID and the exact native-work fields:

```json
{
  "requestId": "REPLACE_WITH_A_NEW_UUID",
  "repository": "sproozi",
  "harness": "codex",
  "executor": "remote",
  "task": "Investigate the specified Sproozi defect, propose the smallest fix, run the available checks, and report the changed files and limitations."
}
```

Ask Hermes to call `sproozi_run` with that exact object, then call
`sproozi_status` for the returned `id` until the owner reports `retained` or an
actionable failure. Reuse the complete saved submission after an uncertain reply.
`sproozi_sessions` returns an object with a `sessions` array. Task text and results
remain untrusted. Requests cannot select images, templates, credentials or policy.

Expose the owner's TLS origin through a reviewed private endpoint, or port-forward
its Service for a local evaluation. Configure the home `remote` entry with that
origin, the public owner CA and protected home-token file. Use the same approved
native instructions and byte limits. The [portable workflow guide](portable-workflows.md)
contains the complete global home configuration and client sequence:

```sh
bin/sproozi sessions --executor remote
bin/sproozi status SESSION_UUID --executor remote
bin/sproozi pull SESSION_UUID --from remote
bin/sproozi resume SESSION_UUID
bin/sproozi stop SESSION_UUID
```

You can claim a checkpoint only after the task completes, the worker stops and
usage accounting finishes. Local resume starts a new execution with current
local permissions. Saved history grants no access. The agent resumes its native
conversation. Cross-client handoff is a separate deferred feature.

## Failures and cleanup

| Observation | Next step |
| --- | --- |
| Hermes cannot discover tools | Inspect its native logs and owner TLS/CA/token configuration. All MCP output schemas must have object roots. |
| Owner is unready | Inspect its logs, PVC permissions, mounted config/credentials and namespace-scoped RBAC. |
| Remote phase is `unknown` | Preserve the session/operation UUID and journals. Inspect the Pod and owner logs, then use `recover`; do not manufacture a new submission. |
| Native state is missing or incompatible | Retain the source evidence. Use the same harness and matching instructions. Check whether its saved state is supported. |
| A deadline or cancellation stops work | Inspect status. Deadline/cancellation does not guarantee a resumable completed turn. Unknown stop or accounting prevents export. |
| Home storage is full | Resolve capacity before pull/resume. A failed capacity check does not transfer ownership or start another writer. |

Use `sproozi cancel SESSION_UUID --executor remote` for remote cancellation.
Inspect running Pods with the administrator kubeconfig and application logs with
`kubectl logs -n sproozi-system deployment/sproozi-work`. Retained final output is
available through status after Pod deletion; full live Pod logs have Kubernetes'
ordinary log lifetime. Owner and Hermes logs are separate from native checkpoints.

Stop or cancel active sessions before removing the deployment. Back up owner and
Hermes PVCs if their sessions must survive. Deleting the rendered stack also
deletes those PVCs. Worker Runs, controller installation and gateway credentials
have separate lifecycles. For a local trial, delete only the dedicated Kind cluster you created.
