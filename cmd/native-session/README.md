# Managed native terminal command

Build this command into the approved agent image at `/usr/local/bin/native-session`.
Use it as the agent container's PID 1:

```sh
/usr/local/bin/native-session serve --config /etc/sproozi/native/config.json
```

The launch contract is a bounded read-only file mounted by the trusted runtime.
It supplies these JSON fields:

| Field | Meaning |
| --- | --- |
| `harness` | Required `codex` or `claude-code`; immutable throughout the session |
| `deadline` | Original effective absolute deadline, RFC3339 |
| `threadId` | Selected harness conversation ID for resume; empty only for a new session |
| `instructions` | Actual trusted instruction string supplied independently of portable history |
| `instructionDigest` | SHA256 of the UTF-8 instruction string |
| `env` | Explicit fresh `KEY=value` entries; fixed homes, cwd and Claude behavior switches cannot be replaced |
| `rolloutMaxBytes` | Positive bounded journal size, at most 64 MiB |
| `model`, `options` | Optional trusted model; `key=TOMLvalue` options apply only to Codex and cannot replace approved instructions |
| `trustBundlePEM` | Optional bounded public CA certificate bundle installed outside portable state |
| `gateway` | Optional HTTPS endpoint and fixed projected token/trust paths; mutually exclusive with literal proxy credentials and `trustBundlePEM` |

The selected executable and native home are fixed:

| Harness | Executable | Portable native home |
| --- | --- | --- |
| `codex` | `/usr/local/bin/codex` | `/workspace/session/codex` |
| `claude-code` | `/usr/local/bin/claude` | `/workspace/session/claude` |

The workspace is `/workspace/session/workspace`. `HOME` is `/home/agent`, separate
from portable state; Codex receives `CODEX_HOME` and Claude receives
`CLAUDE_CONFIG_DIR` pointing to its selected native home.
The runtime restores the workspace/native pair while inert and generates fresh
native configuration before activation. The command passes the approved
instructions explicitly to the selected client. Codex resume retains its original
developer instruction block, which destination admission checks against the
approved digest. Claude receives the same approved instructions through fresh
`--append-system-prompt`; its transcript cannot prove which instructions were
approved. Neither native history nor this command selects placement authority.

Model names are limited to 128 bytes and cannot contain NUL or line breaks.
Codex options are limited to 32 entries of 4096 bytes each, with single-line
values. Keys use dotted bare ASCII segments or double-quoted segments, supporting
project paths and named MCP/provider settings. Whitespace outside quoted keys is
rejected. Normalized top-level `developer_instructions` and
`check_for_update_on_startup` remain reserved even when quoted or escaped.
Sproozi validates this restricted key boundary; the selected client interprets the
administrator-selected TOML values.
Claude rejects these options and receives fixed foreground Bash tools, empty MCP
configuration, explicit fresh completion hooks and `bypassPermissions`. User and
project settings sources are excluded. Fresh onboarding/preferences and hook
settings are not selected by the transcript checkpoint. Background tasks and
prompt suggestions are disabled. Codex uses `--sandbox danger-full-access
--ask-for-approval never` because its inner sandbox failed in the restricted
worker kernel. Both clients rely on independently enforced outer container
confinement; see [ADR 0011](../../docs/adr/0011-native-session-compatibility-and-completion.md).

Activation uses the installed executable. Sproozi checks native state structure,
conversation identity and completion without an exact-version allowlist.

Docker starts an inert bootstrap supervisor before the gateway allocation is
initialized:

```sh
/usr/local/bin/native-session serve --bootstrap --deadline <original-RFC3339Nano-deadline>
/usr/local/bin/native-session configure
```

`configure` accepts the same bounded launch JSON on stdin exactly once, with a
deadline equal to the original bootstrap deadline. An identical retry succeeds;
a changed contract fails. Kubernetes continues using the read-only mounted
contract. Local public CA certificates go to `/tmp/sproozi-native/ca.pem`, and
projected gateway tokens are read at activation from
`/var/run/sproozi/tokens/gateway/token`. Neither enters the portable state pair.

The server creates `/tmp/sproozi-native/control.sock` under a new 0700 directory.
This path is in the agent's own tmpfs, never a volume shared with the trusted
capture helper. The native workload can access this socket as the same UID.
Its replies are diagnostics, not authority, activation receipts or writer
fencing. Gateway admission/active registration and actual container termination
remain server-owned controls. Worker tampering can make an execution unavailable;
it cannot justify source capture or destination activation.

Trusted Docker/Kubernetes exec runs only these fixed commands:

```sh
/usr/local/bin/native-session activate
/usr/local/bin/native-session status
/usr/local/bin/native-session gate
/usr/local/bin/native-session attach
/usr/local/bin/native-session quiesce
```

