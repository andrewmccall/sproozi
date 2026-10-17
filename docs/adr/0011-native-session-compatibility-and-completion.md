# 0011: Native session compatibility and completion

Status: accepted
Date: 2026-10-11
Implementation: pinned foreground Codex and Claude compatibility verified
Live extended Codex and ordinary Claude roundtrip acceptance: passed on 2026-10-12
Unassessed: Claude's full held placement move/cancel matrix and broader native state

Current compatibility policy: [ADR 0015](0015-default-clients-and-native-tool-schemas.md)
replaces exact client-version selection and extends supported native context
records. Identity, completion, writer-fencing and accounting rules remain in effect.

## Problem and constraints

[ADR 0007](0007-codex-round-trip-before-harness-expansion.md) selected a complete
Codex round trip followed by an independently verified Claude Code extension.
Both must preserve the stock interactive terminal and native UUID. A common
summary format or a prompt interface around headless execution would change that
experience. Each client also needs its own native state and completion checks.
A provider's final response, displayed assistant text and a stopped PTY are
insufficient evidence that a transferable native conversation exists.

The retained grounding, competing designs and synthesis separated native
continuity from placement authority. [ADR 0008](0008-selected-native-and-git-checkpoints.md)
records selected checkpoints; [ADR 0009](0009-runtime-backends-and-sealed-accounting.md)
records runtime fencing and sealed accounting; [ADR 0010](0010-managed-session-authority-and-admission.md)
records fresh admission and authority. This decision covers the client-specific
compatibility and completion mechanisms exposed by the second adapter.

## Decision and reasons

Require an explicit immutable native kind, `codex` or `claude-code`, in managed
launches and admission. An empty selector is invalid. Pin Codex to 0.161.0 and
Claude Code to 2.1.27. Activation observes the actual fixed executable's version
with a fresh explicit environment and a bounded observation time. Native history
must separately match the selected UUID and supported version. A session cannot
change its native kind during a move; a destination must support that same kind.

Write version 2 checkpoints with an explicit kind and exactly one matching native
state, plus the selected Git workspace. Retain a separate decoder for version 1
Codex artifacts. That decoder recognizes the old Codex format explicitly; it does
not make an absent selector a default for new artifacts or launch contracts.
Restore validates both components in inert staging and publishes the pair
atomically. Claude's exact primary transcript is restored under the destination's
native project path without rewriting its historical cwd or conversation rows.
Metadata describes the exact artifact and native identity, not permission to run.

Generate trusted launch configuration independently on every visit. Provider
credentials, fresh gateway access, model selection, hooks and native preferences
do not come from the checkpoint. Codex history retains its initial developer
instructions on resume, so admission checks the actual initial instruction digest
and keeps the approved digest invariant. Claude's pinned transcript contains no
approved system instruction block. Its adapter cannot make the same assertion
from native bytes. The trusted launch retains the approved digest independently
and supplies the same instructions through `--append-system-prompt` on every
visit. Provider observations in the native fixture establish that those fresh
instructions reach resumed requests.

Claude receives fixed foreground Bash tools, an empty MCP configuration and
fresh explicit hook settings. User/project/local settings sources are excluded.
Fresh onboarding preferences approve only the workload's placeholder API key and
selected workspace. Settings under private `/tmp` and preferences generated in
the native home are excluded from the selected transcript checkpoint. Historical
permission fields remain opaque; the destination supplies its own fixed native
permission mode. Claude currently accepts a bounded model selection and rejects
administrator options that would otherwise be interpreted as Codex TOML.

For Codex, gate input and inspect the selected journal for its completed final
turn, compatible instructions and resolved supported tools. Then request native
quit and observe the process exit. For Claude, fixed `UserPromptSubmit`,
`PreToolUse`, `PostToolUse` and `Stop` hooks report the exact native UUID, project
path and foreground Bash lifecycle to the supervisor. A submitted prompt clears
prior completion. The supervisor counts accepted attachment writes and binds
completion to the input generation at prompt submission. A prior Stop cannot
satisfy a move after more input was accepted, even if the next prompt hook has
not arrived yet.

