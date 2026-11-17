# Selected native checkpoint helper

`checkpoint` transfers one stopped harness conversation and its Git workspace as
a bounded artifact. Build it into the trusted helper image at
`/usr/local/bin/checkpoint`. The placement owns admission, transport authentication,
quota-backed storage, actual writer fencing and activation. This command does
not infer a fence from a native journal, hook or process exit.

New artifacts use version 2 with explicit `harness: codex` or `claude-code`,
exactly one matching native component, and the workspace component. Client versions do not determine compatibility. Version 1 Codex artifacts have a separate
explicit decoder; they do not create a default client for new launches. A move
cannot switch kind. Metadata contains `harness`, `threadId`, `turnId`, `sha256`
and `bytes`; it describes continuity, not authority or finalized usage.

## Managed helper lifecycle

The trusted runtime starts this command as the helper container's PID 1:

```sh
/usr/local/bin/checkpoint hold --deadline ORIGINAL_RFC3339NANO_DEADLINE
```

It retains the bounded workspace mount after the agent stops. The helper's
`/tmp/sproozi-checkpoint` directory, control socket and restore receipt are private
to that container, not shared with the native worker. The original deadline also
ends the helper. The placement supplies a managed checkpoint on stdin while the
agent remains inert:

```sh
/usr/local/bin/checkpoint prepare --harness codex \
  --instruction-digest APPROVED_SHA256 --max-bytes 8388608 < checkpoint.json
```

`prepare` validates the selected kind, native continuity and independently
supplied instruction digest before publishing the pair under `/workspace/session`.
Codex validates its actual initial instruction block; Claude has no equivalent
instruction evidence in its transcript. The trusted launch/profile must retain
and supply Claude's approved instructions independently. The resulting pair is
`workspace` plus `codex` or `claude`. Publication happens only after both staged
components restore successfully.

The helper persists an exact restore receipt, `restore.json`, in its private
directory. Each write replaces it atomically (temporary file, `fsync`, rename).
It holds the artifact metadata and a `stage`: `restoring` is written before the
pair is published and `restored` once the published pair is durable, so an
interrupted restore always leaves a receipt naming how far it got. An identical
retry of a `restored` receipt returns the same metadata; a retry of a
`restoring` one removes whatever the interrupted restore left at the target and
restores again, which is safe because the agent stays inert until `prepare`
succeeds. A conflicting artifact fails closed, and a target with no receipt is
never adopted or removed. `capture-stopped` refuses to run without a `restored`
receipt, and `finish` refuses while the receipt is `restoring`; repeating
`prepare` completes that restore. For an initial local conversation, stdin instead contains
the captured workspace artifact:

```sh
/usr/local/bin/checkpoint prepare-workspace --harness claude-code \
  --max-bytes 8388608 < workspace.json
```

This publishes the workspace with a fresh empty selected native home. Its receipt
has an empty harness conversation ID (`threadId`); the stock client generates the UUID later. It accepts
no instruction digest or caller-supplied thread ID.

After the placement independently confirms the exact worker container stopped,
capture the selected native/workspace pair through the surviving helper:

```sh
/usr/local/bin/checkpoint capture-stopped --harness claude-code \
  --home /workspace/session/claude --workspace /workspace/session/workspace \
  --thread NATIVE_UUID --instruction-digest APPROVED_SHA256 \
  --include-ignored needed.bin --max-bytes 8388608 > checkpoint.json
```

For Codex select `--harness codex --home /workspace/session/codex`. The command
writes the artifact to stdout. Whole native homes, Git configuration/hooks, credentials
and launch preferences are excluded; only the selected rollout/transcript and
workspace state travel. `--include-ignored` repeats for explicitly selected
individual ignored files. The placement retains that immutable selection through
all visits and seals. Missing selected files or unsafe paths fail capture.

### Capture results

`capture-stopped` says why it failed with its exit code, defined in
`internal/checkpoint` (`exit_codes.go`), which the placements import
([ADR 0062](../../docs/adr/0062-one-exit-rule-and-exits-over-the-wire.md)):

| Code | Meaning |
| --- | --- |
| 0 | The artifact is on stdout. |
| 65 | The stopped state was refused: it is a property of the fenced bytes, so capturing again fails the same way. The conversation file has no completed turn, names another conversation, is not a plain file, is absent, or cannot be read because of its own permissions; a native directory fails the safety checks; the state exceeds `--max-bytes`; or the continuity check against `--instruction-digest` fails. |
| 74 | The capture could not be carried out: an I/O error from the volume, a Git failure, anything else. Repeating it may succeed. |

Every other command exits 1 on any failure, as every command of a helper
older than these codes does. The reason is on stderr either way, after the
line prefix `Checkpoint operation failed:`, which every release writes
(`checkpoint.HelperFailurePrefix`).

Once the placement has retained the artifact and finalized usage receipt:

```sh
/usr/local/bin/checkpoint finish
```

`finish` acknowledges termination to the helper's private PID 1. It supplies no
proof that the agent stopped and must not precede capture/receipt retention.

The helper's exit ends its container, which can kill the `finish` command
before its exit status reaches the placement. So after it replies, the helper
waits for `finish` to read the reply and close, at most 10 seconds, and then
exits 0. It exits 0 only after a finish: its deadline, `SIGTERM` and every
error exit 1. `finish` keeps its connection open for writing until it has
read the reply; the helper half-closes after replying, which ends that read.
A Kubernetes stop whose `finish` exec fails anyway is complete when the Pod
shows this container terminated with code 0 within 15 seconds
([ADR 0068](../../docs/adr/0068-a-lost-finish-is-proven-by-the-helpers-end.md)).

## Inert restore and limits

For a separate controlled inspection, restore a checkpoint into an absent
destination owned by the caller:

```sh
checkpoint restore --destination /absolute/private/absent-session \
  --max-bytes 8388608 < checkpoint.json
```

`restore` selects the explicit artifact kind, including the recognized version 1
Codex format. It accepts no `--harness`, source home, thread, instruction digest
or ignored-file selection. This is inert restoration, not managed admission;
it does not start a native process or reconstruct credentials. Claude is placed
under the destination's project path while historical transcript rows remain
unchanged.

Operations default to 64 MiB and one minute; explicit bounds allow at most 1 GiB
and ten minutes. Managed callers supply their smaller approved checkpoint limit.
Encoded-byte limits alone do not bound Git extraction: the caller also needs
quota-backed staging and process/resource limits. Native parsing stays within
the supported state subset, including supported foreground tools and the
observed recovered Claude 429 shape followed by a new successful assistant leaf.
Claude transcript inspection does not establish its live Stop acknowledgement.
An unresolved retry cannot reuse earlier completion. Background tasks, subagents,
compaction and broad long-history compatibility remain unsupported or unassessed.

See [ADR 0008](../../docs/adr/0008-selected-native-and-git-checkpoints.md),
[ADR 0011](../../docs/adr/0011-native-session-compatibility-and-completion.md) and the
[production mobility fixture](../../hack/verify/session-mobility/README.md) for
the selected-state decisions and genuine Codex/Claude round-trip verification.
