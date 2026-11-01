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
and what was fixed or left for later. Do not rewrite earlier records to match.

Update affected explanations, references and examples. Update the main README
when supported capabilities change. Compare claims with code and test evidence,
then run `make verify-docs`. The command checks required documents and links. It
does not check documentation accuracy or decision coverage.

Preserve accepted decision history. To replace a decision, add a new ADR, mark
the old record superseded and link both records.

## Decisions

These records preserve the reasons for each design. A later record can replace
part of an earlier decision. ADR 0015 replaces exact client-version selection
and the combined MCP discovery schema. ADR 0021 replaces ADR 0015's unversioned
client installs with pinned lockfiles and digests for CI and the verifiers.
ADR 0027 replaces the single active-run slot from ADRs 0003 and 0017, and ADR
0016's rejection of a revalidation cache. ADR 0028 replaces ADR 0002's buffered
model responses and response bound, narrows ADR 0009's full charge to calls
that can't be estimated and budget cuts, limits ADR 0015's per-account serialization to
unbounded calls, and settles ADR 0015's unknown reservations after a restart
at their recovery charge, an estimate, once their leases expire. ADR 0048 makes
Copilot, an AgentRun harness in ADR 0046, a managed one. ADR 0049 replaces ADR
0047's login storage with a live store per named login.

ADR 0040 removes ADR 0014's kube-dns exception: Kubernetes sandboxes resolve
no names. ADR 0041 gives ADR 0020's failed Docker removals an owner, the local
service's periodic retry. ADR 0045 replaces ADR 0041's `sproozi-budget`
namespace with `BudgetLedger` resources in `sproozi-system`.

ADR 0043 removes the event webhook, superseding ADRs 0022 and 0042 and the
replay records of ADR 0017.

ADR 0029 extends those pinned clients to the published native image, replacing
ADR 0024's statement that releases do not pin clients.

