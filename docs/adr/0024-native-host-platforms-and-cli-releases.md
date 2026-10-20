# Native host platforms and CLI releases

Date: 2026-10-18

Status: accepted; the client-version statement below is partly replaced by [ADR 0029](0029-local-provisioning-and-lifecycle.md)

## Context

The CLI also runs the trusted home service. Shipping a Windows executable
requires the same private credentials, exclusive state ownership, journal
replacement and local administrative access as the Unix host. Removing Unix
checks to permit cross-compilation would change those trust boundaries.

Users also need a released executable and Homebrew installation. CLI packaging
is separate from installing Kubernetes resources, Linux worker images, network
policy and credentials.

## Decision

Share host-file policy in `internal/privatefs`. Domain stores retain their
schemas, bounds, mutexes and lock lifetimes. Every lock, read and replacement
operation validates its own boundary. All existing home, accounting, placement,
grant and work stores use the shared policy.

Unix keeps ownership and mode checks, `flock`, flushed files, rename and parent
directory sync. Windows requires current-user ownership, a non-null DACL that
grants access only to the user and privileged system accounts, and ordinary
files on a local NTFS drive. New private directories receive a protected DACL
at creation. Reparse points and network shares fail validation. Windows uses
retained nonblocking `LockFileEx` locks and flushed same-directory replacement
with `FileRenameInfoEx` and POSIX replacement semantics. Existing readers retain
the old file; new readers open the replacement. Lock handles forbid delete
sharing and cannot be replaced.
These operations do not establish an independent hardware power-loss guarantee.
The [Windows rename contract](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information)
defines how open readers retain their file after replacement.

Local control keeps the shared request and terminal protocol. Windows uses a
named pipe with an explicit current-user and SYSTEM DACL, remote-client
rejection and first-instance protection. The client checks the connected server
process's user before sending a request. This retains same-user access across
logons, matching Unix ownership. Local administrators remain privileged.

Docker commands and terminal attach share one fixed endpoint and clean process
environment. Windows accepts only local Docker pipes. The service requires a
Linux engine before mutating runtime resources. Workers keep their Linux
supervisor, network controls and container permissions. Host workspace capture
rejects reparse points and Windows filename aliases. It preserves executable
metadata from the Git index because Windows file modes cannot express it.

Product tags publish CLI archives and SHA-256 checksums. Native runner tests and
extracted-archive execution gate publication. The source bundle includes Go
dependencies so Homebrew can build without network access during installation.
The public tap owns its formula and updater. It reads stable releases and
publishes tested formula updates with its own workflow token. Sproozi does not
hold a cross-repository tap credential.

Agent packages continue to use upstream defaults. Product versions do not pin
agent clients. A resolved runtime image identity remains part of execution
identity, as required by the existing placement design.

## Alternatives and tradeoffs

- Separate Windows copies of each store would keep existing files smaller but
  duplicate security policy across owners. Shared functions remove that
  duplication while leaving domain ownership in place.
- A directory object could combine validation and journal access. The current
  owners already hold their lifetime locks and nested paths. Complete boundary
  functions avoid introducing a second ownership model.
- Loopback TCP would need a separate authentication credential and handshake.
  Named pipes provide local access control and peer identity without another
  secret file.
- Restricting pipes to one logon would prevent the same account from controlling
  its home from another session. Current-user access preserves existing
  semantics with less impersonation machinery.
- A binary Homebrew cask would install faster, but normal macOS distribution
  would need signing and notarization. A source formula costs build time and
  a Go dependency. It does not disable quarantine checks.
- Direct tap publication would update immediately but require another token or
  GitHub App. Tap-owned polling accepts up to an hour's delay. Manual dispatch
  provides an immediate update without broadening token authority.

## Verification and revisit conditions

Native tests must cover Windows ACL rejection, cross-process lock contention,
journal replacement and reopen, private control and peer checks, workspace
round trips, and the fixed Docker command environment. Cross-compilation alone
does not establish support. Real Docker Desktop and interactive-console
acceptance remain distinct from hosted host-boundary tests.

Revisit the storage backend if NTFS recovery checks fail the durable-intent
contract or another filesystem becomes a supported requirement. Revisit the
pipe policy if same-account access across logons becomes unacceptable. Revisit
source Homebrew builds when signed and notarized binaries justify a cask or
maintained bottles.

See [installation](../guides/install.md), [Windows setup](../guides/windows.md),
[release steps](../contributing/releases.md), and
[testing requirements](../../test/acceptance/README.md).
