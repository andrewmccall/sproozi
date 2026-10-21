# Derived pull operations and work owner names

Status: accepted
Date: 2026-10-17

## Problem and constraints

`sproozi pull` transfers ownership of a stopped remote session to home. The
remote owner records the first transfer operation UUID and accepts only that
operation for the export and claim. The CLI generated a fresh UUID on every
invocation. A pull interrupted after the export was recorded could therefore
only be finished by finding the printed UUID and passing `--operation`; a plain
retry failed with an unexplained conflict.

The same configured name also resolved to a remote owner three different ways.
The CLI preferred a `workSources` entry per name and fell back to `remote`. The
daemon used `workSources` only when that map was non-empty, otherwise `remote`.
Project approvals accepted a name from either map. With `workSources` holding
`a` and `remote` holding `a` and `b`, the CLI could query `b` while the daemon
could not pull from it.

## Decision

Without `--operation`, the CLI derives the pull operation as a name-based UUID
of the session ID and source name. Repeating the same command repeats the same
pull, so an interrupted pull completes on a plain retry. An explicit
`--operation` still wins. When home retains a pull with a different source or
operation, the error names the retained source and operation and the flags to
retry it.

`home.Config.WorkConnections` resolves every remote work owner name for run,
discovery, remote control, pull and project approvals. A `workSources` entry
wins; any other name uses its `remote` profile. This matches the per-name rule
the guide already described and the CLI already used.

## Alternatives

- Keep random operations and tell users to copy the printed UUID. This keeps
  the existing behaviour but makes the common recovery path depend on scrollback.
- Have the CLI ask home for a retained pull operation before choosing one. This
  converges only after home saved its catalogue record, not in the window after
  the source recorded the export, and needs a status field home does not expose.
- Derive the operation in the daemon. This needs a protocol change to let the
  request omit its operation, and the CLI could no longer print it beforehand.
- Use `workSources` only when non-empty. This is the daemon's former rule; it
  silently hides `remote` names the CLI and approvals accept.

## Tradeoffs and consequences

Two homes holding the same principal credential would derive the same pull
operation for a session and source, so the source could acknowledge both.
Copying a home's principal credential into independent homes is already
unsupported; the transfer fence assumes one home per principal.

A `remote` profile now also serves as a work source whenever `workSources` is
present but lacks that name. Both maps already require the same checkpoint byte
limit.

## Revisit when

- Several homes legitimately share one owner principal.
- Pull needs more intent than session and source, such as a selected checkpoint.
- `remote` and `workSources` gain different trust or transport requirements.

## Subsequent decision

[ADR 0036](0036-remote-executors-through-kubeconfig-contexts.md) names a remote
executor by the placement name it describes, reached through a kubeconfig
context, in place of `remote` and `workSources` entries. Once the CLI uses it,
the per-name lookup above has a single source.

## Links

- [Retained remote work](../guides/portable-workflows.md)
- [Session commands](../reference/session-commands.md)
- `cmd/sproozi/parse.go`, `internal/session/home/config.go`,
  `internal/session/home/catalog.go`
