# Testing requirements

This document defines the behaviours that changes to Sproozi must preserve.
Use the sections affected by a change. [Run acceptance checks](running.md)
describes the existing commands and required setup.

Run cluster tests in a dedicated Kind cluster. Keep credentials and raw provider
output out of committed reports. Use deterministic providers for repeatable
protocol tests. Use provider-backed tests when checking a real provider contract.
Neither test type establishes the quality of an agent's proposed change.

## Kubernetes runs

Tests must check that:

- Admission resolves the approved template, policy and runtime. A caller cannot
  select another image, change trusted instructions or enlarge capabilities.
- A permitted operation succeeds through the gateway. A forbidden namespace,
  resource, repository or capability returns the expected denial without an
  upstream side effect.
- Direct API and provider calls fail from the agent Pod, including when it shares
  a node with the API server. A broken client or trust bundle is not a denial.
- The `agent` container's exit status determines ordinary run success. Cancellation
  and deadline expiry stop the run independently of agent output.
- Cleanup removes the owned Pod, identity, network policy and contract. Interrupted
  provisioning is a separate recovery scenario; normal cleanup cannot substitute
  for testing it.
- Retained final answers match the exact Run UID and size limit. Missing, invalid,
  oversized or foreign results do not become successful answers.

The PR demo must also check the target repository, base branch, run-owned branch,
commit and allowed file changes. It must not merge or deploy a proposed fix.
[cases.json](cases.json) catalogues planned acceptance scenarios. It is a design
checklist, not evidence: no gate or test checks its entries. The planned
[home-ops acceptance procedure](home-ops.md) covers alert delivery, a GitOps PR
and recovery after review and merge.

## Gateway and MCP

Tests must check:

- Native Kubernetes REST and MCP calls share the same namespace/resource rules
  and `kubernetes.read` usage account.
- Configured MCP tools require registration, a named run capability and explicit
  policy permission. Arguments satisfy both provider and policy schemas.
  Discovery keeps the provider schema usable by native clients and describes additional policy restrictions
  without adding a top-level `allOf`.
- Two servers with different schemas work through the same configured bridge.
  Missing tools, invalid schemas, oversized messages and malformed requests fail
  without falling back to destination access.
- Failed upstream tool attempts consume the configured unit. Reconnection and
  policy changes do not reset usage; separate runs keep separate accounts.
- Provider credentials stay in the gateway. Registration and credential changes
  take effect after restart. Workload headers cannot select another account.
- Removing permission, cancellation and deadline expiry deny later requests and
  interrupt active requests. Test stale credentials from a separate contained
  client after the original Pod stops.
- Audit records identify the run, capability, operation and outcome without
  exposing arguments, results, credentials or provider error bodies.
- Upstream cancellation and session deletion use bounded timeouts. A committed
  provider side effect cannot be reversed by cancelling its request.

When changing the MCP SDK, include revocation during an in-flight call and
session teardown. The tests must detect cancellation notifications lost during
asynchronous shutdown.

## Harnesses and persistent Hermes

Install agent clients from the pinned lockfiles in `hack/clients` with install
scripts disabled, and run the Hermes image by its pinned digest
([ADR 0021](../../docs/adr/0021-pinned-ci-inputs-and-latest-client-canary.md)).
Record observed versions and resolved image identities in test output. Product
code must not reject a client solely because its version differs from a past
run; the weekly latest-clients canary runs the same tests on newer releases.

For each supported ordinary harness, tests must check generated configuration,
trusted launch settings, a real model/tool loop and completion handling. An API
error must not count as success because a client exits zero. Check both allowed
calls and capability/budget denials through the deployed gateway.

Persistent Hermes tests must cover native chat and scheduled task submission,
exact-request retries, UID-bound status and cancellation, retained answers, PVC
restart and direct-network restrictions. Multiple jobs must run concurrently,
or queue under a concurrency cap, without the coordinator counting as an active
run. Worker budgets must remain separate from
coordinator inference.

Provider-authentication changes need callback and identity validation, private
storage, refresh serialization, persistence retry and stale-writer rejection.
A mock callback test does not replace a real sign-in compatibility check.
Model tests must cover usage settlement, missing prices, cache categories,
unsupported billable options and ambiguous responses without crediting unknown
consumption as zero. Concurrent calls sharing an allowance must wait for
settlement without a false budget denial. Waiting must respect cancellation;
separate accounts must remain independent. Gateway restart must preserve held
or unknown ledger reservations.

## Retained work and Projects

Tests must check that remote work can start while the laptop is absent. Completion
must belong to the submitted task and a new native turn. Retention requires a
stopped worker, finalized usage and a stored compatible checkpoint.

Pull must transfer ownership once without starting an agent. Retrying an operation
after a lost reply must not create another session or transfer. Resume must keep
the native conversation ID while creating a new execution, allocation, deadline
and current grant. Earlier usage records must remain unchanged. Delayed stop,
move and cancel requests must not affect a later execution.

Project tests must cover membership, filtering, capability narrowing and separate
local approval tied to the source installation. Imported Project labels cannot
grant access. Test removal of approval during resume recovery and movement.

