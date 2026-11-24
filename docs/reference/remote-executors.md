# Remote executor access through a kubeconfig context

The CLI and this machine's Sproozi service reach a
[remote executor](../../GLOSSARY.md#remote-executor) through a kubeconfig
[context](../../GLOSSARY.md#context): the developer's Kubernetes identity finds
the executor in a namespace, opens a port-forward to its Pod and runs the
executor's TLS protocol over it. No load balancer, hand-copied CA or token file
is needed. [ADR 0036](../adr/0036-remote-executors-through-kubeconfig-contexts.md)
records why.

[Remote commands](session-commands.md#remote-commands) take `--context` and
`--namespace`. `run --remote`, `sessions --remote`, `status`, `cancel` and
`recover` reach the cluster from the CLI. `move --remote` and `pull` send the
selection to the service, which reaches the cluster from its own environment.
The executor also runs [interactive sessions](#interactive-sessions) of its
own.

## Selecting the context and namespace

| Input | Meaning |
| --- | --- |
| no `--context` | The context named `sproozi` in the default kubeconfig: the files in `KUBECONFIG`, else `~/.kube/config`. |
| `--context NAME` | That context, when the default kubeconfig defines a context with exactly this name. |
| `--context PATH` | Otherwise, an existing kubeconfig file; its current context is used. A context of the same name wins over a file. |
| `--namespace NS` | The executor's namespace. Without it, the context's namespace, else `default`. |

The service records each executor it reaches in `executors.json` in its
directory (mode `0600`): the executor's name, harness and checkpoint limit, and
the absolute kubeconfig files, context and namespace that reached it. It keeps
using them after the command exits and after a restart, and connects only when
a session needs the executor. A later `kubectl config use-context`, or a
different `KUBECONFIG` in the service's environment, does not change them. The
service reloads the files on each new connection, so a refreshed credential is used.
Exec credential plugins run in the service's environment, so their binaries
must be on its `PATH`. Reaching the same executor name through another context
or file points the record at that one.

`move ID --local` needs no context: the service already knows where the
session is. `pull ID` uses the default context unless `--context` names
another; a pull the service already began continues from its recorded executor
whatever the retry names.

The context's own credential goes only to the Kubernetes API server, never to
the executor. On each connection the CLI or service asks the API server for a
short-lived token for the `sproozi-cli` ServiceAccount in the executor's
namespace (a TokenRequest, valid for at most an hour), bound to the audience
`sproozi-remote-executor`, and sends the executor that token alone. The
executor's TokenReview accepts only that audience, and the API server does not
accept it, so an executor that is compromised or impersonated cannot replay a
caller's token against the cluster. The service connects again, minting a new
token, five minutes before the current one expires.

Any context that may create that token works: the `sproozi` context below, or
an administrator's context, including one that signs in with an exec plugin or a
client certificate. Whatever context you use, the executor sees the
`sproozi-cli` identity, and RBAC decides on that identity.

| Message begins | Fix |
| --- | --- |
| `the kubeconfig … has no context named "sproozi"` | Create the context as shown [below](#create-the-sproozi-context), or pass `--context`. |
| `--context "X" is neither a context in the kubeconfig … nor an existing kubeconfig file` | Pass a context from the listed names, or a path to a kubeconfig file. |
| `--context "X" was read as the kubeconfig file …, which sets no current context` | Run the printed `kubectl --kubeconfig FILE config use-context NAME`. |
| `no remote executor in namespace "NS"` | Pass `--namespace` with the executor's namespace, or ask the administrator to install it there. |
| `your Kubernetes user (…) may not …` | Pass `--namespace` if the executor is elsewhere; otherwise sign in as `sproozi-cli`, whose Role grants it. |
| `the cluster rejected the credentials of …` | The context's token is invalid or expired; write a new token into it. |
| `the remote executor … refused your Kubernetes user` | The identity lacks `use` on `remoteexecutors.sproozi.com`; bind the `sproozi-cli` Role. |
| `your Kubernetes user (…) may not create a token for the ServiceAccount sproozi-cli` | Bind the `sproozi-cli` Role, which grants `create` on `serviceaccounts/token` for `sproozi-cli`, or use a context that has it. |
| `namespace "NS" has no ServiceAccount sproozi-cli` | Ask the administrator to apply `cli-access` (`config/cli-access`, or `sproozi-release/cli-access/`) in that namespace. |
| `the remote executor … could not authenticate the sproozi-cli token` | The executor's TokenReview rejected the minted token. An executor older than this client does not accept the `sproozi-remote-executor` audience; ask the administrator to upgrade it. |
| `… has no ready Pod` | The executor is not running; check its Pods. |
| `the remote executor's certificate is not trusted` | The executor's certificate must be issued by the published CA and cover `SERVICE.NAMESPACE.svc`. |
| `the remote executor … accepts checkpoints up to N bytes, but local.maxCheckpointBytes is M` | Make the two limits equal. |
| `the remote executor is named "local", which is this machine's own name` | Ask the administrator to give the executor another `placementName`. |
| `the Sproozi service does not know which cluster holds session …` | Pass `--context` to `pull`. |

## Placement outcomes

The service and the executor upgrade together. A placement request that fails
answers with one of these codes; status and messages never carry the
executor's own error text.

| Code | HTTP status | Meaning |
| --- | --- | --- |
| `invalid_request` | 400, 404, 413, 415 | The request is malformed or names the wrong placement. |
| `denied` | 401, 403, 426 | The token or the Kubernetes user was refused. |
| `conflict` | 409 | The request conflicts with what the executor holds for that execution. |
| `expired` | 410 | The execution's deadline has passed. |
| `unsupported` | 422 | The executor cannot take this launch, such as a profile it does not keep. |
| `unavailable` | 503 | The executor could not answer. A stop is then unproven: the session becomes `unknown-stop` until `recover` or another `stop` proves it. |
| `turn_pending` | 503 | A stop is still waiting for the agent's turn, about two minutes after it asked. Its `waiting` field says what for, in at most 160 printable bytes. The session stays `stopping`, as on this machine, and `stop`, `move` or `recover` again keeps waiting. |

A code this Sproozi does not know counts as `unavailable`, so a stop is never
taken as proven. The message names the code and says `the executor is probably
newer than this Sproozi, so update Sproozi and restart its service`. A service
too old to know `turn_pending` reports `profile: invalid response` instead and
records `unknown-stop`; update the service, then `recover` resumes the stop.

### Cancelling a preparation

`abort-preparation` cancels a visit whose `prepare` never answered with an
incarnation, for example because its run waits under a concurrency cap. The
request carries only the launch, as `inspect` does; an incarnation or a
checkpoint is `invalid_request`. It is authorized like every placement
action and is never `expired`: cancelling is accepted after the deadline.
Success means no agent for that launch runs or can start; the answer has no
fields besides `version`. One request waits at most 30 seconds once no
other operation on the same visit holds it, under the service's 2-minute
wait for it ([request timeouts](#request-timeouts-and-dropped-connections)); a Pod that uses its whole termination grace is seen to
stop only by a later request. If the executor cannot prove the abort
by then, because the controller has not ended the cancelled run or a Pod's
agent container has not been seen to stop, it answers `503 unavailable`.
The session stays `unknown-stop`, and `recover` sends the same request
again, which resumes the abort. It answers `409 conflict` for a visit that
may already have run (active, stopping or stopped), and when an
administrator deleted the visit's registration or replaced its run
([session commands](session-commands.md#status-and-listing-output)).

| Service | Executor | What happens |
| --- | --- | --- |
| This release | This release | `abort-preparation`; the session ends `cancelled`. |
| This release | Older | The executor answers `404 invalid_request`. The service inspects the visit as before: one never registered is `cancelled`, one already prepared is aborted, one active or in an unknown state is refused as not a preparation, and one still preparing stays `unknown-stop` with `the remote executor cannot abort a preparation that has not reported its Pod; it is older than this Sproozi`. Update the executor, then `recover`. |
| Older | This release | The service never sends `abort-preparation`; nothing changes. |

## Request timeouts and dropped connections

Each request waits as long as the executor may take over it, plus a minute for
the answer to arrive
([ADR 0036](../adr/0036-remote-executors-through-kubeconfig-contexts.md#amendment-a-stop-outlives-its-request-2026-11-16)).

| Request | The executor takes at most | The client waits |
| --- | --- | --- |
| Placement `stop`; owner `stop` and `recover` | 9 minutes 43 seconds: two 2-minute waits for a first visit's conversation and turn, the supervisor's 30-second wait for the agent to quit, 3 seconds after killing one that did not, and 10 seconds for its reply to be read ([native control](native-control.md)), then 5 minutes to end the container (at most 1 minute), seal usage and capture the checkpoint | 10 minutes 43 seconds |
| Placement `prepare` | 2 minutes for an interactive visit's Pod to run, plus sending and restoring the checkpoint | 8 minutes |
| Placement `abort`; owner `cancel` | The Pod's termination grace period, 30 seconds by default, and sealing usage | 2 minutes |
| Placement `abort-preparation` | 30 seconds once no other operation on the same visit holds it ([cancelling a preparation](#cancelling-a-preparation)) | 2 minutes |
| Everything else, and the wait for an `attach` to be admitted | A few API calls | 45 seconds |

A stop still waiting for the turn answers `turn_pending` about two minutes
after it asked (four for a first visit), well inside the client's wait.

When the connection drops or the client gives up during a stop, because the
laptop closed, the port-forward broke or the user pressed Ctrl-C, the executor
carries the stop on to its outcome, for at most the time in the table. The
session meanwhile reads `unknown-stop` at home, or `stopping` on the
executor. Repeat `stop`, or run `recover`, with the same operation: the retry
waits for the stop in progress and returns its result, the same checkpoint an
uninterrupted stop keeps. `stop --remote` does this by itself after up to
three dropped connections in a row. `cancel` still interrupts the stop at
once. Other requests end with their connection and are retried the same way;
none loses state. A load balancer in front of the API server that cuts idle
connections (often after four minutes) can end a stop's request early in the
same way: the stop still finishes on the executor.

### Restarts and rollouts

On `SIGTERM` the executor stops accepting requests, gives those in flight 10
seconds and closes their connections, attachments included. It then waits up
to 9 minutes 43 seconds for the stops it carries on and refuses new ones,
interrupts any still running, waits up to 15 seconds for them to return, and
exits. For a stop in flight at a rollout to finish, set the executor Pod's
`terminationGracePeriodSeconds` above 608 seconds; the
[examples](../../examples/portable-workflows/kubernetes/owner.yaml) set 620.
With less, the kubelet kills the executor at the grace period. That is
safe, since nothing half-written is kept, but like an interrupted stop it
loses the reply of an agent's stop in flight: a Claude Code visit then keeps
the checkpoint it started from, and a Codex visit records an exit. The
executor logs `Interrupted stops still running after … at shutdown; repeat
each stop to finish it` when it had to interrupt any, and `Exited without
closing the placement: …` when one still ran 15 seconds later.

Stops of one visit with the same launch and incarnation share one run: a
repeated stop while one runs gets that one's answer. An abort that names
another incarnation answers `conflict` only once the visit's stop ends, which
can be after home's two-minute wait for an abort.

## Interactive sessions

The executor owns the sessions it starts, tasks and interactive sessions alike
([ADR 0057](../adr/0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md)).
An interactive session runs until its owner stops it; nothing waits for a
task to complete. Only the home principal (the `sproozi-cli` identity above)
starts one, cancels it, or takes the owner actions below. The work token that
Hermes holds keeps tasks only: these requests answer it `403`, and its MCP tool
`sproozi_run` offers no mode or upload. A Codex executor and a Claude Code
executor (`"harness": "claude-code"`) each run tasks and interactive
sessions; a Claude Code task's result is Claude's final reply. A
submission for another harness than the executor's answers `unsupported`.
Codex and Claude Code on one cluster are two executors, one per namespace
([example](../../examples/portable-workflows/kubernetes/claude/kustomization.yaml)).

Owner requests are `POST` to `/work/ACTION` with a JSON body, over the same
TLS connection as the placement protocol, and answer JSON: the session's
status, or `{"error":"CODE"}` with the codes and HTTP statuses
[above](#placement-outcomes) (`turn_pending` excepted), or one of the codes
of a [full executor](#retention).

| Action | Body | Answer |
| --- | --- | --- |
| `run` | A submission with `"mode":"interactive"` and no `task`. | The session. It starts in the background: `preparing`, then `active`. |
| `stop` | `id`, `operationId` | The session after a graceful stop that waits for the current turn: `retained`, or `failed` when the agent exited with a failure. `stopping`, with a `problem`, while it still waits; repeat the request with the same operation, or `cancel`. |
| `resume` | `id`, `operationId`, optional `maxDuration` | The session with a new execution (`executionId`) that continues from the last checkpoint, or from the starting workspace when it stopped before any conversation. |
| `attach` | One line `{"id":"..."}`, then terminal frames | A stream of terminal frames (below). |
| `remove` | `id`, optional `pullFinished` | `{"id":"..."}` once the executor keeps nothing of the session ([removing sessions](#removing-sessions)). Tasks too. |
| `cancel`, `recover`, `info`, `list` | as for tasks | as for tasks |

Each `operationId` is a UUID the caller keeps for retries: repeating a `stop`
or `resume` with the same operation, also after the executor restarts, returns
the same result and never stops or starts twice. An operation belongs to one
action and one execution, and the submission's `requestId` is not one; reusing
either answers `conflict`.

`resume` answers `conflict` for a session that is running, stopping, being
pulled, pulled or cancelled, and `unsupported` for a task, which takes no
`stop`, `resume` or `attach`. The new execution has the executor's full
allowance and its own deadline: `maxDuration` is `none` or a Go duration, and
empty keeps the submission's choice. The executor's `maxDuration` caps it, so
`none` needs an executor without one; a choice beyond the cap answers
`denied`.

An interactive session does not recover by itself. When the executor cannot
prove a stop or seal usage, the session stays `unknown-stop` or
`unknown-usage`, after a restart too, until `recover` retries it once.

### Starting from an approved repository or an upload

A submission names an approved `repository` or `project`, as a task does, and
starts from its snapshot, from `workspaceFrom`, or from an uploaded
`workspace`: a captured workspace archive, base64 in JSON, of at most the
executor's `maxCheckpointBytes`. The repository or project still sets the
session's association, allowances and reference. A repository approved only
by URL starts an empty workspace; the agent fetches it when asked. The
upload is home-only, interactive-only, and excludes `workspaceFrom`.

The executor validates an upload before it keeps anything, as a placement
validates a workspace that moves to it: it reads the archive's manifest and
entries, then restores it in private scratch space within the byte limit and
five minutes, which checks every Git object, the index and each file and
installs no Git configuration, hooks or credentials. A malformed or truncated
archive, a symbolic link or any entry other than a regular file, an
unsupported Git layout, or a body over the limit answers `invalid_request`.
The `run` body bound grows by the upload's base64 size for the home principal
only. The executor keeps the upload privately until the session's first
execution holds it, and then deletes it. Replaying the submission with the
same upload returns the same session.

### Attaching

`attach` names the session only: the executor finds its active visit itself,
so the caller never sends a launch or incarnation, and attaching works before
the agent's first conversation exists. The body is the JSON line `{"id":"..."}`
followed by [terminal frames](native-control.md#terminal-attachment), one JSON
object per line: `write` with base64 `data`, or `resize` with `rows` and
`columns`. Any other frame ends the attachment. The answer streams the
terminal's output frames until either side ends, or the execution's deadline;
a failed attachment ends with `{"op":"attach","error":"attachment unavailable"}`.
Input is bounded at 8 MiB and output at 64 MiB per attachment, so reconnect
before either. Ending an attachment never stops the session.

`attach` answers `conflict` before streaming when the session is not
`active`, and `unsupported` when the server cannot stream both ways.

### Retention

`retainedBytes` bounds what the executor keeps under `directory/work` and
`directory/sessions`; 1024 bounds its sessions. Only admitting new work
checks them: a submission (`run`, with or without an upload) and a `resume`.
Admission reserves what the work may still come to keep, `maxCheckpointBytes`
each: a submission its input workspace and its checkpoint, a resume its
checkpoint, since it starts from one the executor already keeps. It is
refused while what is kept, plus the reservations of every session not yet
`retained`, `failed`, `cancelled`, `exporting` or `pulled` (two for one whose
first visit has not started, one after), plus its own, would exceed
`retainedBytes`. A running interactive session therefore reserves a
checkpoint until it stops. A stop, a cancellation and a task's completion are
never refused for space: what they keep was reserved, and refusing them would
lose work.

Outside the bound: removal marks (below), and scratch space. Validating an
upload before admission, and capturing a revision or an empty workspace for
a task after it, each write up to `maxCheckpointBytes` under
`directory/work` and delete it when done; a concurrent admission counts that
as kept, but the volume may briefly hold that much beyond `retainedBytes`.

At the smallest `retainedBytes`, twice `maxCheckpointBytes`, a submission is
admitted only when the executor keeps nothing, so it keeps one session, and a
resume only when that session's workspace, checkpoint and metadata fit in one
`maxCheckpointBytes`: a larger session can be removed but not resumed there.

| Code | HTTP status | Meaning |
| --- | --- | --- |
| `sessions_full` | 507 | The executor keeps 1024 sessions. Remove some, then retry. |
| `retention_full` | 507 | What the executor keeps and has reserved leaves no room for this submission or resume. Remove sessions, stop running ones, or raise `retainedBytes`, then retry. |
| `executions_full` | 507 | A `resume` of a session that has had 256 executions. Pull it, or start new work. |
| `operations_full` | 507 | A `stop` or `resume` of a session with 1024 bound operations. Cancel it, or pull it once stopped. |

Nothing changed when any of these answers, and repeating the request does
not help until something changes. Hermes reads the same text from
`sproozi_run`. A Sproozi older than these codes reports them as
`unavailable`.

### Removing sessions

`remove` deletes a session the executor no longer needs: its record, its
executions' journals, and the workspaces, checkpoints and profiles no other
session names. Profiles the placement keeps by digest stay. Only the home
principal removes; the work token gets `403`. The body is `id`, the
session's, and `pullFinished`, which a pulled session needs.

| Phase | `remove` |
| --- | --- |
| `retained`, `failed`, `cancelled` | Removed. |
| `pulled` | Removed when `pullFinished` is `true`, otherwise `conflict`. Once removed, the executor answers neither `info` nor `claim` for it, so a home whose claim reply was lost could never finish its pull. `sproozi rm ID --remote` sends it only after this machine's service reports the session as its own. |
| `preparing`, `activating`, `active`, `stopping` | `conflict`: wait for it to start or finish, or cancel it; an interactive session can also be stopped. A task is `active` while its turn runs and `stopping` only while its answer is recorded. |
| `unknown-stop`, `unknown-usage`, `cancelling` | `conflict`: `recover` it first. |
| `exporting` | `conflict`: finish the pull first. |
| Any, while a session whose first visit has not started names it in `workspaceFrom` | `conflict`: that session's start reads this one's checkpoint. Wait for it to start, or cancel it. |
| A version 1 record (`retained`, `cancelled`, `pulled`) | Removed with the checkpoint it kept, `pulled` with `pullFinished`; `exporting` answers `conflict`. |

A removed session leaves a mark of about 50 bytes, outside `retainedBytes`
and the session count, so its ID never starts again: a `run` that repeats its
`requestId` answers `conflict`, and so does a `run` whose `workspaceFrom`
names it. Removing it again answers `{"id":"..."}`. An ID the executor never
held answers `invalid_request`.

Removal does not wait for other sessions: it takes only the removed
session's executions, and waits at most five seconds for an operation on one
of them before it answers `unavailable`; repeat it. The mark is written
first, then the journals and the record go, and the mark shrinks last; after
a crash the executor finishes the removal before it starts any session. A
removal it cannot finish is logged (`Could not finish removing work session
ID`) and the executor starts anyway; the session is not listed or run, and
`remove` retries it. A pulled session can be removed only while the machine
that pulled it still holds it: remove the cluster's record first.

### Agents that exit by themselves

Every 15 seconds the executor checks each active interactive session's Pod.
When the agent's native container has ended, the executor stops the session
through the ordinary stop, which reads the
[exit report](native-control.md#exit-reports) and keeps the conversation of a
completed Codex turn. A stopped container never proves a completed Claude Code
turn, so a Claude Code session keeps no turns from that visit: the workspace
it started from after a first visit, its previous checkpoint after a resumed
one. The session then shows `failed` with its `exit` (code, cause
and last output lines) for a nonzero exit, or `retained`. An `attach` to such
a session stops it the same way and answers `conflict`. The executor needs
`get` on `pods/log` in `sproozi-agents` to read the report.

## What the executor publishes

One remote executor per namespace. The administrator provides:

| Object | Name | Content |
| --- | --- | --- |
| Service | any, labelled `sproozi.com/remote-executor: "true"` | A port named `https` targeting the executor. |
| ConfigMap | `sproozi-remote-executor` | `ca.crt`: the CA that issued the executor's serving certificate. Created with `immutable: true`. |
| Serving certificate | in the executor's own Secret | Covers `SERVICE.NAMESPACE.svc`. |
| ClusterRoleBinding | any | Binds the executor's ServiceAccount to `system:auth-delegator`, for TokenReview and SubjectAccessReview. |

See [the executor setup](../../cmd/session-profile/README.md#create-private-configuration).
The executor accepts a caller as its home principal when a TokenReview
authenticates the bearer token for the audience `sproozi-remote-executor` and a
SubjectAccessReview allows `use` on
`remoteexecutors.sproozi.com`, named by the executor's `placementName`, in its
`namespace`. Removing the RoleBinding revokes access on the next request. The
[retained-work example](../../examples/portable-workflows/kubernetes/owner.yaml)
carries the Service label and the binding. Publish the CA from its file as an
immutable ConfigMap. The executor may update other ConfigMaps in its namespace,
so immutability is what stops it replacing the CA the CLI trusts for it:

```sh
{ kubectl -n sproozi-sessions create configmap sproozi-remote-executor \
    --from-file=ca.crt=/path/to/executor-ca.crt --dry-run=client -o yaml
  echo 'immutable: true'; } | kubectl create -f -
```

To replace the CA, delete the ConfigMap and create it again the same way.

The executor describes itself on request: its placement name, harness and
`maxCheckpointBytes`. The placement name is the executor's name in session rows
and commands, whatever the context is called; its checkpoint limit must equal
the service's `local.maxCheckpointBytes`.

## The CLI's ServiceAccount

`config/cli-access`, released as `sproozi-release/cli-access/`, holds the CLI's
identity in the executor's namespace. Apply it there with the rest of your
manifests. It targets `sproozi-sessions`, the example executor's namespace: the
checkout's `kustomization.yaml` sets it, and the release's `cli-access.yaml`
is rendered with it. An overlay with its own `namespace` changes it. Never apply it in `sproozi-system`:
there `pods/portforward` would reach the gateway and controller.

| Object | Name | Purpose |
| --- | --- | --- |
| ServiceAccount | `sproozi-cli` | The CLI and the service's identity. |
| Role, RoleBinding | `sproozi-cli` | Exactly the permissions below. |
| Secret | `sproozi-cli-token` | A long-lived token Kubernetes fills for `sproozi-cli`. |

| API group | Resource | Verbs | Why |
| --- | --- | --- | --- |
| core | `services` | `list` | Find the Service labelled `sproozi.com/remote-executor=true`. |
| `discovery.k8s.io` | `endpointslices` | `list` | Find a ready executor Pod. |
| core | `pods/portforward` | `create` | Open the TLS tunnel to that Pod. |
| core | `configmaps` named `sproozi-remote-executor` | `get` | Trust the executor's certificate. |
| core | `serviceaccounts/token` named `sproozi-cli` | `create` | Mint the short-lived, audience-bound token the executor accepts. It is a token for the same identity, so it grants nothing new. |
| `sproozi.com` | `remoteexecutors` | `use` | Act as the executor's home principal. No such API resource exists; the executor checks the rule with a SubjectAccessReview. Add `resourceNames` with a placement name to allow one executor only. |

`pods/portforward` covers every Pod in the namespace. A dedicated namespace for
the executor keeps the grant to the executor alone.

## Create the sproozi context

With an administrator context for the cluster, apply the CLI access and wait
for Kubernetes to fill the token:

```sh
NS=sproozi-sessions
kubectl apply -k sproozi-release/cli-access      # or config/cli-access from a checkout
kubectl -n "$NS" wait --for=jsonpath='{.data.token}' secret/sproozi-cli-token
TOKEN=$(kubectl -n "$NS" get secret sproozi-cli-token -o jsonpath='{.data.token}' | base64 -d)
```

Write the `sproozi` context into your kubeconfig, reusing the administrator
context's cluster entry:

```sh
CLUSTER=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')
kubectl config set-credentials sproozi-cli --token="$TOKEN"
kubectl config set-context sproozi --cluster="$CLUSTER" --user=sproozi-cli --namespace="$NS"
kubectl --context sproozi auth can-i create pods --subresource=portforward
```

The last command prints `yes`. To keep the context in its own file instead,
for example to pass it as `--context PATH`, add `--kubeconfig FILE` to the
`config` commands, first copying the cluster entry with
`kubectl --kubeconfig FILE config set-cluster NAME --server=URL --certificate-authority=CA_FILE --embed-certs`,
then run `kubectl --kubeconfig FILE config use-context sproozi` and
`chmod 600 FILE`.

To rotate the token, delete the Secret, re-apply `cli-access`, and run the
`set-credentials` command again with the new token. The service picks it up on its
next connection.
