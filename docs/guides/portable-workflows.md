# Start remote work and continue the same session locally

Use this workflow when Hermes or the CLI should start a Codex task in Kubernetes,
retain its native conversation and Git workspace, and let you continue it later
in local Docker. The remote task service runs independently of the laptop. Pull
does not start a writer; resume obtains a new bounded execution grant.

The initial task harness is Codex 0.161.0. Existing Codex and Claude interactive
live moves remain documented in the [session guide](session-mobility.md).
Automated Claude tasks, IDE/GUI attachment, background tools, multiplayer and
existing personal-session adoption are not part of this workflow.

## Prepare the installation

1. Follow [getting started](getting-started.md) for the controller, authenticated
   gateway and enforcing CNI. Use a dedicated Kind cluster for evaluation; use
   [the Kubernetes installation guide](install-kubernetes.md) for an operator-owned
   cluster. Do not use a personal current kubeconfig context implicitly.
2. Build the pinned native image and provision its managed runtime/template/policy
   using [the complete mobility setup](../../hack/verify/session-mobility/README.md).
   The image contains Git, the native supervisor, checkpoint helper and
   `/usr/local/bin/session-profile`. The profile service needs private durable
   storage and administrator-owned Kubernetes credentials; workers and Hermes do
   not receive those credentials.
3. Configure the [home daemon](../../cmd/sproozi/README.md) with one private JSON
   file. It selects local Docker images/network, gateway TLS/provider credential
   files, finite allowances and named remote profiles. Keep it outside any
   transferred repository. Use `--config FILE` consistently or put it at the
   platform's user configuration directory under `sproozi/config.json`.
4. Configure scoped GitHub access through the existing GitHub App gateway for
   engineering work. Use approved repositories/base branches and finite budgets.
   The worker gets a placeholder `GH_TOKEN`, never the App private key. Add the
   corresponding `gateway.github` block and `github.pull_request` allowance to
   the home configuration when local continuation should publish work.

Keep provider credentials in owned files with mode `0600`, referenced by the
gateway configuration. For the first live engineering run, save an API key with
your local secret tooling and configure `providerTokenFile`; do not paste it into
chat, put it in a task or commit it. Provider keys stay in the gateway. The
documented fixture uses a public deterministic placeholder and establishes
execution mechanics, not paid inference or a genuine software contribution.

## Enable retained remote tasks

For Sproozi engineering tasks, prepare an image containing the Go toolchain and
the repository's public module cache **before** provisioning the fixed local and
remote placements. The stock native image contains Git and the clients, but no
Go compiler. Build the [engineering image example](../../examples/portable-workflows/sproozi.Dockerfile)
from this checkout with your resolved native image:

```sh
docker build --build-arg NATIVE_IMAGE=APPROVED_NATIVE_IMAGE \
  -f examples/portable-workflows/sproozi.Dockerfile \
  -t YOUR_ENGINEERING_IMAGE .
```

Resolve and retain the resulting Docker content ID for the local configuration
and canonical registry digest for the remote runtime. Use this image for the
agent and existing compatible helper/anchor roles from the start of the
installation; do not replace an existing execution's immutable image binding.
The inherited supervisor/native pins stay fixed. Go uses image-owned cached
modules with `GOPROXY=off`, a local toolchain and ephemeral writable build state.
Build state lives under `/workspace/sproozi-go-build`, outside the transferred
`/workspace/session` pair; Docker's `/tmp` remains small and non-executable.
Approve sufficient workspace and memory for compilation before installation:
for example, a 2 GiB workspace and 3 GiB agent memory, with matching remote
runtime requests/limits and policy ceilings. The small mobility fixture's
defaults are for short native conversations, not cold repository compilation.
Missing or changed dependencies require an operator-built image or separately
approved package access. Module/build caches are not conversation checkpoints.

This prepares ordinary Go unit/build checks such as `go test ./internal/work
./internal/session/...`. Full envtest, race and lint checks require their own
prepared tools and assets; hosted CI remains a separate verification gate.

For local GitHub continuation, merge the following into the full home configuration:

