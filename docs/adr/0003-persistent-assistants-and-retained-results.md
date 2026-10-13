# 0003: Persistent assistants and retained worker results

Status: accepted
Date: 2026-10-08

## Problem and constraints

A persistent assistant must accept tasks and routines, delegate bounded work,
and retrieve answers after disposable Pod cleanup. Sproozi's AgentRun owns a
bounded process and one global active execution slot. Hosting an always-running
assistant inside that lifecycle would prevent its delegated workers from running.
The alert webhook does not supply recoverable submission, observation or
cancellation, and Pod log polling races cleanup.

The user prioritizes community growth and interest over an existing installation;
a root bootstrap for the main assistant is acceptable. Selection must consider
native integration seams, not conflate a shell-tool backend with whole-worker
isolation. Preserve shared gateway identity, live policy, Run UID budgets,
administrator-owned runtime and stock agent loops established by ADRs 0001/0002.

## Decision

Use Hermes as the first persistent coordinator, independently deployed with
durable native state. Use its native MCP client to call a private TLS task service.
No custom Hermes plugin or replacement native agent harness is required. The
generic service accepts only submit/status/wait/cancel intent; administrator
configuration fixes each workflow's namespace, template and capability subset.
Kubernetes write credentials stay in this trusted service, outside the assistant
and bounded workers. One service instance represents one authenticated principal.

The controller remains sole admission and worker lifecycle owner. The assistant
owns native conversations, schedules and delivery; its provider credentials and
inference accounting are separate from disposable Run identity and budgets.
The existing serialized worker queue remains explicit.

Use the official image's native s6 profile supervisor as the sole owner of its
gateway process. Kubernetes owns the Pod and persistent volume; the container
main program remains idle. Seed running state only on a fresh volume with
`HERMES_GATEWAY_BOOTSTRAP_STATE=running`, then honor persisted native state.
A foreground gateway command also worked on first boot, but competed with the
auto-restored profile gateway after restart and interrupted cron execution.
Resetting native state on every boot would discard operator intent; bypassing
the stock bootstrap would recreate its volume and supervision responsibilities.
Native supervision accepts an idle container main process and a saved stopped
gateway requiring explicit native restart. Readiness follows the actual API.

Use deterministic caller/key Run names and complete immutable-request comparison
for replay. Submission includes a fixed UTC expiry no more than one hour ahead.
An exact retained Run is reusable after expiry; a missing expired operation
cannot create new work. Status/cancel require the expected UID and caller owner.
Cancellation only sets true using UID and resource-version tests. Live policy
retention must exceed 65 minutes before creating work. Administrative deletion
or retention shortening resets prior replay guarantees; no indefinite exactly-once
claim is made. This avoids a separate receipt store and second Run finalizer.

Retain a small explicit untrusted answer in `AgentRun.status.result`. A strict
versioned Run-UID envelope is read from the same owned Pod completion observation
as its exit code, then persisted atomically with terminal phase before cleanup.
Limit the envelope to 3,584 encoded bytes, use UTF-8-safe truncation and explicit
`truncated`, and accept only File termination policy. Missing or malformed output
is unavailable; available empty output is distinct. Actual process exit determines
success independently of content. Cancellation observed during completion takes
precedence. The controller never copies arbitrary fallback logs into results.

Use a narrow immutable native launch adapter where needed to extract final text
and preserve process exit/signals. It does not assess answer quality, implement
the model/tool loop or grant authority. Trusted instructions and untrusted task
inputs remain separate; worker homes and sessions are disposable.

Allow an administrator to set a canonical Anthropic upstream origin at gateway
startup, matching the existing OpenAI composition seam. This enables native
deployed acceptance against a local provider without replacing the worker loop.
The logical Anthropic authority, gateway-owned credential, live policy and Run
budget remain fixed; a worker cannot choose the upstream. Reject origin paths,
userinfo, query and fragment delimiters to preserve endpoint routing.

## Selection evidence and limits

Pinned upstream research covered OpenClaw v2026.9.9 and Hermes v0.21.6. Both
provide native MCP, persistent sessions/schedules and headless worker interfaces.
OpenClaw had greater accumulated GitHub reach on 8 October 2026. Hermes had
greater recent surviving fork creation volume in equal 30-day windows and higher
Reddit activity in the available first-party snapshots. Merged PR acceleration
favored Hermes but measures development output, not audience growth. Fork
additions declined for both projects. Comparable attributable X growth was
unavailable; Hermes' official social link points to the whole Nous Research
organization, so its followers cannot stand in for project-specific users.

