# Architecture decisions

Architecture decision records explain why Sproozi uses a design. Current guides
and references describe how to use it. An accepted decision can remain valid
after its initial implementation is complete.

## When to add a record

Architecturally significant changes MUST include an ADR and updated docs in the
same change. This applies to trust boundaries, public API concepts, integration
patterns, ownership, lifecycle, and runtime or harness responsibilities.

Include the ADR and affected docs in the task's done criteria before
implementation. Record consequential choices as you make them. Before
completion, check that the record describes the actual implementation.

## What to record

Use `NNNN-short-description.md` with the next available number. Include a date
and an accepted or proposed status. Record:

- The problem and constraints.
- The selected decision and reasons.
- Meaningful alternatives, their benefits and why they were rejected.
- Accepted tradeoffs, costs and consequences.
- Conditions that would justify revisiting the decision.
- Links to affected explanations, references and implementation.

Keep operational steps and current support limits in guides and references. The
ADR preserves the reasons for the choice.

## Review before completion

Look back over the work: the options you considered, the checks you ran and
any review comments. Every important choice should either have an ADR or a
sentence in the change description saying why it needs none. One ADR can cover
several related choices. Say which options were chosen, which were rejected,
and what was fixed or left for later.

Update affected explanations, references and examples. Update the main README
when supported capabilities change. Compare claims with code and test evidence,
then run `make verify-docs`. The command checks required documents and links. It
does not check documentation accuracy or decision coverage.

## Keeping the records current

This folder holds only records that describe the system as it is. A record is
current or it is removed.

- **Replaced or gone.** A record whose decisions are all replaced, or whose
  feature is gone, is marked deprecated and deleted in the change that replaces
  it. Add a row for it to the Removed table, with the record or page that now
  holds the decision and links to the change that removed it and to its last
  text.
- **Corrected.** A current record may receive a dated correction of fact in
  place, for example `Corrected 2026-11-15: ...`.
- **Changed.** A change to a decision needs a new record. The record it
  replaces is removed in the same change.
- **Numbers.** A number is never reused. Reserve one in the Reserved numbers
  table when a record is in flight on another branch.

`make verify-docs` fails when an `ADR NNNN` mention in the repository names a
number that has no record, no reserved row and no removed row.

## Decisions

These records preserve the reasons for each design. A later record can replace
part of an earlier decision. ADR 0015 replaces exact client-version selection
and the combined MCP discovery schema. ADR 0021 replaces ADR 0015's unversioned
client installs with pinned lockfiles and digests for CI and the verifiers.
ADR 0027 replaces the single active-run slot from ADR 0003, and ADR
0016's rejection of a revalidation cache. ADR 0028 replaces ADR 0002's buffered
model responses and response bound, narrows ADR 0009's full charge to calls
that can't be estimated and budget cuts, limits ADR 0015's per-account serialization to
unbounded calls, and settles ADR 0015's unknown reservations after a restart
at their recovery charge, an estimate, once their leases expire. ADR 0048 makes
Copilot, first an AgentRun harness, a managed one. ADR 0049 replaces ADR
0047's login storage with a live store per named login.

ADR 0040 removes ADR 0014's kube-dns exception: Kubernetes sandboxes resolve
no names.

ADR 0029 extends those pinned clients to the published native image, replacing
ADR 0024's statement that releases do not pin clients.

ADR 0056 replaces the rule, in ADRs 0004, 0010, 0012 and 0034, that every
execution has a deadline: whoever starts work sets it, and work its owner
starts may have none. A set deadline still binds as those records describe.

ADR 0057 extends ADR 0018 to the remote executor, which now runs its work
through the coordinator, and replaces ADR 0032's consequence that Kubernetes
visits are never turnless.

ADR 0060 adds the `abandoned` event to ADR 0058's execution journal table. It
also replaces ADR 0057's rule that an interactive session whose execution has
not started by its deadline is cancelled: the session now stops with its work
kept.

ADR 0067 extends ADR 0060's `abandoned` event to a start its placement
refuses for good, whether or not the execution has a deadline.

ADR 0072 extends ADR 0039 and changes one sentence of its 15 November
correction, that behaviour stays in code: the registry now also holds a
managed harness's argument syntax and the capture of its conversation, and
that behaviour sits behind two contracts with guards.

