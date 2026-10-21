# Sproozi terms

These are the words the documentation uses, one per concept. Where code, flags
or older records still use another word, it is listed as "also seen as". For
examples and a reading path, see [core concepts](docs/explanations/core-concepts.md).

## The agent and where it runs

### Harness

The existing agent program that Sproozi launches: Codex, Claude Code, OpenCode
or Hermes. The harness runs the conversation and its tool calls; Sproozi
supplies the environment and access. Selected by `clientConfig.harness`.
Also seen as: client, native client.

### Sandbox

The isolated environment a harness runs in. On Kubernetes it is a disposable
agent Pod in `sproozi-agents` with its own run identity, NetworkPolicy and input
contract; on Docker it is a controlled container. It is not the `Sandbox`
resource from the Kubernetes Agent Sandbox project; see
[cluster dependencies](docs/reference/dependencies.md).
Also seen as: agent Pod (the Kubernetes form), worker Pod.

### Gateway

The trusted proxy that every request from a sandbox to an outside service goes
through. It holds provider credentials, identifies the run and checks each
request against current policy. See
[capabilities and gateways](docs/explanations/capabilities-and-gateways.md).

### Control connection

The `sproozi` MCP server that every execution reaches on its own gateway. Its
`diagnose` tool explains the execution's recent gateway refusals. It needs no
capability and grants none. See
[Sproozi integration and diagnose](docs/reference/sproozi-integration.md).

### Denial journal

The gateway's bounded, in-memory record of each execution's recent refused
requests, kept from its audit records. `diagnose` reads it.

### Sproozi integration

What Sproozi adds to every harness it launches: environment instructions, the
`sproozi-access` skill and the control connection. It is guidance, not
authority. Also seen as: managed harness configuration.

### Profile

Your own instructions and skills, in the `profile` folder beside the home
configuration (`~/.sproozi/profile` for a local installation), that every
managed Codex and Claude Code session receives after the Sproozi integration.
Like the integration, it is guidance, not authority. See
[Profile](docs/reference/profile.md). Also seen as: shared agent files, agents
directory, user preferences.

### Inspected connection

