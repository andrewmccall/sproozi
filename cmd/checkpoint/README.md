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
and `bytes`; it describes continuity, not authority or sealed usage.

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

The helper persists an exact restore acknowledgement in its private directory.
An identical retry returns the same metadata; a conflicting artifact or missing
receipt after publication fails closed. It does not silently adopt an existing
worker-supplied target. For an initial local conversation, stdin instead contains
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

Once the placement has retained the artifact and sealed accounting receipt:

```sh
/usr/local/bin/checkpoint finish
```

`finish` acknowledges termination to the helper's private PID 1. It supplies no
proof that the agent stopped and must not precede capture/receipt retention.

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