Claude quiescence requires both a matching Stop observation with no pending
supported tools and a settled selected transcript. Transcript validation checks
the pinned primary chain, paired foreground Bash calls/results and a final
assistant leaf. It rejects incomplete, foreign, unrecovered API-error and unsupported native
records. A successful Stop summary is validated when present, but is optional:
controlled shutdown can follow the hook before the client appends that summary.
The proven native resume accepts the resulting transcript. Native background
tasks, subagent state, compaction and interrupted turns are outside this contract.
Prompt suggestions and native background-task support are disabled for this
selected foreground workflow.

Use controlled PTY process-group shutdown after Claude completion. The tested
quit input sequence became a literal user prompt in the pinned client and did
not provide a reliable shutdown operation. Controlled shutdown avoids submitting
another conversation turn. It can leave optional trailing native metadata absent;
the post-stop checkpoint validator must still accept the actual selected state.

Hooks, journals and the worker control socket are accessible to the workload.
They supply cooperative completion hints, never authorization, accounting
receipts or proof that every writer stopped. Missing or inconsistent observations
block a graceful move. A workload can tamper with them and make the execution
unavailable, or misreport its own activity. After native quiescence, the placement
revokes authority and independently fences the entire container before capture,
as specified in ADR 0010. Processes that escape the PTY process group remain
covered by that runtime fence. Forced cancellation continues to revoke first.

Use Codex `--sandbox danger-full-access --ask-for-approval never` inside the
managed worker. The actual tool fixture failed in Codex's inner bubblewrap
sandbox because the worker's kernel did not permit the required unprivileged
user namespaces. The confinement boundary is the independently controlled outer
container: non-root identity, read-only root filesystem, bounded writable state,
dropped capabilities, no privilege escalation, and enforced network access through
the gateway. The worker has no host Docker socket, host repository bind or
Kubernetes credentials. This flag choice does not weaken the placement's outer
limits or permit checkpoint contents to select them. Claude's fixed
`bypassPermissions` mode uses the same outer confinement responsibility.

## Alternatives and accepted tradeoffs

- A universal conversation representation would simplify shared parsing, but
  would replace opaque native history and require reimplementing each client's
  resume behavior. Two explicit adapters reuse placement mechanics while retaining
  independent native compatibility checks.
- Copying whole native homes would preserve more settings and indexes, but would
  transfer credentials and ambient configuration. Selected state requires fresh
  launch preparation and rejects unsupported records rather than guessing their
  meaning. It also excludes background and subagent continuity.
- Terminal text or provider `end_turn` alone is easy to observe, but does not
  establish persisted history or resolved tools. Claude needs lifecycle hints and
  a checked transcript. These checks remain conservative and workload-accessible;
  the independent container fence is still required.
- Reusing the previous Stop after an attachment write would reduce waiting, but
  races a queued prompt. The input-generation barrier can also block a move after
  an unfinished draft or a late terminal-probe reply. This accepts false refusals
  until a new completed turn rather than interpreting terminal keystrokes as a
  reliable native lifecycle protocol.
- Retrying quit keystrokes would keep a nominal graceful-exit path, but the tested
  sequence changed native conversation history. Claude uses controlled shutdown
  after completion and preserves the selected transcript instead.
- Enabling kernel namespaces or giving the worker more privileges could restore
  Codex's inner sandbox. That would couple launches to extra kernel/runtime setup
  and change the enforced worker boundary. The selected deployment keeps its
  outer restrictions and records the absence of a second sandbox layer.

## Evidence, limits and revisit conditions