`activate` starts the fixed native process once. Retrying while active returns
its PID. The gate cannot reopen after quiescing. `status` returns the diagnostic
state and selected UUID when known. `gate` immediately blocks further native
input without interrupting the current turn. Control operations cannot replace
launch commands, paths or environment after configuration. Deadline expiry also
stops an execution that is still inert.
Claude `status` also returns bounded `completion` diagnostics: lifecycle flags,
pending tool count, prompt/current input generations, last accepted hook event,
fixed rejection category and selected-journal observation category. These fields
contain no prompts, tool arguments or journal bytes and do not authorize a move.
The fixed `observe-claude` command consumes a bounded native hook payload and is
installed in the trusted fresh hook settings; successful hooks emit no unrelated
stdout. It is not a developer-facing lifecycle operation.

`attach` forwards newline-delimited JSON on stdin/stdout. It emits an initial
`attach` response, then `output` messages with base64 `data`, monotonic `cursor`
and a replay `gap` flag. Input permits only `write` with base64 `data` and
`resize` with positive `rows` and `columns`. Each input receives an acknowledgement
with `written` or `error`. Writes are limited to 4096 decoded bytes. A disconnect
releases the sole attachment lease while the native process and bounded output
replay survive. Reconnect replays the retained tail; it may include prior output.
The caller implements authenticated transport, terminal probe handling and
backpressure. A stalled attachment cannot block PTY output draining.

`quiesce` blocks new input immediately. For Codex, it polls the selected native
journal for a completed final turn, matching initial approved instructions and
resolved function/custom-tool and native exec/MCP/patch activity. Partial rows,
unknown tool calls, unresolved calls, incompatible metadata or instruction
mismatch fail closed. The first new session requires exactly one native journal.
For Claude, it requires a matching native `Stop`, no pending foreground Bash
tools, and a settled selected primary transcript. `UserPromptSubmit` clears
earlier completion; accepted attachment writes after that prompt's generation
prevent reuse of its Stop. The parser checks the exact UUID, primary
chain and supported tool pairs. The observed bounded recovered 429 record clears
the previous assistant leaf and needs a later successful assistant response.
Other native error shapes, subagents, compaction and background/parallel work are
unsupported. A displayed answer or provider response alone does not satisfy the
gate. Draft input and late terminal replies can conservatively prevent reuse of
an earlier completion observation.

These are read-only turn observations, not checkpoint capture. A journal is
worker-accessible, and observing it does not prove every background process or
side effect has finished. The runtime must validate captured native state again
and independently stop/inspect the Docker container or observe the exact
Kubernetes agent-container termination before capture. PTY process-group stop
does not include tools that escape into other groups or sessions.

Once Codex's observed turn completes, the supervisor sends native `/quit` and
requires normal exit. Claude instead uses controlled process-group shutdown
after its completed-turn gate; the `/quit` input was not a reliable native
shutdown operation. The supervisor returns the selected UUID/turn and exit,
then exits PID 1. Forced deadline/signal stops do not establish graceful completion.
The placement persists its accepted quiescence acknowledgement before revocation;
the supervisor itself does not store a durable completion receipt. A lost Claude
acknowledgement cannot be replaced by a terminated container or a settled
transcript: that graceful move remains unconfirmed and can require cancellation.
Never restart or relaunch a container to infer successful quiescence. Pending
input admitted just before gating can race native journal emission. Final
post-stop native checkpoint validation must reject an incomplete/aborted last
turn; the earlier turn observation is insufficient by itself. Whole-container
fencing and sealed usage remain the placement's responsibility.

The stock TUI fixture is separate from container containment:

```sh
go build -o /tmp/sproozi-native-pty ./hack/verify/native-pty
python3 hack/verify/native-tui.py --helper /tmp/sproozi-native-pty --report .local/verification/native-tui/evidence.json
```

It exercises production Go PTY attachment, detach/reconnect to the same PID,
input rejection during a held actual provider turn, native completed-turn quit
and production state capture/restore to a fresh home with the same UUID/history.
It uses a credential-free loopback provider and tests text turns. It does not
prove Docker/Kubernetes fencing, tool side effects or remote admission. Raw
requests/history remain in a temporary private evidence directory; the report
contains outcomes, census and source/binary digests.

Both selected clients also passed the separate production Docker/Kind round-trip
fixture. Codex's extended proof covers real tool effects, held moves, forced
cancellation and denials; Claude's round trip covers genuine TUI turns,
foreground Bash, Git/ignored-file continuity and sealed Messages usage. See the
[repeatable privileged setup](../../hack/verify/session-mobility/README.md).