```json
{
  "allowances": {
    "github.pull_request": {
      "units": {"bounded": true, "value": 30},
      "costMicros": {"bounded": false, "value": 0}
    }
  },
  "gateway": {
    "github": {
      "appID": 123,
      "installationID": 456,
      "privateKeyFile": "/private/sproozi/github-app.pem",
      "scope": {
        "repositories": ["andrewmccall/sproozi"],
        "allowedBaseBranches": ["feat/cli-harnesses"]
      }
    }
  }
}
```

Replace the numeric IDs and scope with your approved App installation. Use the
[GitHub App setup](manual-pr-demo.md) for its required repository permissions
and protected key file. The inspection certificate must additionally cover
`github.com` and `api.github.com`, retaining its model and gateway-IP SANs.
For remote work, configure the same approved repository/base scope and finite
`github.pull_request` budget on the managed AgentPolicy, and provision the App
credentials only in the shared gateway as described in that setup.

Add `workTokenFile` and approved repositories to the existing private
`session-profile` configuration:

```json
{
  "workTokenFile": "/private/sproozi/work-token",
  "repositories": {
    "sproozi": {
      "url": "https://github.com/andrewmccall/sproozi",
      "ref": "feat/cli-harnesses"
    }
  }
}
```

These fields extend the full profile configuration, not a standalone replacement.
Generate a distinct task token locally with `openssl rand -hex 32`, save it with
mode `0600`, and reference that file. The existing profile principal token belongs
to the trusted home and additionally permits pull/claim. Hermes receives only the
task token. Do not reuse the same bytes for both roles.

An approved repository can instead specify an absolute `snapshot` path to a
stopped ordinary Git checkout owned by the administrator. The owner captures it
before dispatch. A URL source starts from a controlled seed repository and asks
the native worker to fetch that approved source through its GitHub capability.
The repository's files never supply launch instructions or credential references.

Run `session-profile --config FILE` with the explicit administrator kubeconfig,
or deploy `/usr/local/bin/session-profile --config /etc/sproozi/profile.json` in a
trusted Pod with the native image, a private persistent directory, its TLS/token
files and narrowly scoped placement credentials. Use one replica and an owner
lock; restarting it recovers the same stored execution intents. Its placement
credentials need the existing managed Run/registry and exact Pod-control access.
It is not a worker-facing arbitrary exec service.

Put the profile's HTTPS origin, public CA and home principal token file in the
global home configuration's `remote` map. The map name must equal the configured
profile `placementName`. `workSources` can use separately provisioned owner
connections; when omitted it uses the same named remote profiles. Both transports
verify TLS 1.3, use one fixed origin and reject redirects.

For a persistent in-cluster owner and the exact Hermes `/work/mcp` configuration,
use [Deploy Hermes with retained native work](retained-work-kubernetes.md).

## Run and inspect work

Build the public CLI:

```sh
go build -o bin/sproozi ./cmd/sproozi
go build -o bin/session-profile ./cmd/session-profile
```

Create a task file describing a reproducible problem and observable checks. Then
submit against the explicit configured profile name:

```sh
bin/sproozi run --config /private/sproozi/config.json \
  --executor engineering --harness codex --repository sproozi \
  --task-file task.txt --operation ORIGINAL_REQUEST_UUID
bin/sproozi sessions --config /private/sproozi/config.json --executor engineering
bin/sproozi status SESSION_ID --config /private/sproozi/config.json --executor engineering
```

Preserve the request UUID when retrying. Repeating identical intent returns the
same session and execution; changed intent is rejected. The session ID outlives
the Pod. Status returns lifecycle and a bounded final answer labelled as untrusted
workload output. `retained` requires compatible native completion, exact writer
termination, complete sealed accounting and a stored checkpoint. An `unknown`
status cannot be pulled or silently interpreted as stopped.

For a known recoverable interruption, use `sproozi recover SESSION_ID --executor
engineering --config FILE`. Cancel uses a separate stable `--operation UUID` and
revokes before forced stop. Keep the original owner journals and accounting
records; deleting resources or replacing a failed Run does not establish stop or
refund evidence.