One CONNECT tunnel from a sandbox whose TLS the gateway terminates and checks.
Every connection the gateway admits is inspected. An execution can hold 16 at
once by default, or the gateway's connection limit if that is lower, set by
`SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION`, whose name says sessions but
which counts inspected connections. Each lasts at most 5 minutes, and each
request on it must finish 10 seconds before it closes, except a model call,
which keeps the connection open until its model call deadline. That lifetime
applies to the connection, not to the user's session, which continues over new
connections. The gateway connection limits that bound these are separate from
admission. See
[transport limits](docs/reference/security-hardening.md#protect-transport-and-identities).
Also seen as: gateway session, inspected session.

### Capability

A named kind of access a run can request, such as `kubernetes.read`,
`github.pull_request`, `model.inference`, `network.egress` or `mcp.<server>`.
Policy decides whether it is allowed and sets its limits. See
[permissions and capabilities](docs/reference/permissions-and-capabilities.md).

### Budget

A usage limit for one capability in one run, set in `AgentPolicy.spec.budgets`:
tokens or requests (`maxUnits`) and cost (`maxCostMicros`). On the home
daemon, the budget is the execution's [allowance](#allowance), set by the
daemon's `allowances` configuration; the gateway charges both the same way,
and its messages say "budget" for both. Budgets are not hard spending
ceilings; see
[security hardening](docs/reference/security-hardening.md#model-budgets-are-not-hard-spending-ceilings).

### Allowance

The total usage limit, per capability, granted to one execution and shared by
all of its visits. For local executions it comes from the service's
`allowances` configuration. A move doesn't refill it; a new execution gets a
fresh one.

### Reservation

The usage the gateway holds back from a budget for one model call before
forwarding it, then settles against the usage the provider reports or an
estimated charge. It is a bounded reservation when the request allows one, and
otherwise all remaining units and cost. A part of the budget with no limit
reserves nothing that can be charged. The ledger records every reservation
as a [lease](#lease) until its call settles.
Write "charges the whole reservation"
for what happens to an unverifiable call or a budget cut.
Also seen as: forfeit, charged in full (charging the whole reservation).

### Bounded reservation

A reservation sized from the request itself: its bytes plus its output bound
in units and, under a cost limit, its bytes at the highest input rate plus the
output bound at the output rate. Only a self-contained request with an output
bound and no provider-run tools gets one, such as a Claude Code request with
`max_tokens`. Bounded calls from one run can run in parallel. See
[reservations and parallel calls](docs/guides/model-provider-auth.md#reservations-and-parallel-calls).

### Unbounded call

A model call without a bounded reservation, because its request has no output
bound, references input the gateway can't size or enables a provider-run tool.
It reserves all remaining units and cost, runs alone and can meet a budget
cut, unless its request references input the gateway can't size: such a call
is never cut. Every Codex and ChatGPT-mode call is unbounded. Unrelated to the
`bounded` field of a home allowance, such as `"units": {"bounded": true,
"value": 200000}`, which says only whether that dimension has a limit.

### Estimated charge

What the gateway charges a model call interrupted after the provider received
the whole request, before or after its response headers, such as by Esc, the
model call deadline or a provider failure: input from Anthropic's reported
usage or the request bytes ÷ 4, plus output as the larger of the visible
output bytes, counted as one token each, and 250 tokens per second, capped at
the output bound and the reservation. Anthropic's exact output replaces this
once `message_delta` reports it. Calls refused before inference or while
waiting for their budget, calls whose request never fully reached the
provider and calls the provider answers with an error are free; see
[calls that aren't charged](docs/guides/model-provider-auth.md#calls-that-arent-charged).
It can differ from the provider's bill either way. Audited as `usageSource` `estimated`. See
[interrupted calls](docs/guides/model-provider-auth.md#interrupted-calls).

### Lease

The ledger's durable record of one pending [reservation](#reservation), kept
for every reservation from when it is made until its call settles or is
released: model calls and GitHub, package, Kubernetes, egress and MCP
requests alike. A model call's lease expires at its model call deadline plus 5
minutes and carries a [recovery charge](#recovery-charge); any other request's
lease expires at its request deadline plus 5 minutes and charges the whole
reservation. A lease that outlives its call, because its gateway was killed,
its drain ran out of time or its settle failed, is abandoned; after its expiry
the next reservation or seal on the account, or a periodic sweep, settles it.
A recovery never recreates a deleted account. An account holds at most 64
leases, so at most 64 of its calls can be pending at once; one more model call
gets a 429 with code `too_many_pending_calls` (the code is in the OpenAI body
only; the Anthropic body has the type `rate_limit_error` and no code). See
[gateway restarts](docs/guides/model-provider-auth.md#gateway-restarts).

### Recovery charge

What the ledger charges an abandoned [lease](#lease) once it expires: the
request bytes ÷ 4 at the plain input rate, plus the whole model call deadline
at 250 output tokens a second, capped at the output bound and the
reservation, or the whole reservation when the request references input the
gateway can't size. It is an estimate, fixed when the call reserves. Unlike an
[estimated charge](#estimated-charge), it knows nothing of how the call went:
no reported input or output, no visible output and no elapsed time, so it
charges the whole deadline even for a call that ended sooner. The recovery is
saved in its own update before the reservation or seal that found it, so it
persists even if that fails, and only then logged as `Settled abandoned model
reservation`, with no audit record.

### Unverifiable call

A model call whose usage the gateway can neither read from the provider nor
estimate: an interrupted request that references input the gateway can't
size, a response with an event over the 8 MiB parse limit, or, under a cost
cap, one that reports a cost category the administrator didn't price. The
gateway charges the whole reservation. See
[unverifiable calls](docs/guides/model-provider-auth.md#unverifiable-calls).

### Budget cut

The gateway ending an unbounded streaming model call whose observed usage, its
input plus its visible output bytes ÷ 4, reached its reservation. Unlike an
estimated charge, which counts each visible byte as one token, the cut divides
visible bytes by 4. Bounded calls are never cut, and neither are unbounded
calls whose request references input the gateway can't size. The harness receives the
protocol's error event with `model budget exhausted: the call reached its
reservation`, and the gateway charges the whole reservation. Elapsed time
doesn't count towards a cut. See
[budget cut](docs/guides/model-provider-auth.md#budget-cut).

### Model call deadline

The longest one model call may stream, counted from when the gateway receives
it: 20 minutes by default, set by `SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS` or
the home configuration's `gateway.maxModelCallSeconds`, and lowered per policy
by `AgentPolicy.spec.maxModelCallSeconds`. A call can outlive the connection
that carries it, up to this deadline. See
[long model calls](docs/guides/model-provider-auth.md#long-model-calls).

### Drain

What a gateway does when it stops on purpose: on SIGTERM the shared gateway,
and the service when it closes, refuses new model calls with a 503
(`gateway_stopping` in the OpenAI body; the Anthropic body has the type
`server_error` and no code), ends every call in flight with `endReason`
`gateway_restart` so each settles its reservation, and waits up to 30 seconds
(10 for the service) before it exits. A call the provider had received is
charged as interrupted, usually an [estimated charge](#estimated-charge). A
call still waiting for its budget, not yet fully written or still waiting for
its response headers gets the same 503; the first two are free. Anything still unsettled is left as
an abandoned [lease](#lease). See
[gateway restarts](docs/guides/model-provider-auth.md#gateway-restarts).

## Kubernetes resources

### AgentRuntime

Administrator-owned configuration for how the agent runs: image, command,
resources, security settings and gateway endpoint. See
[AgentRuntime](docs/reference/agentruntime.md).

### AgentPolicy

Administrator-owned rules for capabilities, resource access, budgets and
duration. See [AgentPolicy](docs/reference/agentpolicy.md).

### AgentTemplate

A named type of task that selects a runtime and a policy and supplies trusted
instructions. See [AgentTemplate](docs/reference/agenttemplate.md).

### AgentRun

One request to run a task through an AgentTemplate, with an explicit list of
requested capabilities. Also called a run. See [AgentRun](docs/reference/agentrun.md).

### Admission

The check Sproozi makes before a run, or a visit, starts: the requested
capabilities, resources and deadline must fit current policy. For an
AgentRun, admission also needs room under any concurrency cap.

### Active run

An AgentRun in phase `Admitted` or `Running`. Active runs count against
concurrency caps; a `Queued` run does not.

### Concurrency cap

An optional limit on how many active runs there can be at once: across the
cluster, set by the controller flag `--max-active-runs`, or under one policy,
set by `AgentPolicy.spec.maxActiveRuns`. A run held by a cap stays `Queued`
with reason `WaitingForConcurrencyCap`. There is no cluster cap by default, and
a policy has no cap unless it sets one. Write "cluster cap" for the first and
"policy cap" for the second. Also seen as: slot, active-run slot, max active
runs, global cap.

## Sessions and continuing work

### Session

A Sproozi record that tracks one harness conversation and its Git workspace
across executions. Its `id` is stable. The harness's own conversation ID
(`threadId`) is separate. Also seen as: Sproozi session; native session or
managed native session, for a session whose harness runs its own native client
(`clientConfig.nativeSession` in an AgentRuntime). When "native session" means
the harness's own conversation, write "harness conversation".

### Harness conversation ID

The harness's own ID for its conversation, shown as `threadId` in session
records. Sproozi resumes the harness conversation by this ID; it is separate
from the session `id`. Write "harness conversation ID (`threadId`)". Also seen
as: native UUID, conversation ID, harness conversation UUID.

### ChatGPT credential

The renewable OAuth sign-in to a ChatGPT plan that the gateway uses for model
requests in `chatgpt` mode, saved locally as `chatgpt-session.json` and then
held in a Kubernetes Secret. One active gateway owns it. It is not a
[session](#session); write "ChatGPT sign-in" or "ChatGPT credential". See
[model provider authentication](docs/guides/model-provider-auth.md).
Also seen as: ChatGPT session.

### Execution

One approved period of work on a session, with its own permissions, allowance
and deadline. `resume` starts a new execution; `move` keeps the current one.
Its ID is `executionId`. Also seen as: Sproozi execution.

### Placement

Where an execution runs: local Docker, managed by this machine's service, or a
Kubernetes cluster reached through a remote executor.

### Visit

One stay of an execution at one placement. Each `move` ends one visit and
starts another, with fresh admission, gateway access and a new sandbox. The
execution's deadline and allowance carry across visits. Its ID is
`allocationId`. Also seen as: allocation.

### Turnless visit

A first visit whose harness never started a conversation: no prompt was
submitted, or, for Claude Code, every prompt ended on a provider error before
any turn completed. `stop` ends it without waiting for a turn and keeps no
checkpoint; the session keeps the workspace it started from. See
[ADR 0032](docs/adr/0032-stopping-a-visit-with-no-native-conversation.md).

### Harness arguments

The user's own arguments for the stock harness, given after `--` on `start` or
`resume`. They belong to that execution, not the session. Sproozi refuses the
ones that would replace a flag it controls. See
[pass arguments to the harness](docs/reference/session-commands.md#agent-arguments).

### Writer

The running sandbox process that can change a session's conversation or
workspace. Before another visit starts, Sproozi must confirm the previous writer
has stopped. Status reports the exact writer as `incarnation`.

### Checkpoint

The saved conversation state and Git workspace of a stopped harness. It carries
work, never permissions or credentials. See
[ADR 0008](docs/adr/0008-selected-native-and-git-checkpoints.md).
Also seen as: Sproozi checkpoint.

### Finalized usage

The usage of a stopped writer, recorded so that it can no longer change.
Sproozi needs finalized usage before it captures a checkpoint or starts another
visit. Also seen as: sealed usage, sealed accounting (in ADR 0009 and other
earlier records).

### Workspace revision

The Git commit recorded in a checkpoint. A new session can start from that exact
commit with a clean checkout and a fresh conversation. See
[Projects and workspace revisions](docs/reference/projects-and-revisions.md).

### Remote work

A session that a remote executor runs in a Kubernetes sandbox and keeps in its
catalogue after the sandbox stops. It starts from `sproozi run --remote` or a
coordinator's request. Once the writer has stopped, usage is finalized and the
checkpoint is stored, its phase is `retained`: it can seed fresh work at its
workspace revision, or be pulled to this machine and resumed there. See
[Run a task remotely, then resume it locally](docs/guides/portable-workflows.md).
Also seen as: retained work, work session.

### Project

An administrator-configured group of retained sessions with an approved
template, capability limits and explicit membership. See
[Projects and workspace revisions](docs/reference/projects-and-revisions.md).

## Owners and services

### Owner

This machine's service or a remote executor: whichever's catalogue holds a
session. Only the owner starts, stops or hands over that session's executions.
The owner is not the placement: this machine stays the owner when `move
--remote` runs an execution in Kubernetes. A successful `pull` transfers
ownership from a remote executor to this machine, one way. Catalogue rows
report it as `ownership`: `local` when this machine owns the session,
`cluster` when the remote executor the row came from owns it, or `pulling`
while a pull is unacknowledged. After a pull, the remote executor still lists a
read-only row in phase `pulled` with `"ownership": "local"`. See
[status and listing output](docs/reference/session-commands.md#status-and-listing-output).
An owner lock is the lock on an owner's state directory that stops a second
process using it.

### Service

The background `sproozi service run` process on your own machine, which
`sproozi service start` and the session commands start for you. It owns local
Docker placements, their gateway and accounting, and the sessions you have
pulled. Commands and output call this machine `local`: `move --local`,
`"executor": "local"`, `"ownership": "local"` and `"placement": "local"`.
Also seen as: home daemon, home, home service (in code, configuration keys and
older records).

### Local installation

The private directory, `~/.sproozi` by default, that `sproozi init`
sets up for the service: its configuration, provider credentials, certificate
authority, journals and profile. `--root` or `SPROOZI_ROOT` chooses another.
See [local installation](docs/reference/local-installation.md). Also seen as:
installation root.

### Trust generation

One certificate authority in the gateway's certificate store. A worker trusts
the generations in the bundle it received at launch; rotation adds a new
generation and retires the old one only after every consumer has the new one.

### Certificate controller

The optional `/certificates` program in a Kubernetes installation. It keeps the
gateway's private CA, serving certificate and immutable trust ConfigMaps
without cert-manager. See [Kubernetes installation](docs/reference/gateway-certificates.md).

### Remote executor

The `session-profile` program. It serves one administrator-approved Kubernetes
placement, holds that cluster's credentials and hosts a catalogue of remote
work. Its work token (`workTokenFile`) is the bearer token a coordinator uses
for `/work/mcp`; it differs from the home principal token (`tokenFile`), which
also permits pull, and from the task service's token. Also seen as: profile
service, remote profile, fixed remote profile, work owner, native work owner, remote work
service.

### Home principal token

The bearer token this machine's service presents to a remote executor. The profile
service reads it from its `tokenFile`, and the home configuration names the
same secret in `remote.NAME.tokenFile` or `workSources.NAME.tokenFile`. It
permits remote work, moves and pull, unlike the work token. It needs at least
32 non-whitespace characters. Also seen as: home token, principal token,
profile principal token, profile token, remote token.

### Executor

The owner whose catalogue a command reads. `local` is this machine's service;
any other name is a remote executor. Remote commands select one with
`--context` (a kubeconfig context or file) and `--namespace`. Executor names
are lowercase DNS labels of 1 to 63 characters. See
[remote commands](docs/reference/session-commands.md#remote-commands).

### Work source

An entry in a hand-written configuration's `workSources` map: a connection to
a remote executor that `run --remote`, `sessions`, `status`, `cancel`,
`recover` and `pull` use but `move --remote` does not, because it is not a
placement. Entries take the same fields and name rules as `remote` entries.
When both maps hold the same name, those commands use the `workSources` entry
and `move` uses the `remote` one. See the
[configuration reference](cmd/sproozi/README.md#configuration-reference).

### Catalogue

The list of sessions that one owner holds. `sproozi sessions` combines this
machine's catalogue with each cluster's. See
[status and listing output](docs/reference/session-commands.md#status-and-listing-output).

### Coordinator

A persistent assistant, currently Hermes, that starts delegated runs through
the task service or a remote executor's remote work. It runs outside the
sandboxes it starts.

### Delegated run

An AgentRun that a coordinator starts, one per unit of work it delegates.
Delegated runs count against concurrency caps like any other run; the
coordinator does not. The example policy and template they use are in
`examples/orchestration/worker.yaml`. Also seen as: delegated task, delegated
job, job, disposable job, worker.

### Task service

The `sproozi-tasks` program: a private MCP service that turns a coordinator's
request into a delegated run of a fixed, approved workflow. It keeps the run's
status and final answer, not its conversation or workspace. For a delegated run
that keeps a checkpoint for later resume, the coordinator uses
[remote work](#remote-work) instead. See
[orchestration](docs/reference/orchestration.md). Also seen as: task API,
bounded task API, disposable-job service, orchestration service.