These proxies support selecting Hermes for recent interest with moderate
confidence; they do not establish installed users or an overall fastest-growing
community. Existing OpenClaw use and its non-root container path did not decide
the final selection. Sources: [Hermes](https://github.com/NousResearch/hermes-agent),
[OpenClaw](https://github.com/openclaw/openclaw),
[Hermes subreddit](https://www.reddit.com/r/hermesagent/),
[OpenClaw subreddit](https://www.reddit.com/r/openclaw/).

## Alternatives and rejection reasons

A native OpenClaw/Hermes plugin talking directly to Kubernetes eliminates the
task-service deployment. However, namespaced create/patch RBAC cannot constrain
template fields or caller-owned cancellation, and exposes collective namespace
authority to the assistant process. It also introduces a client-specific SDK,
plugin package and persistent operation ledger. A generic native MCP service
keeps those policy decisions in one place and works with either client.

Permanent ConfigMap submission receipts, a second finalizer and a reconciliation
loop could retain terminal answers and deduplication beyond Run TTL. They add
another lifecycle owner, crash-window repair and unbounded retained records.
The initial bounded replay contract does not require that machinery. Revisit it
when lossless delivery beyond configured retention is a concrete requirement.

Polling Pod logs is simpler at the facade but cannot reliably retrieve the final
answer before controller deletion. Controller log capture before terminal status
would avoid that race, but adds a logs transport, rotation/node-loss semantics
and mixed progress/error/output parsing. A result-write gateway and artifact store
would support larger output, but adds an authority-bearing write API and durable
storage ownership. Small termination-file answers reuse Kubernetes completion.

Replacing the coordinator's native harness with Kubernetes execution could move
complete turns remotely, but couples native transcripts, fallback, tools and
session ownership to Sproozi. A terminal backend moves only shell operations.
Neither is needed for explicit bounded task delegation. Keeping a persistent
assistant inside an AgentRun conflicts with the execution deadline, disposable
state and single worker slot.

## Accepted costs and consequences

The task service adds private TLS, a bearer Secret and namespaced permissions.
Token rotation requires updating both service and client configuration. Its
principal is an assistant installation, not automatic per-channel-user isolation.
The trusted assistant retains its own model/provider and channel authority.

Answers are small and can be truncated or unavailable after crashes. They are
untrusted, potentially sensitive, and visible to AgentRun API readers; they
expire with the Run. This deliberately changes result confidentiality while
leaving audit-safe fields unchanged. It does not supply artifact storage,
streamed progress, task-quality evaluation or indefinite delivery.

Replay safety depends on ordinary Run retention and excludes administrative
destructive resets. A native routine needs a new operation identity for every
intentional execution, while retries reuse the exact identity and expiry.
Queued jobs do not become a parallel multi-agent execution graph.

## Evidence and reconciliation

Retained task records include three upstream/current-system groundings, traced
how/why synthesis, two structurally distinct candidate designs, a scored judgment,
community measurements and the synthesized contract. The generic facade base
was selected; bounded expiry and UID disappearance behavior were grafted from
the direct-plugin alternative. Permanent receipts/finalizers were rejected.
All design seats used the available parent model; no cross-family diversity is
claimed. The judge had authored one candidate, so root also read and assessed
both candidates.

Implementation reconciliation covered `internal/tasks`, `cmd/tasks`,
`internal/agentcontract`, `internal/kubernetes`, `internal/controller` and
`internal/harness`. Independent reviews corrected native tool-name collisions,
inherited stdout completion, per-client acceptance attribution, public cron
syntax and the scoped Kubernetes acceptance gap. Full deployed acceptance on
9 October 2026 passed all four pinned native clients, policy and budget denials,
UID-bound answers, replay, cancellation, deadlines, isolation and cleanup.
Native Hermes API delegation and cron delegation passed across PVC-backed Pod
replacement. A process census found one native gateway before and after restart;
the scheduled worker was created after the replacement Pod and its retained
answer reached native cron output. The supervisor ownership correction is
therefore covered by the default Kind gate.

Deterministic provider fixtures establish deployed lifecycle, transport,
delegation and accounting behavior. They do not establish paid-provider billing,
real chat-channel delivery or production reliability.

Current contract: [orchestration reference](../reference/orchestration.md).

## Revisit conditions

Revisit coordinator choice if community-interest evidence changes or native
integration limitations appear in real operation. Add separate principals or
authenticated sender attribution when multiple users require independent
authority. Add receipts/artifact storage when requirements exceed Run retention
or the small answer bound. Revisit worker concurrency when real queueing demands
parallel admission. Simplify launch adapters when pinned native releases provide
reliable final-file output and completion semantics directly.