The stock Claude fixture passed genuine Bash execution, a held post-tool provider
request, input rejection during that held turn, detach/reconnect to the same live
PTY process, actual Stop hooks, controlled shutdown and fresh-home interactive
resume with the same UUID and conversation facts. The adapter tests retain exact
captured bytes and digests, including the final transcript without an optional
Stop summary. A failed diagnostic displayed an answer and emitted Stop while its
assistant row was absent; the continuity check rejected that state. Display and
hooks therefore remain insufficient without the native transcript.

Unit and race tests cover launch validation, hook identity, pending tools, stale
input generations, executable pinning and supported native state. Checkpoint tests
cover explicit discriminators, legacy Codex decoding and selected Claude restore.
The final dedicated enforcing-CNI Kind proofs passed for both pins using the
current native image. Claude preserved the same UUID through three native TUI
turns, real Bash execution and Git restoration across Docker/Kubernetes/Docker;
eight deterministic Messages requests matched 320 sealed units. Both clients
passed denied capability/direct-HTTP probes on each placement. The Codex proof
also exercised graceful held-turn movement, forced cancellation and conservative
immutable accounting replay. See [recorded acceptance](../../test/acceptance/reports/verified-session-mobility.md).
These proofs do not establish paid-provider, background/subagent or compacted
history compatibility.

Revisit pins only with renewed native TUI, tool, checkpoint and placement evidence.
Revisit Claude instruction validation if a supported native format records the
approved instruction contract. Revisit input-generation refusals if the client
provides a supported lifecycle/attachment protocol that removes the queued-input
race. Broaden background, subagent, compaction or native-option support only with
its own completion and selected-state proof. Revisit the inner Codex sandbox when
the supported kernel can run it without weakening outer confinement. Revisit
controlled Claude shutdown if a supported native quit mechanism preserves history
and reliable completion under the managed terminal protocol.

Implementation lives in [native kind selection](../../internal/harness/native_kind.go),
[trusted launch](../../internal/harness/native_launch.go),
[checkpoint transfer](../../internal/checkpoint/codex.go),
[Claude state validation](../../internal/harness/claude_state.go),
[native supervisor](../../cmd/native-session/main.go),
[Claude lifecycle observation](../../cmd/native-session/claude.go) and
[launch arguments](../../cmd/native-session/launch.go).
The public [Claude native fixture](../../hack/verify/claude-session.py) and
[Codex TUI fixture](../../hack/verify/native-tui.py) preserve the prerequisite checks.
Current support and configuration belong in the
[harness reference](../reference/harnesses.md),
[runtime reference](../reference/agentruntime.md) and
[native command guide](../../cmd/native-session/README.md).
Official [Claude hooks](https://code.claude.com/docs/en/hooks),
[Claude CLI options](https://code.claude.com/docs/en/cli-reference) and
[Codex security](https://learn.chatgpt.com/docs/security) describe the upstream
mechanisms; current upstream documentation does not replace empirical evidence
for these older pinned client versions.

A successful graceful completion acknowledgement is retained by the placement
before it relies on stopped Claude state during recovery. A terminated container
or settled transcript alone cannot recreate that acknowledgement. Losing the
reply before it is persisted makes graceful movement unavailable, even when the
native turn may have completed; forced cancellation can still establish the
physical fence and seal existing accounting. This costs availability to avoid
claiming a completed conversation from unexpected process death. Revisit with a
trusted durable supervisor completion receipt if that ambiguity becomes common.

A live Claude return move exposed a recovered native `api_error` history row from
a gateway 429 reservation response. The following assistant reply and Stop were
valid, but the transcript adapter had treated every system row as a Stop summary.
The adapter now dispatches that captured, bounded retry shape explicitly and
clears its candidate leaf until a later successful assistant reply. Unknown
system/error shapes and a transcript ending at the retry remain incompatible.
The exact native transcript is retained as a regression fixture. Anthropic input
and cache use are unknown before inference, so the gateway reserves remaining
capacity conservatively; concurrent auxiliary calls can receive 429 and retry.
This fix preserves that budget boundary rather than relaxing reservations.
