# Run a task remotely, then resume it locally

Start a Codex task in Kubernetes through Hermes or the CLI. Sproozi saves its
conversation and Git workspace so you can resume it later in local Docker. The
[remote executor](../../GLOSSARY.md#remote-executor) that runs the work in
Kubernetes keeps going independently of your laptop.

The workflow has four steps:

1. Submit a task to an approved remote executor.
2. Wait for completion and inspect the saved result.
3. Pull the stopped session to your local installation.
4. Resume the same conversation with current local permissions.

Pull does not start an agent. Resume starts a new execution with a new
deadline and allowance. See [core concepts](../explanations/core-concepts.md)
for the difference between a session and an execution.

This workflow supports Codex. Here the remote executor owns the task until you
pull it, and nobody attaches while it runs. To work interactively in a Codex or
Claude Code session and move it between Docker and Kubernetes while your home
daemon stays the owner, use
[Move a live session between Docker and Kubernetes](session-mobility.md). Automated
Claude tasks, IDE/GUI attachment, background tools, multiplayer and existing
personal-session adoption are not part of this workflow.

## Prepare the installation

1. Follow the [investigation-to-PR demo on Kind](pr-demo.md) for the
   controller, authenticated gateway and enforcing CNI. Use a dedicated Kind
   cluster for evaluation; use [Install from a checkout](install-from-checkout.md)
   for an operator-owned cluster. Do not use a personal current kubeconfig context implicitly.
2. Build the selected native image and provision its managed runtime/template/policy
   using [the complete mobility setup](../../hack/verify/session-mobility/README.md).
   The image contains Git, the native supervisor, checkpoint helper and
   `/usr/local/bin/session-profile`. The remote executor needs private durable
   storage and administrator-owned Kubernetes credentials; sandboxes and Hermes
   do not receive those credentials.
3. Configure this machine's [service](../../cmd/sproozi/README.md) with one private JSON
   file. It selects local Docker images/network, gateway TLS/provider credential
   files, finite allowances and named remote executors. Keep it outside any
   transferred repository. Use `--config FILE` consistently or put it at the
   [default path](../reference/session-commands.md#command-flags). Your
   [profile](../reference/profile.md) is the `profile` folder beside it.
4. Configure scoped GitHub access through the existing GitHub App gateway for
   engineering work. Use approved repositories/base branches and finite budgets.
   The sandbox gets a placeholder `GH_TOKEN`, never the App private key. Add the
   corresponding `gateway.github` block and `github.pull_request` allowance to
   the home configuration when the resumed local agent should publish work.

Keep provider credentials in owned files with mode `0600`, referenced by the
gateway configuration. For the first live engineering run, save an API key with
your local secret tooling and configure `providerTokenFile`; do not paste it into
chat, put it in a task or commit it. Provider keys stay in the gateway. The test setup uses a placeholder credential. Replace it with your provider
credential for normal use.

## Enable retained remote tasks

For tasks that compile Go code, also [prepare a Go development
image](#prepare-a-go-development-image) before you configure the local and
remote environments. The standard native image contains Git and the agent
clients, but no Go compiler.

To use GitHub after local resume, merge the following into the full home configuration:

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

Replace the numeric IDs and scope with your approved App installation. Step 5 of
[Getting started](pr-demo.md#5-create-a-github-app) gives its required
repository permissions and protected key file. The inspection certificate must additionally cover
`github.com` and `api.github.com`, retaining its model and gateway-IP SANs.
For remote work, configure the same approved repository/base scope and finite
`github.pull_request` budget on the managed AgentPolicy, and provision the App
credentials only in the shared gateway as described in that setup.

Add `workTokenFile` and approved repositories to the remote executor's
configuration:

```json
{
  "workTokenFile": "/var/run/sproozi-work/work-token",
  "repositories": {
    "sproozi": {
      "url": "https://github.com/andrewmccall/sproozi",
      "ref": "feat/cli-harnesses"
    }
  }
}
```

These fields extend the full executor configuration, not a standalone
replacement. Generate the work token with `openssl rand -hex 32` and put it in
the executor's credentials Secret. Hermes receives only the work token. Your
own CLI and service sign in as the `sproozi-cli` ServiceAccount instead, which
also permits pull.

An approved repository can instead specify an absolute `snapshot` path to a
stopped ordinary Git checkout owned by the administrator. The remote executor
captures it before dispatch. A URL source starts from a controlled seed repository and asks
the harness to fetch that approved source through its GitHub capability.
Repository files cannot supply trusted launch instructions or credential
references.

Deploy the remote executor in the cluster with the
[retained-work example](../../examples/portable-workflows/kubernetes/owner.yaml):
one replica, a private persistent volume, its TLS and work-token Secret and its
placement permissions. A lock on its state directory rejects a second executor
using the same directory, and restarting it recovers the stored execution
intents. Sandboxes cannot use it to execute arbitrary commands.

You reach it through a kubeconfig context. Apply `cli-access` and create the
`sproozi` context as [remote executor access](../reference/remote-executors.md)
shows; that page also lists what the executor must publish.

For a persistent in-cluster remote executor and the exact Hermes `/work/mcp` configuration,
use [Hermes with remote work](retained-work-kubernetes.md).

## Run and inspect work

[Install the CLI](install.md). Build `session-profile` and the images from a
checkout at the same release as the CLI, as described in
[Move a live session between Docker and Kubernetes](session-mobility.md#prepare-the-administrator-owned-setup).

Create a task file describing a reproducible problem and observable checks. Then
submit it; without `--context`, the `sproozi` context is used:

```sh
sproozi run --remote --harness codex --repository sproozi --task-file task.txt
sproozi sessions
sproozi status ID --context sproozi
```

Add `--context NAME` or `--context FILE` to reach another cluster, and
`--operation UUID` to retry a submission whose outcome is unknown.

Preserve the request UUID when retrying. Repeating identical intent returns the
same session and execution; changed intent is rejected. Public `id` addresses
the stable session. `executionId`, `allocationId` and `incarnation` identify the
execution, visit and exact writer separately. The session ID outlives the Pod.
Status returns the task phase and a size-limited final answer marked as
untrusted output. The remote executor reports `retained` only after compatible native
completion, confirmed sandbox stop, finalized usage accounting and checkpoint
storage. An `unknown` status cannot be pulled or silently interpreted as stopped.

`sessions --json` returns one labelled catalogue. Check `executors` for
unavailable owners and `truncated` for omitted results; a stopped local service
does not hide the cluster's rows. The [command reference](../reference/session-commands.md)
explains owner selection, JSON fields and safe error categories.

For a known recoverable interruption, use `sproozi recover ID --context sproozi`. Cancel uses a separate stable `--operation UUID` and
revokes before forced stop. Keep the original owner journals and accounting
records; deleting resources or replacing a failed Run does not establish stop or
refund evidence.

## Connect Hermes

Use the persistent Hermes deployment and native MCP setup from
[Run a persistent Hermes assistant on Kubernetes](hermes-orchestration.md).
Register the executor's `/work/mcp` URL with its work token as the bearer token
and its public CA.
The supported tools are `sproozi_run`, `sproozi_status`, `sproozi_cancel` and
`sproozi_sessions`. Hermes supplies this intent, without images, PodSpecs,
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
new task. The task service's `submit/status/wait/cancel` tools remain available for
delegated runs that keep only a final answer; they don't provide portable
native state.

## Pull and resume locally

With this machine's service running, transfer the completed session:

```sh
sproozi pull ID
sproozi resume ID
```

`pull` uses the `sproozi` context unless `--context` names another. The
service reaches the cluster itself and remembers it. After the pull this machine
owns the session, so `resume` needs no `--context`.

Pull validates and saves the conversation and Git checkpoint without starting an
agent. It then records the remote executor's one-way ownership transfer. If the
reply is lost, run the same command again: a pull the service already began
continues from the same executor with its original operation. Downloading a
checkpoint alone
does not permit local execution. The remote catalogue now reports `home-owned`
and cannot reopen that task session. One executor serves one developer's
service: two services signed in to the same executor share its single
principal, which is unsupported.

The home catalogue enforces `local.retainedBytes` and a finite file count for
imported artifacts and journals; new executions also use the existing
initial-execution retention checks. A capacity error keeps downloaded state
stopped and prevents a new execution. Restore capacity and retry the original
operation. Keep protected owner/receipt journals intact; deleting them is not
recovery.

Resume opens the real native TUI with the same harness conversation ID (`threadId`) and supported
Git history, index, staged/unstaged changes and selected files. Its new deadline,
allowances and gateway identity come from current home approval. Earlier receipts
remain unchanged. `--detach` starts without attaching; `sproozi attach ID`
reconnects, and Ctrl-] detaches without stopping work.

After reviewing and changing the work, use
`sproozi stop ID --config FILE` to wait for the current
turn and foreground tools, stop the sandbox, finalize usage accounting and save a
new checkpoint. It does not launch a destination. A later resume starts another
execution with current approval. Use `move` for live visits within one
execution; its original deadline and aggregate allowance continue across those
visits. Keep the original operation UUID when retrying. Product operations stay
bound to the execution they addressed. An old stop, move or cancel request
cannot act on a later resume, even though its session ID is the same.

For GitHub publication, create a branch for the current execution:

```sh
git switch -c "sproozi/$SPROOZI_EXECUTION_UID/review"
git push -u origin HEAD
```

The old branch can remain in local history, but its name is not authority to
overwrite another execution's published branch. The pinned gh client's
`gh pr create` uses unsupported GraphQL fragments and is denied by the current
bounded gateway contract. Publish through its supported REST operation instead:

```sh
gh api --method POST repos/OWNER/REPO/pulls \
  -f title='Describe the reviewed change' -f head="$(git branch --show-current)" \
  -f base=APPROVED_BASE_BRANCH -f body='Describe the change and completed checks'
```

Confirm the exact changes and
checks before publishing. A successful infrastructure task is not evidence that
its proposed fix is correct or merged.

## Prepare a Go development image

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
The image inherits the harnesses and supervisor from its approved base. Go uses image-owned cached
modules with `GOPROXY=off`, a local toolchain and ephemeral writable build state.
Build state lives under `/workspace/sproozi-go-build`, outside the transferred
`/workspace/session` pair; Docker's `/tmp` remains small and non-executable.
Approve sufficient workspace and memory for compilation before installation:
for example, a 2 GiB workspace and 3 GiB agent memory, with matching remote
runtime requests/limits and policy ceilings. The small session setup's defaults are intended for short conversations.
Increase them before compiling the repository.
Missing or changed dependencies require an operator-built image or separately
approved package access. Module/build caches are not conversation checkpoints.

This prepares ordinary Go unit/build checks such as `go test ./internal/work
./internal/session/...`. Full envtest, race and lint checks require their own
prepared tools and assets; hosted CI remains a separate verification gate.

## Home-ops setup and use

Use the same task/pull/resume flow with an administrator-approved read-only
profile. Select one intended API endpoint and namespace/resource allowlist.
`kubernetes.read` covers supported Pods, logs, events, Services and apps workloads;
it does not grant writes, arbitrary contexts or Flux custom-resource access.

For the local gateway, add `gateway.kubernetes` with `upstreamURL`, private
`tokenFile`, public `caFile` and `scope` containing approved namespaces/resources.
Use a restricted read-only credential. The sandbox's regenerated kubeconfig points
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
Use only the approved endpoint and namespaces in your home configuration.

## One external MCP definition across executors

The [DeepWiki example](../../examples/portable-workflows/deepwiki-servers.json)
registers the real public HTTPS provider without credentials. Use that same
authored registry file for `gateway.mcpRegistryFile` locally and the remote
gateway's mounted registry ConfigMap. Set the local inspection authority and
certificate SANs according to the [MCP reference](../reference/configured-mcp.md).
`gateway.mcpAuthority` is required even without a registry: it also serves the
[Sproozi control connection](../reference/sproozi-integration.md).
The policy example permits only `read_wiki_structure` for the public
`modelcontextprotocol/go-sdk` repository, with a finite `mcp.docs` allowance.
Keep the local copy outside the repository, owned and mode `0600`. Both copies
must contain the same registration bytes; restart gateways after registry edits.
Use the same registration and policy on both gateways.

Regenerate connection settings at each native launch. Registry configuration
alone does not grant access. Each execution needs the named capability, matching
tool and argument policy, an available budget and active run status. Restart the
owning gateways to apply registry or credential changes; removing a grant must
deny later calls. Codex managed-native MCP is supported; managed Claude MCP
is not supported. No provider-specific
server code, catalogue, OAuth enrollment or stdio lifecycle is required for this
HTTPS example.

## Projects and exact revisions

See the [configuration and caller reference](../reference/projects-and-revisions.md) for optional
Project grouping, installation membership, independent local approval and fresh
workspace-only execution at a retained Git revision. Standalone work remains
valid. Native resume still restores the complete supported unfinished workspace.
