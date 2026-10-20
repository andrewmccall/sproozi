# 0025: Local provisioning, per-harness placements and home-owned certificates

Status: accepted

Date: 2026-10-20

## Problem and constraints

The [simplified local onboarding proposal](../explanations/future-architecture/simplified-local-client-onboarding.md)
asks an installed CLI to prepare a usable local environment from a running
Docker engine. Before this change a user built three images, resolved Docker
content IDs, created a network, chose a host address, generated certificates
with OpenSSL and wrote the home configuration by hand.

The existing home already enforced the important boundaries: fixed image IDs,
a network anchor that permits one gateway socket, a gateway that holds provider
credentials, journals that reject changed placement configuration, and
selected-state checkpoints. Provisioning had to produce those inputs without
weakening them. The home also served exactly one harness: one placement, one
gateway listener, one provider credential and one inspection certificate.

## Decision

### Provisioning is a client-side journal, not a new service

`sproozi init --local` runs in the CLI. Each step records its result in
`installation.json` under one root lock, so a repeated or interrupted run
converges: Docker endpoint, image, owned network, probed gateway endpoint,
harnesses and credentials, certificate authority, configuration, service and
readiness. The output is the existing home configuration, which the home
service reads unchanged. Setup never overwrites an edited configuration. It
adds missing harnesses and refuses when placement-binding values differ.

The default root is `~/sproozi`. An existing platform configuration stays the
session commands' default, so manual installations keep working.

### The gateway address is measured, not assumed

Setup serves a nonce on a candidate host bind and fetches it from a container
on the installation's network. Candidates run from narrowest to widest:
loopback reached through the engine's host name, then the bridge gateway
address, then all interfaces. On Docker Desktop a live check showed the host
name forwards to host loopback, so the gateway binds only `127.0.0.1`. The home
accepts a loopback listener for this reason.

Readiness requires the real restricted path: a network anchor installs its
firewall, a worker joined to its namespace verifies the gateway's TLS identity,
then attempts forbidden paths. A probe without the anchor reaches those paths,
which shows the check discriminates. `sproozi-network probe` holds these checks,
and the image reports a helper contract version that setup requires.

### One local placement per harness

Each configured harness is a complete local stack: its own placement, grants
owner, gateway listener and provider credential. The stacks share the journal,
accounting ledger, images, network and certificate source. The first harness
keeps the original `local` placement and directories, so existing records stay
valid. `start --harness` chooses a stack per session. A move to any local
placement name resolves to the session's own harness.

A worker's anchor permits only its own placement's gateway port, so a Codex
worker cannot reach the Anthropic route or credential. Each gateway also
routes only its harness's model authority.

### The home owns its certificate authority

`internal/pki` creates ECDSA P-256 authorities and leaves with the operating
system's random source. Its store keeps CA generations, the trusted set, the
serving leaf and a rotation stage in one private directory. `state.json` is the
atomic commit point: new key and certificate files are written before the state
that names them. With `gateway.pkiDirectory`, the home daemon:

- renews the leaf after two thirds of its lifetime, checking every minute;
- serves it through an atomically replaced certificate, so new connections get
  the new leaf without a restart and existing connections keep theirs;
- records each allocation's trust generations before the bundle leaves the
  service, releases the record after a sealed stop or independent fence, and
  switches or retires a rotation only when no recorded consumer lacks the new
  generation;
- never creates an authority at startup. A missing key stops issuance rather
  than producing an unrelated authority.

Emergency replacement is an explicit command that destroys every older key.

### Images come from the release, with explicit alternatives

One native image serves the worker, checkpoint-helper and anchor roles; the
placement fixes each role's command and privileges. A release builds it for
amd64 and arm64 on native runners from the existing pinned recipes, publishes it
by digest and embeds the digest in the CLI. Setup pulls that digest with a
private Docker configuration and records the content ID. `--image` accepts an
administrator override and `--build` runs the same recipes locally. Setup never
substitutes another source silently.