ADR 0065 replaces ADR 0057's rule that an interactive session's unsettled stop
or cancellation always waits for `recover`: once its project is revoked, the
driver settles it as it settles a task's.

| Decision | Topic |
| --- | --- |
| [0001](0001-mcp-capabilities-through-shared-gateway.md) | MCP access through the shared gateway. |
| [0002](0002-native-cli-harnesses-and-model-protocols.md) | Native CLI harnesses and model protocols. |
| [0003](0003-persistent-assistants-and-retained-results.md) | Persistent assistants and retained job answers. |
| [0004](0004-portable-sessions-and-bounded-executions.md) | Sessions across bounded executions. |
| [0008](0008-selected-native-and-git-checkpoints.md) | Selected conversation and Git checkpoints. |
| [0009](0009-runtime-backends-and-sealed-accounting.md) | Runtime backends and finalized usage accounting. |
| [0010](0010-managed-session-authority-and-admission.md) | Managed-session permissions and destination admission. |
| [0011](0011-native-session-compatibility-and-completion.md) | Native state compatibility and completion. |
| [0012](0012-retained-work-and-fresh-session-continuations.md) | Retained work and local resume. |
| [0013](0013-configured-projects-and-workspace-revisions.md) | Configured Projects and workspace revisions. |
| [0014](0014-review-boundaries-and-native-contracts.md) | Shared gateway limits, native contracts and CLI identities. |
| [0015](0015-default-clients-and-native-tool-schemas.md) | Default clients, MCP schemas and model requests sharing an allowance. |
| [0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md) | Capability checks at the gateway dispatcher and fresh revalidation reads. |
| [0018](0018-coordinator-owns-every-visit.md) | The home coordinator owns every visit, including the first, and launches never change. |
| [0020](0020-docker-placement-removes-stopped-visits.md) | The Docker placement removes a stopped visit's containers and volume after recording it, and the local service retries a failed removal. |
| [0021](0021-pinned-ci-inputs-and-latest-client-canary.md) | Pull request CI runs only pinned clients, images and manifests; a weekly canary reports upstream drift. |
| [0023](0023-controller-writes-only-in-sproozi-agents.md) | The controller writes sandbox objects only in sproozi-agents; its cluster role keeps reads. |
| [0024](0024-native-host-platforms-and-cli-releases.md) | Native host security and automated CLI releases with a Homebrew tap. |
| [0025](0025-managed-harness-integration-and-diagnose.md) | Every harness receives the Sproozi integration, and the gateway explains an execution's own denials. |
| [0026](0026-single-gateway-refresh-owner-with-recreate.md) | One gateway replica with a Recreate rollout owns the rotating ChatGPT credential refresh. |
| [0027](0027-configurable-concurrent-admission.md) | Optional cluster and policy caps replace the single active-run slot; a run's runtime is checked at admission; the gateway serves revalidation from a watch cache. |
| [0028](0028-streamed-model-responses-and-estimated-interrupts.md) | The model gateway streams responses, charges interrupted calls an estimate, refuses detached work, runs bounded calls in parallel and gives model calls their own deadline. |
| [0029](0029-local-provisioning-and-lifecycle.md) | Local provisioning, per-harness local placements and home-owned certificates. |
| [0030](0030-kubernetes-release-assets-and-certificates.md) | Kubernetes release assets and a self-managed certificate controller. |
| [0031](0031-user-profile-for-managed-sessions.md) | One user profile, sent with each Docker launch and stored by digest for Kubernetes, replacing the shared agent files. |
| [0032](0032-stopping-a-visit-with-no-native-conversation.md) | A first visit with no harness conversation stops at once without a checkpoint, and its usage seals without a bound conversation ID. |
| [0033](0033-harness-arguments-from-the-command-line.md) | Arguments after `--` on start and resume go to the harness, refusing flags Sproozi controls; Codex upgrade prompts are acknowledged from its own catalogue. |
| [0034](0034-resume-continues-every-home-session.md) | Every home session is catalogued, and `resume` continues a stopped one in a new execution, including sessions created with `start` and after the old deadline. |
| [0035](0035-bounded-graceful-stops-and-settled-input.md) | Graceful stops report what they wait for after a bounded time, settled terminal input no longer blocks them, and Claude system request caches are accepted. |
| [0036](0036-remote-executors-through-kubeconfig-contexts.md) | The CLI reaches a remote executor through a kubeconfig context (default `sproozi`) and a port-forward; the executor authorizes the caller's ServiceAccount with TokenReview and RBAC, and describes its own name and checkpoint limit. |
| [0037](0037-one-command-surface.md) | One grouped command surface: `run` for local and remote work, `init` for local Docker, `local` as this machine's only name, ID prefixes, `rm`, text output with `--json`, and `~/.sproozi` found by `--root` or `SPROOZI_ROOT`. |
| [0038](0038-exited-agents-and-one-recovery-path.md) | The service stops sessions whose agent exited by itself, showing them failed with the exit; a resumed visit with no prompt stops at once; stuck stops and cancelled sessions each name one next command. |
| [0039](0039-one-harness-registry-and-the-mcp-requirement.md) | One harness registry with guard tests; every harness needs an MCP client for the `sproozi` control connection. |
| [0040](0040-sandboxes-reach-the-gateway-without-dns.md) | Sandboxes resolve no names: the controller pins the gateway address in each Pod, and the NetworkPolicy has no DNS rule. |
| [0043](0043-remove-the-event-webhook.md) | The event webhook is removed until a supported sender exists; its design and gotchas move to a future-architecture note. |
| [0044](0044-the-api-stays-alpha-with-a-written-policy.md) | The CRDs stay `v1alpha1` under a written compatibility policy, with criteria for moving to `v1beta1`; the 1.0 promise is dropped. |
| [0045](0045-budget-ledgers-are-a-custom-resource.md) | Budget ledgers are `BudgetLedger` resources in `sproozi-system`, and the remote executor runs in `sproozi-sessions`, so neither the gateway nor the executor can write ConfigMaps in `sproozi-system`; the gateway prunes settled ledgers of deleted runs; session registrations keep no expiry yet. |
| [0047](0047-harnesses-sign-in-to-their-own-model-providers.md) | Every harness offers every credential mechanism its provider accepts: a gateway-held key, a gateway-held sign-in, or its own sign-in forwarded and inspected by the gateway, from one provider table. |
| [0048](0048-github-copilot-cli-is-a-managed-harness.md) | GitHub Copilot CLI is a managed harness: the supervisor names each session, observes completion in its events journal, checkpoints only that journal, and reaches OpenAI with a key or GitHub's Copilot service with a token or its own sign-in; it relays GET requests under GitHub's `/copilot_internal/` and `/copilot/` account namespaces, verified through the authenticated gateway; and it drops `--no-remote`. |
| [0049](0049-named-logins-share-a-live-store.md) | A harness's own sign-in lives in a named login store that the supervisor keeps in step with each running session, a Docker volume or a controller-created claim per harness and login. |
| [0050](0050-prebuilt-cli-and-explicit-image-builds.md) | Prebuilt Homebrew bottles and explicit native/control image builds. |
| [0051](0051-interactive-sessions-may-run-without-a-model-budget.md) | A local session's model allowance may have no limit, which setup now writes; every other capability keeps a unit bound and the Kubernetes placement keeps requiring one. |
| [0054](0054-claude-code-agentruns-launch-through-a-helper.md) | Claude Code AgentRuns launch through `claude-launch.py`, which publishes the stock client's result and keeps its login in step with a mounted login store; `--login` signs a login in once. |
| [0056](0056-whoever-starts-work-sets-its-deadline.md) | Whoever starts work sets its deadline: AgentPolicy `maxExecutionDuration`, the home's and the remote executor's `maxDuration` are optional, `run` and `resume` take `--max-duration DURATION\|none`, and one helper decides when a deadline has passed; managed Kubernetes sessions reach the gateway through a loopback forwarder that adds the current projected token to each CONNECT. |
| [0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md) | The remote executor owns its sessions through the coordinator: the work catalogue keeps intent, project, executions, the task's result and the pull, and derives its phase from the coordinator's; work-token tasks take `workTokenMaxDuration`; `retainedBytes` bounds the executor's storage; stopped version 1 records stay readable. |
| [0058](0058-lifecycle-records-declare-states-and-transitions.md) | Lifecycle records declare a typed phase validated on load and save, a `(from, event) → to` table with a deadline column, and one apply function for live code and recovery; the execution journal derives its phase from its structure and adopts it first. |
| [0060](0060-no-state-waits-on-a-deadline-to-end.md) | No state waits on a deadline to end: a start or move that its deadline overtakes ends as `abandoned`, once nothing can run at its destination, and leaves the execution idle with its last checkpoint, so `resume` continues the session instead of `cancel` being the only way on. |
| [0061](0061-a-stop-survives-an-interrupted-write.md) | A stop survives an interrupted write: capture keeps the rows a fenced writer finished; the login store's lock and a stop's save are bounded and a skipped save is logged; only a whole login, or a sign-out two syncs see while the harness runs, crosses between a login file and its store, nothing after a kill, and the store is rewritten so that a stopped write is never empty. |
| [0062](0062-one-exit-rule-and-exits-over-the-wire.md) | One rule decides what a stopped visit keeps on Docker and Kubernetes: an exited visit whose stopped state the capture helper refuses, by its own exit code, keeps what it started from, any other capture failure is retried, a helper older than the codes keeps each placement's earlier behaviour, a lost quiesce reply is a graceful stop, a discard is logged, and a stopped Claude Code container is never captured; a first Kubernetes visit may keep no checkpoint after binding a conversation; an `exited` placement action lets home's bounded exit watcher stop a visit on a cluster, and an older executor reports no exits. |
| [0063](0063-a-session-releases-executions-it-never-recorded.md) | A home session releases the executions it never recorded, which an interrupted `resume` leaves: one that never ran is cancelled and deleted at the session's next `resume` or `rm`, one that ran is kept and reached by `cancel` and `rm` of its own ID, and an unreadable journal is kept and logged. |
| [0064](0064-the-supervisor-alone-stops-a-harness-at-its-deadline.md) | The supervisor alone acts on the execution's deadline: one `deadline` event, raised once by its watcher or a `quiesce` that finds it passed, unless the harness already exited by itself; `activate` checks it again just before the launch; the harness's terminal no longer has a deadline of its own. |
| [0065](0065-a-revoked-project-settles-interactive-sessions.md) | A revoked project settles interactive sessions: the remote executor's driver carries out a held cancellation, or cancels for the revoked project, an interactive session whose stop or cancellation is unsettled, since its owner can no longer recover it. |
| [0066](0066-a-contradicted-retained-record-refuses-work-and-never-blocks-an-end.md) | A contradicted retained record of a Kubernetes visit refuses work: `Prepare`, `Activate` and `Attach` answer `conflict`; `Inspect`, `Exited`, `Stop`, `Abort` and `AbortPreparation` continue from the facts that hold, take what the registration's phase proves from it, observe the rest again and never trust a forgotten fence. |
| [0067](0067-a-start-ends-when-its-run-has-ended.md) | A start ends when what it would start from has ended: the Kubernetes placement refuses a start whose AgentRun ended, was deleted or lost its registration before its Pod, naming the run's reason; the coordinator ends it through `abandoned`, deadline or none; the driver cancels such a task with that problem and keeps such an interactive session; the placement protocol carries it as `refused` (409) with the reason; a cancellation's wait for its Pod to stop is bounded at 90 seconds. |
| [0068](0068-a-lost-finish-is-proven-by-the-helpers-end.md) | A lost finish is proven by the helper's end: the checkpoint helper waits, at most 10 seconds, for the client that asked it to finish to read the reply and close; a Kubernetes stop whose `finish` exec fails reads the Pod for up to 15 seconds and is complete when it shows the helper ended with code 0, or the Pod gone, beside a stopped registration and a fenced writer, and stays unproven otherwise; a failed fixed command keeps the exec's error text, without the exec request's URL. |
| [0069](0069-a-failed-chatgpt-refresh-is-remembered.md) | A failed refresh of the gateway's ChatGPT sign-in is remembered beside it: a 400 or 401 `invalid_grant` refuses it until the stored sign-in changes, any other failure holds further refreshes for 10 seconds while a still-valid access token keeps serving, a new sign-in ends either within the 10-second reload interval, the 503 a model request sees names the cause, and the refresh runs in a goroutine the authenticator owns, so no caller waits past its own deadline and a started refresh is saved; the token endpoint's redirects are never followed. |
| [0070](0070-gateway-routes-declare-admission-and-requests-record-one-decision.md) | Every gateway route declares how it is admitted (`AdmitCapability` or `AdmitSelected`), both compositions validate them before serving and the dispatcher refuses an undeclared route; the dispatcher admits through `gateway.Admit`; each dispatched request records one decision through `gateway.Record`, declares a deliberate omission with `gateway.Unrecorded`, or gets an `unrecorded` record from the dispatcher; audit records gain `outcome` while `allowed` is unchanged; the dispatcher's budget and handler-less refusals and `LeakGuard`'s refusal are recorded; `gateway.Spend` tells a refused budget from an unavailable one; one test enumerates every composed route. |
| [0071](0071-a-pod-gets-a-key-placeholder-only-for-a-client-that-reads-it.md) | A Pod gets a key placeholder only for a client that reads it: the controller sets `OPENAI_API_KEY` in an AgentRun Pod's workload containers only for a harness whose registry entry names it in `RunKeyEnv` (Codex) and for a runtime that names no harness; every other harness's launch file or runtime command sets its own placeholder, which the registry guard checks. |
| [0072](0072-the-managed-harness-contract.md) | Extends ADR 0039 and changes its sentence that behaviour stays in code. A managed harness is registry facts and two contracts: the registry holds its facts, its argument syntax (`ModelFlag`, `ConfigOverride`) and one capture function (`CaptureState`); its conversation state implements `harness.ConversationState`, which every checkpoint operation goes through, with a conformance test per kind; its supervisor adapter is a guarded struct of required function values; every scope still branching on a kind must name every managed kind; local setup refuses a provider it does not know. |
| [0073](0073-codex-harness-arguments-fail-closed.md) | Codex harness arguments fail closed: the validator reads each `-c`/`--config` key as a TOML dotted key and refuses one it cannot read, one with no `=`, a controlled key, a key below one and a table that holds one; the whole `mcp_servers` table is controlled (awaiting the owner); `-c`, `--config`, `--model` and `-m` need a separate value that is not an option; arguments are checked where a launch is created and started, not when a stored record loads. |
| [0074](0074-account-requests-only-with-a-forwarded-sign-in-or-no-api-authority.md) | Extends ADRs 0047 and 0048. `Provider.Routes` serves a provider's account requests on another capability's host only when its credential forwards the harness's own sign-in or the provider has no API authority, so a gateway-held key or token for a provider's API never reaches another host; both gateway compositions take this rule from the one function. |

