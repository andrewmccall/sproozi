# 0015: Default agent clients and gateway compatibility

Status: accepted; package selection replaced by [ADR 0021](0021-pinned-ci-inputs-and-latest-client-canary.md)
Date: 2026-10-16

## Problem and constraints

Sproozi previously selected exact client releases in installers, executable
checks, saved-state parsers and Kubernetes registrations. Updating a client
required coordinated version edits even when its state and protocol remained
compatible. Client installs must use upstream defaults without version selectors.

Current Claude Code also rejects a top-level `allOf` in an MCP input schema.
The gateway added this wrapper even when a tool had no argument restrictions.
That prevented the client from loading otherwise compatible tools.

Codex can generate a title while submitting a user turn. Requests without an
output bound reserve the remaining model allowance. One overlapping request
could therefore receive a budget-exhausted error while the other was still
settling. The TUI treated that temporary contention as a failed turn.

## Decision

Install Codex, Claude Code and OpenCode with unversioned package names. Pull the
official Hermes image without a release tag or digest in the authored source.
Rebuild client images with fresh package resolution in acceptance tests.
[ADR 0021](0021-pinned-ci-inputs-and-latest-client-canary.md) replaces this
paragraph: CI and the verifiers now install pinned lockfiles and a Hermes
digest, and a weekly canary tests the latest releases.

Remove exact-version checks from activation, state parsing and registration.
Keep explicit harness selection, conversation identity, supported state formats,
completed-turn checks, instruction checks and independent writer-stop and usage
accounting. Recorded client versions remain descriptive metadata.

Resolve image content when preparing a deployment or execution. That identity
binds its resources and retries; it does not select a fixed upstream release.
Read the retired `nativePin` storage field only to recover existing journals.
It does not appear in new registrations or affect permission or compatibility.

For MCP discovery, return the provider's input schema directly. Describe
additional policy argument restrictions in the tool description. Validate the
provider schema and policy restrictions independently before forwarding a call.
The discovery description cannot grant permission.

Accept the observed Claude metadata caches, primary-chain context attachments,
API request caches, turn-duration records and Stop summaries that omit a
separate progress record.
Preserve native transcript bytes and chain identity. These records cannot resolve
pending tools, establish live completion or select destination launch settings.
Prompt snapshots are history; fresh launch arguments and independent hook, writer
and accounting checks still govern activation and movement.

Serialize model inference requests sharing one budget account in the active
gateway. A waiting request can be cancelled and remains subject to its transport
and execution deadlines. Different accounts remain independent. Release the
request slot after settlement or failure; retain the durable ledger as the
source of budget authority. Restarting the gateway cannot release an unknown
reservation. Discovery requests do not enter this queue.

This replaces the version-pinning portions of ADRs 0008, 0010 and 0011, and the
combined discovery-schema presentation in ADR 0001. It also replaces the pinned
client versions described in ADRs 0002, 0003, 0007 and 0012. Their state, lifecycle,
credential and authorization boundaries remain in effect.
The shared native launch and reply checks in [ADR
0014](0014-review-boundaries-and-native-contracts.md) also remain in effect.
They validate the launch contract and selected conversation without selecting
an exact client release.

## Alternatives and tradeoffs

Keeping a release allowlist would make historical combinations explicit, but
would retain manual upgrades and reject structurally compatible state. Optional
version configuration would still create a version-selection workflow. Both
conflict with the selected upstream-default policy.

Accepting arbitrary state would remove more checks but would lose conversation,
completion and ownership guarantees. Version independence does not require that
change. Unsupported state still fails validation.

Flattening arbitrary JSON Schema intersections would require complex schema
rewriting and could change their meaning. Client-specific MCP adapters would
multiply integration code. Keeping the provider schema and enforcing policy at
the gateway preserves the two independent validators. The cost is that clients
read additional scope from descriptions instead of the input schema alone.

Independent allowances for titles and user turns would split a single execution
budget and require client-specific classification. Assuming a smaller token
reservation would weaken accounting. Relying on client retries does not work
when the native TUI treats the denial as final. Serializing the shared account
avoids these changes, at the cost of waiting behind an in-flight inference.
The queue is local to the existing single active gateway owner. The durable
ledger still prevents overspending across owners or restarts.

Default packages can change between builds. Tests must exercise the installed
clients and record their actual versions and image identities. Existing images
do not update in place during a running execution.

## Testing requirements

The [acceptance requirements](../../test/acceptance/README.md) cover native resume,
conversation identity, tool use, denied operations, cancellation and accounting.
Test state from more than one client version, reading old registration records,
MCP discovery without an added `allOf`, and independent policy denials.
Native deferred-tool discovery must use the client's real search and tool-result
flow. Fixture responses need unique message and tool-call IDs and must obey
requested structured output schemas. Test native context records, rejected foreign metadata,
broken attachment parents and unresolved tools. Cached messages and tool results
cannot replace primary-chain records. Concurrent unbounded inference requests
must both finish and settle usage without a false exhaustion error. Test queued
cancellation, independent accounts and unknown reservations after restart.

## Revisit conditions

Revisit package selection if upstream stops providing a usable default channel
or a deployment requires a separately governed release policy. Revisit schema
presentation if a supported client can consume policy constraints without
changing provider-schema meaning. Extend state support when actual client output
requires a new parser contract. Revisit per-account serialization when provider
bounds can support parallel admission without dividing or enlarging the allowance.