## Connect Hermes

Use the existing persistent [Hermes deployment and native MCP setup](hermes-orchestration.md).
Register the profile's `/work/mcp` URL with its task bearer token and public CA.
The supported tools are `sproozi_run`, `sproozi_status`, `sproozi_sessions` and
`sproozi_session_info`. Hermes supplies this intent, without images, PodSpecs,
Kubernetes tokens or native-state paths:

```json
{
  "requestId": "ORIGINAL_REQUEST_UUID",
  "harness": "codex",
  "repository": "sproozi",
  "executor": "engineering",
  "task": "Reproduce the supplied failing case, make the smallest fix, run its checks and explain the result."
}
```

Poll `sproozi_status` with the returned `id`. A scheduling prompt must preserve
the same request UUID on transport retries. A new request UUID is a deliberately
new task. The ordinary `submit/status/wait/cancel` facade remains available for
the older disposable assistant-job workflows; it does not provide portable
native state.

## Pull and resume locally

Start the home service, then transfer the completed session:

```sh
bin/sproozi serve --config /private/sproozi/config.json
# In another terminal:
bin/sproozi pull SESSION_ID --config /private/sproozi/config.json \
  --from engineering --operation ORIGINAL_PULL_UUID
bin/sproozi resume SESSION_ID --config /private/sproozi/config.json
```

Pull stores the validated native/Git checkpoint inertly, then commits the remote
owner's one-way claim. A lost acknowledgement retries the same pull operation;
downloaded bytes alone cannot activate a writer. The remote catalogue now reports
`home-owned` and cannot reopen that task session. Keep the home configuration and
private directory unique to this trusted installation; copying its principal
credential into independent homes is unsupported.

The home catalogue enforces `local.retainedBytes` and a finite file count for
imported artifacts and journals; fresh continuations also use the existing
initial-execution retention checks. A capacity error leaves downloaded state
inert and blocks admission. Restore capacity and retry the original operation.
Keep protected owner/receipt journals intact; deleting them is not recovery.

Resume opens the real native TUI with the same conversation UUID and supported
Git history, index, staged/unstaged changes and selected files. Its new deadline,
allowances and gateway identity come from current home approval. Earlier receipts
remain unchanged. `--detach` starts without attaching; `attach --socket PATH
--id SESSION_ID` reconnects, and Ctrl-] detaches without stopping work.

After reviewing and changing the work, use `sproozi stop SESSION_ID --config FILE`
to wait for the current foreground turn/tools, fence the writer, seal usage and
retain the new checkpoint. It does not launch a destination. Later resume starts
another independently authorized execution. Use the existing `move` command for
live visits within one execution; its original deadline and aggregate allowance
continue across those visits.
Keep the original operation UUID when retrying. Product operations remain bound
to the execution they addressed; an old stop, move or cancel request cannot act
on a later resume, even though its product session ID is the same.

For GitHub publication, create a branch for the current execution:

```sh
git switch -c "sproozi/$SPROOZI_EXECUTION_UID/review"
git push -u origin HEAD
gh pr create --base APPROVED_BASE_BRANCH
```

The old branch can remain in local history, but its name is not authority to
overwrite another execution's published branch. Confirm the exact changes and
checks before publishing. A successful infrastructure task is not evidence that
its proposed fix is correct or merged.

## Home-ops setup and use

Use the same task/pull/resume flow with an administrator-approved read-only
profile. Select one intended API endpoint and namespace/resource allowlist.
`kubernetes.read` covers supported Pods, logs, events, Services and apps workloads;
it does not grant writes, arbitrary contexts or Flux custom-resource access.

For the local gateway, add `gateway.kubernetes` with `upstreamURL`, private
`tokenFile`, public `caFile` and `scope` containing approved namespaces/resources.
Use a restricted read-only credential. The worker's regenerated kubeconfig points
to the inspected `kubernetes.default.svc` authority with a placeholder token;
the gateway supplies its own credential for the configured API. The remote
gateway's ordinary Kubernetes handler targets its configured execution cluster.
For a different home-ops cluster, provision an explicitly reviewed target path
instead of treating every kubeconfig context as available.

