# Native host and release requirements

The [target inventory](../../hack/release/targets.json) defines the CLI archives.
Each target must pass native host tests and execute its extracted archive before
release publication. Keep cross-compilation results separate from execution.

## Hosted checks

The Native platforms workflow runs the changed host packages on Linux, macOS
and Windows. Windows tests must exercise:

- Owner and DACL checks, broad/null DACL rejection, reparse rejection and private
  creation. Record privilege-dependent wrong-owner skips separately.
- A second process contending for a held lock, then reacquisition after release
  or process termination. A held Windows lock must not permit replacement.
- Repeated journal replacement and reopen with complete bounded records.
- Real local control exchanges, restart, cancellation, terminal frames and
  duplicate listeners. Check server identity before sending application bytes.
  Record whether the two-user squatting test actually ran.
- Git capture and restore, executable index modes, reserved filenames, case
  aliases, junctions, ignored-file selection and isolated Git configuration.
- Actual subprocess Docker arguments and environment, fixed local endpoint,
  Linux-engine rejection and no Docker launch during home composition.
- Installed help/version and private configuration-directory creation.

The Releases workflow verifies the archive inventory and SHA-256 hashes, then
runs each extracted executable. The Homebrew tap must build the source bundle
with network access disabled for Go, run its formula tests, and complete a fresh
macOS and Linux installation.

## Windows Docker Desktop and console

Hosted file and pipe tests do not prove a complete Docker Desktop session. On
an actual Windows x86-64 host with Docker Desktop using Linux containers:

1. Record the release version, commit, Windows build, Docker version and agent
   version. Create a new private home with `sproozi init` and use an isolated
   repository, network and session configuration.
2. Start a managed session through the configured local pipe. Check worker UID,
   limits, dropped capabilities, network restrictions and workspace contents.
3. Attach from Windows Terminal. Send input, resize the terminal, detach with
   `Ctrl-]`, reconnect and confirm conversation continuity.
4. Stop and resume retained work. Restart the home during a journaled operation,
   run recovery, and verify that no duplicate execution or allowance appears.
5. Capture a tracked executable script and unfinished Git changes. Move or
   restore them into a Linux worker and verify bytes, index mode and status.
6. Clean up owned sessions, containers, volumes and the test network. Record
   what ran and any failures in a dated report.

Keep this live acceptance row incomplete until performed. An unsigned Windows
archive can be published with its explicit support limits; it must not be
described as a completed Docker Desktop or interactive-console acceptance run.