## Reserved numbers

These numbers belong to records that are not on `main` yet. Do not use them.

| Number | Record |
| --- | --- |
| 0055 | Release Please, proposed on an unmerged branch. |
| 0059 | Renovate proposes pin updates, in a change based on main. |

## Removed

These records were deleted because they no longer describe the system. Each row
names the record or page that now holds anything still true. "Removed in" lists
the history of the file, which ends at the removing commit once merged; "last
text" is the file as it stood on `main` before removal.

| Number | Title | Why removed | Now held by | Removed in | Last text |
| --- | --- | --- | --- | --- | --- |
| 0005 | Controlled Docker launch before adopting existing sessions | A sequencing decision, fulfilled: the controlled launch exists, and adoption of directly started sessions stays deferred. | [Session mobility](../explanations/future-architecture/session-mobility.md#adopt-directly-started-sessions), [execution runtimes](../explanations/future-architecture/execution-runtimes.md), and the no-host-bind rule in [ADR 0010](0010-managed-session-authority-and-admission.md) and [ADR 0011](0011-native-session-compatibility-and-completion.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0005-controlled-docker-launch-before-session-adoption.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0005-controlled-docker-launch-before-session-adoption.md) |
| 0006 | Native CLI and terminal access before GUI integration | Its flat verbs `serve`, `start`, `attach` and `move` were replaced by ADR 0037's grouped surface, and its fixed remote HTTPS profile by ADR 0036. The terminal transport it chose moved to ADR 0011. | Terminal transport: [ADR 0011](0011-native-session-compatibility-and-completion.md). Command surface: [ADR 0037](0037-one-command-surface.md). Deferred GUI/IDE access and no custom chat UI: [harness portability](../explanations/future-architecture/harness-portability.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0006-native-cli-and-terminal-before-gui-mobility.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0006-native-cli-and-terminal-before-gui-mobility.md) |
| 0007 | Codex before additional session adapters | A sequencing decision, fulfilled; its "no generic harness registry" was replaced by ADR 0039. | [ADR 0011](0011-native-session-compatibility-and-completion.md), [ADR 0039](0039-one-harness-registry-and-the-mcp-requirement.md) and [ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0007-codex-round-trip-before-harness-expansion.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0007-codex-round-trip-before-harness-expansion.md) |
| 0017 | Runtime checks at run admission and bounded replay records | Its single active slot was replaced by ADR 0027's caps, and its replay records went with the event webhook removed by ADR 0043. Its runtime check at admission, live reads of the run list and of an absent sandbox, and the alternatives and revisit conditions for them still hold and moved to ADR 0027. | [ADR 0027](0027-configurable-concurrent-admission.md) (runtime check, live reads); [ADR 0043](0043-remove-the-event-webhook.md) and [event webhooks](../explanations/future-architecture/event-webhooks.md) (replay records). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0017-runtime-admission-and-replay-retention.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0017-runtime-admission-and-replay-retention.md) |
| 0019 | Derived pull operations and work owner names | Its owner-name rule (`WorkConnections` resolving `workSources` then `remote`) was replaced by ADR 0036, and `home.Config` now rejects both keys. Its derived pull operation is still true and moved to ADR 0036. | [ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0019-pull-operations-and-work-owner-names.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0019-pull-operations-and-work-owner-names.md) |
| 0022 | Webhook HMAC Secrets live in their own namespace | The event webhook it governed was removed by ADR 0043. | [ADR 0043](0043-remove-the-event-webhook.md) and [event webhooks](../explanations/future-architecture/event-webhooks.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0022-webhook-hmac-secrets-in-own-namespace.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0022-webhook-hmac-secrets-in-own-namespace.md) |
| 0041 | The gateway's state has its own namespace, and retained state has owners | Its `sproozi-budget` namespace was replaced by `BudgetLedger` resources in `sproozi-system` (ADR 0045), the rest was held by two records. Its decisions moved unchanged; ADR 0045 adds what the code does beyond them. | Ledgers, retention, the gateway's read-only registration Role, the remote executor's `sproozi-sessions`, startup checks, the managed-session end rules and session registrations: [ADR 0045](0045-budget-ledgers-are-a-custom-resource.md). The Docker removal retry: [ADR 0020](0020-docker-placement-removes-stopped-visits.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0041-gateway-state-namespace-and-retained-state-owners.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0041-gateway-state-namespace-and-retained-state-owners.md) |
| 0042 | Idempotent webhook delivery and one response before verification | The event webhook it governed was removed by ADR 0043. | [ADR 0043](0043-remove-the-event-webhook.md) and [event webhooks](../explanations/future-architecture/event-webhooks.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0042-idempotent-webhook-delivery.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0042-idempotent-webhook-delivery.md) |
| 0046 | GitHub Copilot CLI is an AgentRun harness in bring-your-own-key mode | Copilot became a managed harness in ADR 0048, `--no-remote` was removed (the decision is in ADR 0048), and the released image now installs it. The AgentRun decisions that still hold moved to ADR 0048. | [ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md#agentrun-harness-and-bring-your-own-key-mode); the Copilot sign-in routes are in [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0046-github-copilot-cli-harness-in-byok-mode.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0046-github-copilot-cli-harness-in-byok-mode.md) |
| 0052 | Relay Copilot's policy-fetch query | Its query allowance on the exact `managed_settings` route was replaced by two GET prefixes (ADR 0048). Its refusal diagnostics, which report the method and whether a query was present but never the query, still hold and moved to ADR 0048. | [ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md#account-namespaces-and-policy-verification). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0052-copilot-policy-fetch-queries.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0052-copilot-policy-fetch-queries.md) |
| 0053 | Copilot account namespaces and policy verification | Copilot is one managed harness with one record. Its decisions did not change: GET relays under both account namespaces, the separate registry-policy probe in CI, and the refusal diagnostics it absorbed from ADR 0052 moved to ADR 0048 unchanged but for headings. | [ADR 0048](0048-github-copilot-cli-is-a-managed-harness.md#account-namespaces-and-policy-verification). | [history](https://github.com/andrewmccall/sproozi/commits/main/docs/adr/0053-copilot-account-namespaces-and-policy-verification.md) | [text](https://github.com/andrewmccall/sproozi/blob/944dfae70f49bf8c1965fcc9e235cf6dd201357f/docs/adr/0053-copilot-account-namespaces-and-policy-verification.md) |