The published native image installs the client versions locked in
`hack/clients` at the release commit, and `images.json` records them. This
replaces [ADR 0024](0024-native-host-platforms-and-cli-releases.md)'s statement
that product versions do not pin agent clients, for that image only. Kubernetes
runtimes and the stock examples can still select other images, and the weekly
latest-client canary still tests newer releases.

### The home daemon is a detached process

`sproozi service start` runs `sproozi serve` in its own session with a private
log and PID file. Readiness is an answered control request; the gateway listens
before the control socket. Stopping the service refuses while sessions are
active unless the user explicitly leaves them running. Session commands start
an absent service for a provisioned installation.

### Shared agent files are a launch-time snapshot

The agents directory holds allowlisted instruction files. The trusted home
reads them on every launch, rejecting links, special files, foreign or
writable-by-others files, oversized and non-UTF-8 text. The launch contract
delivers the text, and the supervisor writes it as the native user instruction
file, separate from the approved developer instructions. Nothing is mounted;
the agent's edits are not written back; checkpoints still select only the
conversation and workspace.

Codex loads `$CODEX_HOME/AGENTS.md`, and its checkpoints still pass continuity
validation. Claude Code, launched with every settings source disabled, ignores
its user memory file. Enabling the user source loaded the file, but Claude then
requested remote managed settings and its graceful stop never completed. Claude
therefore receives no shared files in this release.

## Alternatives

- **A long-running provisioning service.** It would centralise repair, but adds
  another owner and lock. The journal gives the same convergence from the CLI.
- **Assuming platform host addresses** such as `192.168.65.254`. It fails on
  engines with other addresses and cannot prove reachability. Measuring costs
  one container per candidate.
- **One placement serving both harnesses per launch.** It would change the
  placement fingerprint, invalidating retained records, and let one worker
  reach both providers' credentials through a merged route.
- **OpenSSL or a bundled PKI tool.** It adds a host dependency the proposal
  excludes. The Go standard library covers the needed certificates.
- **Time-based rotation overlap.** Simpler, but time alone does not prove a
  consumer has the new trust. Recorded consumers plus independent stop
  evidence do, and a stuck consumer blocks rotation visibly.
- **A writable bind mount of the agents directory.** It matches the proposal's
  wording that agents may alter shared state. It needs a new mount contract in
  an exact-mount verifier, writes untrusted output onto the host, risks new
  transcript records that break capture, and widens configuration loading.
- **Appending shared text to Claude's system prompt.** It would deliver the
  text, but at the same precedence as approved instructions.
- **Platform service managers (launchd, systemd user units).** They add boot
  persistence. They are deferred; the detached service meets the current
  requirement, and session commands restart it on demand.

## Tradeoffs and consequences

- Changing the image, network, gateway address or approved instructions of an
  installation with retained sessions is refused rather than migrated.
- Several local installations on one computer contend for the default port;
  the error names the port.
- Rotation can wait indefinitely for a consumer that never stops; `certificates
  status` names it. Cancelling the session releases it.
- Agents cannot change shared instructions, and Claude Code users get none.
- Docker Engine on Linux uses the bridge-address path, which has unit coverage
  but no recorded acceptance run.
- Automatic setup does not run on Windows hosts.

## Revisit conditions

- Users need agents to persist shared preferences or history: design a mount
  contract, concurrency rules and transcript validation for it.
- Claude Code offers user memory without enabling other settings sources, or
  a supported way to add user instructions below the system prompt.
- A supported platform needs boot persistence or a native service manager.
- Image upgrades must apply to installations with retained sessions: add a
  drained migration.
- A Linux or Windows acceptance run fails the endpoint or containment probes.

## Links

- [Local sessions guide](../guides/local-sessions.md) and
  [local installation reference](../reference/local-installation.md)
- [Home configuration](../../cmd/sproozi/README.md)
- `internal/localsetup`, `internal/pki`, `internal/session/home`,
  `cmd/sproozi-network/probe.go`
- [ADR 0011](0011-native-session-compatibility-and-completion.md) for the
  native launch boundaries this keeps
- [ADR 0026](0026-kubernetes-release-assets-and-certificates.md) for the
  Kubernetes counterpart