An exact-revision launch must use the stopped checkpoint's full Git HEAD and
approved repository identity. It must create a clean workspace and fresh native
conversation. Dirty changes and the source conversation must remain excluded;
the source checkpoint and ownership must remain unchanged. Include unpublished
commits, invalid revision requests, storage limits and interrupted preparation.

Use isolated read-only Kubernetes targets for home-ops scenarios. Personal
infrastructure and paid engineering outcomes require separate, explicit setup.

## Session movement extensions

Before extending managed session support, test each affected client and version
across Docker, Kubernetes and Docker. Check the native conversation ID, earlier
conversation facts, original deadline, aggregate allowance and exact Git state.
Include history, index, working changes, untracked files and selected ignored
files. Check excluded credentials, hooks and client configuration remain absent.

The failure scenarios must include:

- A move during a held foreground turn. New input is blocked while supported
  tools finish; the destination starts only after source stop and accounting.
- Forced cancellation during a move. Access is revoked first, no destination
  starts, and unknown usage keeps the allowance held.
- Capture, restore and admission failures, restarts, stale owners, lost replies,
  unknown stop and unknown usage.
- Fresh destination credentials and MCP settings, plus denials of old credentials
  and direct network access at both placements.

Run the full held-move and cancellation matrix separately for Claude Code; Codex
results do not establish Claude behaviour. Longer histories, compaction,
background tools, subagents and another native version each need their own cases.
Adoption tests must stop the original writer and reject imported authority.

## Local onboarding and certificates

Tests must check that:

- `init --local` reaches a working native terminal without a checkout, OpenSSL,
  image IDs or network edits, and never calls kubectl or a Kubernetes API, even
  with a kubeconfig configured.
- Repeating or interrupting setup reuses the same network, certificate
  authority, credentials and configuration, and keeps user edits.
- Readiness requires the restricted worker path: verified gateway TLS, and
  failed attempts to reach a public address, the provider directly, another
  gateway port, metadata, direct DNS and IPv6. A probe without the anchor must
  reach those paths, so the check is shown to discriminate.
- Each harness's worker reaches only its own gateway port and provider key.
- Provider keys, CA keys and journals never reach worker mounts, environment,
  checkpoints or the agents directory. Shared agent files reach only harnesses
  that load them, are rejected when they are links or unsafe files, and never
  enter a checkpoint.
- A renewed leaf reaches new connections without restarting the gateway, an
  invalid replacement keeps the previous leaf, and a leaf never outlives its
  issuer. Rotation keeps old and new trust until every recorded consumer has
  the new generation or has independently stopped; a restart at any stage
  resumes it; a missing key never creates an unrelated authority.
- In Kubernetes, release CRDs are applied and established before custom
  resources; the self-managed CA works without cert-manager; trust bundles are
  new immutable objects; and the certificate controller cannot read other
  Secrets, change policy or act cluster-wide.

`make verify-local-onboarding` and `make verify-kubernetes-release` cover these
on one engine and a Kind cluster. Linux Docker Engine and Windows need their own
recorded runs before they are advertised.

## Runtime extensions

A new backend must pass equivalent admission, filesystem, network, resource,
lifetime, cancellation and restart-recovery checks. Confirm that all processes
able to write have stopped; terminal detach or process-group exit is insufficient.
Restore state before activation. Unknown source stop or usage must keep the
allowance held and prevent another writer from starting. State backend-specific
host and engine limits in the test setup.

## Harness and client extensions

Test native CLI resume, remote terminal and GUI/IDE connections independently.
Include native deferred-tool discovery, unique response and tool-call IDs,
and requested output schemas, including auxiliary title requests. Check
authentication, session discovery, path changes, detach/reconnect,
revocation and completion. Check that context, prompt snapshots and request
caches cannot authorize launch settings, complete unresolved tools or replace
fresh Stop observations. Each supported state format must preserve its native
meaning without importing cached permissions or launch settings.

For an external agent loop, test disconnects, stale commands, retries, explicit
task completion and idle-executor cleanup. Confirm tools execute only in the
approved environment. Publish a supported client/runtime matrix when adding a
combination.

## Managed MCP servers

A managed-server test must demonstrate separate server identity, inaccessible
provider credentials, service-native restrictions, allowed and denied calls,
live revocation and cleanup after restart. Test reuse with two separately scoped
executions and attempted cross-run access. A tool name or schema cannot establish
that a database operation is read-only.

## Other proposed extensions

| Extension | Required first scenario |
| --- | --- |
| Agent delegation | One scoped child task with separate permission, cost and cancellation ownership; denied calls and cleanup. |
| Cross-harness handoff | Useful context transferred to a fresh conversation, with source references and explicit omissions. |
| Multiplayer | Two authenticated viewers and one explicit transfer of write/input control; stale control requests rejected. |
| Project roles | Individual identity, role administration, revocation and isolation between shared installations. |
| Session forks | One supported client forks stopped state without changing its parent or inheriting permission. |
| Workspace reconciliation | Two isolated proposals with a visible conflict and an explicitly chosen result. |
| Memory and indexing | One useful query with source references, access filtering, stale-result handling and deletion. |

## Historical reports

[Archived reports](reports/README.md) preserve earlier observations and source
versions. They help diagnose regressions. They do not replace these requirements
or establish that the current checkout passes them.

Native host and release checks are specified in [platform requirements](platforms.md).