| Decision | Topic |
| --- | --- |
| [0001](0001-mcp-capabilities-through-shared-gateway.md) | MCP access through the shared gateway. |
| [0002](0002-native-cli-harnesses-and-model-protocols.md) | Native CLI harnesses and model protocols. |
| [0003](0003-persistent-assistants-and-retained-results.md) | Persistent assistants and retained job answers. |
| [0004](0004-portable-sessions-and-bounded-executions.md) | Sessions across bounded executions. |
| [0005](0005-controlled-docker-launch-before-session-adoption.md) | Controlled Docker launch before adopting existing sessions. |
| [0006](0006-native-cli-and-terminal-before-gui-mobility.md) | Native CLI and terminal access before GUI integration. |
| [0007](0007-codex-round-trip-before-harness-expansion.md) | Codex before additional session adapters. |
| [0008](0008-selected-native-and-git-checkpoints.md) | Selected conversation and Git checkpoints. |
| [0009](0009-runtime-backends-and-sealed-accounting.md) | Runtime backends and finalized usage accounting. |
| [0010](0010-managed-session-authority-and-admission.md) | Managed-session permissions and destination admission. |
| [0011](0011-native-session-compatibility-and-completion.md) | Native state compatibility and completion. |
| [0012](0012-retained-work-and-fresh-session-continuations.md) | Retained work and local resume. |
| [0013](0013-configured-projects-and-workspace-revisions.md) | Configured Projects and workspace revisions. |
| [0014](0014-review-boundaries-and-native-contracts.md) | Shared gateway limits, native contracts and CLI identities. |
| [0015](0015-default-clients-and-native-tool-schemas.md) | Default clients, MCP schemas and model requests sharing an allowance. |
| [0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md) | Capability checks at the gateway dispatcher and fresh revalidation reads. |
| [0017](0017-runtime-admission-and-replay-retention.md) | Runtime validation at admission, live slot and sandbox reads, and bounded replay records. |
| [0018](0018-coordinator-owns-every-visit.md) | The home coordinator owns every visit, including the first, and launches never change. |
| [0019](0019-pull-operations-and-work-owner-names.md) | Derived pull operations and one rule for resolving work owner names. |
| [0020](0020-docker-placement-removes-stopped-visits.md) | The Docker placement removes a stopped visit's containers and volume after recording it. |
| [0021](0021-pinned-ci-inputs-and-latest-client-canary.md) | Pull request CI runs only pinned clients, images and manifests; a weekly canary reports upstream drift. |
| [0022](0022-webhook-hmac-secrets-in-own-namespace.md) | Superseded by 0043. Webhook HMAC Secrets live in their own namespace, away from provider credentials. |
| [0023](0023-controller-writes-only-in-sproozi-agents.md) | The controller writes sandbox objects only in sproozi-agents; its cluster role keeps reads. |
| [0024](0024-native-host-platforms-and-cli-releases.md) | Native host security and automated CLI releases with a Homebrew tap. |
| [0025](0025-managed-harness-integration-and-diagnose.md) | Every harness receives the Sproozi integration, and the gateway explains an execution's own denials. |
| [0026](0026-single-gateway-refresh-owner-with-recreate.md) | One gateway replica with a Recreate rollout owns the rotating ChatGPT credential refresh. |
| [0027](0027-configurable-concurrent-admission.md) | Optional cluster and policy caps replace the single active-run slot; the gateway serves revalidation from a watch cache. |
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
| [0041](0041-gateway-state-namespace-and-retained-state-owners.md) | Budget ledgers move to `sproozi-budget` and the remote executor to `sproozi-sessions`, so neither the gateway nor the executor can write ConfigMaps in `sproozi-system`; the gateway prunes settled ledgers of deleted runs; the local service finishes Docker removals; registry records keep no expiry yet. |
| [0042](0042-idempotent-webhook-delivery.md) | Superseded by 0043. Webhook runs are named from the event, replay records track delivery, and every failure before verification returns 401. |
| [0043](0043-remove-the-event-webhook.md) | The event webhook is removed until a supported sender exists; its design and gotchas move to a future-architecture note. |
| [0044](0044-the-api-stays-alpha-with-a-written-policy.md) | The CRDs stay `v1alpha1` under a written compatibility policy, with criteria for moving to `v1beta1`; the 1.0 promise is dropped. |
| [0045](0045-budget-ledgers-are-a-custom-resource.md) | Budget ledgers become `BudgetLedger` resources in `sproozi-system`; write access to them grants no ConfigMap writes, so the `sproozi-budget` namespace is removed. |
| [0046](0046-github-copilot-cli-harness-in-byok-mode.md) | GitHub Copilot CLI is an AgentRun harness that loads its MCP file natively and calls the OpenAI route in bring-your-own-key mode, offline from GitHub. |
| [0047](0047-harnesses-sign-in-to-their-own-model-providers.md) | Every harness offers every credential mechanism its provider accepts: a gateway-held key, a gateway-held sign-in, or its own sign-in forwarded and inspected by the gateway, from one provider table. |
| [0048](0048-github-copilot-cli-is-a-managed-harness.md) | GitHub Copilot CLI is a managed harness: the supervisor names each session, observes completion in its events journal, checkpoints only that journal, and reaches OpenAI with a key or GitHub's Copilot service with a token or its own sign-in. |
| [0049](0049-named-logins-share-a-live-store.md) | A harness's own sign-in lives in a named login store that the supervisor keeps in step with each running session, a Docker volume or a controller-created claim per harness and login. |
| [0050](0050-prebuilt-cli-and-explicit-image-builds.md) | Prebuilt Homebrew bottles, explicit native/control image builds and removal of Copilot’s obsolete remote flag. |
| [0051](0051-interactive-sessions-may-run-without-a-model-budget.md) | A local session's model allowance may have no limit, which setup now writes; every other capability keeps a unit bound and the Kubernetes placement keeps requiring one. |