For example, extend the private home configuration with:

```json
{
  "allowances": {
    "kubernetes.read": {
      "units": {"bounded": true, "value": 50},
      "costMicros": {"bounded": false, "value": 0}
    }
  },
  "gateway": {
    "kubernetes": {
      "upstreamURL": "https://APPROVED_API_HOST:6443",
      "tokenFile": "/private/sproozi/kubernetes-read-token",
      "caFile": "/private/sproozi/kubernetes-ca.pem",
      "scope": {
        "namespaces": ["APPROVED_NAMESPACE"],
        "resources": ["pods", "pods/log", "events", "services", "deployments"]
      }
    }
  }
}
```

Merge these fields with the existing model/gateway settings; this is not a full
replacement configuration. Issue the token from a dedicated read-only
ServiceAccount whose RBAC covers only these resources and namespaces. Save it
directly to the protected file using your secret tooling. Extend the inspection
certificate with the `kubernetes.default.svc` DNS SAN, while retaining its model
and gateway-IP SANs. The configured upstream CA validates the real API separately.
Restart the home gateway to apply its fixed target/credential configuration;
changing config files does not rewrite a running execution's grant.

Ask the agent to report observed deployment state, relevant logs/events and the
limits of its diagnosis. Pull the retained session, resume locally and ask follow-up
questions. A remediation through GitOps requires a fresh, separately approved
repository grant and a reviewed proposed change. Direct Kubernetes mutation is
not supported by `kubernetes.read`; do not supply cluster-admin to make it work.
This guide describes the setup. Verification uses isolated fixture infrastructure
and does not access or change your personal home network.

## One external MCP definition across executors

The [DeepWiki example](../../examples/portable-workflows/deepwiki-servers.json)
registers the real public HTTPS provider without credentials. Use that same
authored registry file for `gateway.mcpRegistryFile` locally and the remote
gateway's mounted registry ConfigMap. Set the local inspection authority and
certificate SANs according to the [MCP reference](../reference/configured-mcp.md).
The policy example permits only `read_wiki_structure` for the public
`modelcontextprotocol/go-sdk` repository, with a finite `mcp.docs` allowance.
Keep the local copy outside the repository, owned and mode `0600`. Both copies
must contain the same registration bytes; restart gateways after registry edits.
The [isolated verification setup](../../hack/verify/portable-workflows/README.md)
renders both copies and their matching policy from these examples.

Regenerate connection settings at each native launch. Registry configuration does
not itself grant a running execution: matching named capability, tool/argument
policy, budget and active lifecycle are required. Restart the owning gateways to
apply registry or credential changes; removing a grant must deny later calls.
Codex managed-native MCP is supported; managed Claude MCP remains gated pending
its client/tool completion proof. No provider-specific server code, catalogue,
OAuth enrollment or stdio lifecycle is required for this HTTPS example.

## Verify and interpret the result

The [portable workflow driver](../../hack/verify/portable-workflows/verify.py)
extends the dedicated mobility fixture. It checks authenticated MCP submission,
stable replay identity, a genuine remote native task while the home is absent,
retained output, one-way claim, fresh local authorization and same-conversation
follow-up. Its report states provider and caller limits. Unit tests separately
exercise lost replies, unknown fence/accounting, new grants and no destination
activation during stop. The subsequent stock Hermes fixture passed native MCP
dispatch and retained-answer retrieval, followed by fresh same-conversation local
continuation. [Recorded acceptance](../demos/verified-portable-workflows.md)
separates it from the earlier protocol-only caller. Paid inference, a real
engineering contribution and personal home-ops diagnosis remain unassessed.

## Projects and exact revisions

See the [configuration and caller reference](../reference/projects-and-revisions.md) for optional
Project grouping, installation membership, independent local approval and fresh
workspace-only execution at a retained Git revision. Standalone work remains
valid. Native resume still restores the complete supported unfinished workspace.
