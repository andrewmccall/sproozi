# Move a native session between local and remote execution

Use this guide to move a managed Codex or Claude Code session between local
Docker and an approved Kubernetes environment. You use the agent's native
terminal. The conversation ID and supported Git workspace state stay the same.

A move keeps the execution's original deadline and total usage allowance. Each
destination gets fresh identity and access. For a completed remote task that you
want to resume later, use [remote work and local resume](portable-workflows.md).

This setup uses the Codex and Claude Code releases pinned in
[`hack/clients`](../../hack/clients/README.md). A session
keeps one selected client throughout its lifetime. Start a new managed
conversation; importing a personal agent home or adopting an existing running
session is not supported.

## Prepare the administrator-owned setup

The trusted home daemon owns a local Unix Docker socket, fixed image IDs, its TLS
gateway, provider credentials and protected journals. Local start requires no
cluster or kubeconfig. A developer can use the profile service after an
administrator provisions it. The remote service holds its own Kubernetes
credentials; the developer receives only the profile URL, public CA and protected
principal token file.

You need a running Linux Docker engine or Docker Desktop, Git, the repository's
Go toolchain, and administrator-created images/network/TLS material. Keep all
service configuration and credentials outside the repository being transferred.
Portable state contains the selected conversation and workspace, including any
ordinary untracked files. It excludes home credentials, gateway tokens, provider
keys, Kubernetes credentials, private TLS keys and Git hooks/config. Protect the
workspace contents you choose to transfer; an untracked credential file would be
workspace data, so move it outside the source before capture.

Build the two host executables from the repository root:

```sh
go build -o bin/sproozi ./cmd/sproozi
go build -o bin/session-profile ./cmd/session-profile
```

Build the stock-client image with
[`hack/demo/codex.Dockerfile`](../../hack/demo/codex.Dockerfile), from the
repository root. It installs the Codex release pinned in
[`hack/clients`](../../hack/clients/README.md). Add Claude Code with the
[combined client image](../../internal/runtime/docker/image/clients.Dockerfile),
which installs the pinned Claude Code release the same way.
Then build the
[native session image](../../internal/runtime/docker/image/Dockerfile) with an
immutable `CLIENTS_IMAGE=repository@sha256:...` reference. That image adds the
fixed native supervisor, checkpoint helper and network anchor executable.
Use your registry's approved architecture-specific or multi-architecture build
process; the [Kubernetes installation guide](install-kubernetes.md#2-build-and-publish-the-images)
shows registry publication and digest selection.

Pull the approved native image onto the local engine and resolve its Docker
content ID. The same approved image can provide the separate native, helper and
anchor containers, with commands and privileges fixed by each role:

```sh
export SPROOZI_NATIVE_IMAGE=YOUR-REGISTRY/sproozi-native@sha256:APPROVED_DIGEST
docker pull "$SPROOZI_NATIVE_IMAGE"
docker image inspect --format '{{.Id}}' "$SPROOZI_NATIVE_IMAGE"
docker network create sproozi-sessions
```

Put the returned `sha256:...` content ID in the local configuration's three image
fields. The local adapter does not pull images or create its bridge network.
The remote runtime uses the canonical registry digest and must make the image
available to cluster nodes separately.

The home gateway's fixed IPv4 and port must be reachable from the chosen Docker
network. Select the actual host/VM address for your platform; `192.168.65.254` in
the example is specific to its Docker Desktop setup. It is not a universal host address.
The trusted network anchor permits worker traffic only to that fixed gateway
endpoint, with IPv4/IPv6 restrictions. The anchor has NET_ADMIN for its fixed
firewall; the native worker has no capabilities or host/Docker socket mounts.
Choose cluster Pod and CNI address pools that do not overlap that host address
or your Docker networks. For example, Calico's default `192.168.0.0/16`
contains `192.168.65.254`. If you use that host address, choose a different Pod
subnet and CNI address pool.

Create a private owned configuration directory with `umask 077`, mode `0700` for
directories and `0600` for regular files. Configure the local TLS certificate to
cover the fixed gateway IPv4 as an IP SAN and the selected model authority as a DNS SAN, `api.openai.com` for Codex or
`api.anthropic.com` for Claude Code. Its
CA chain must verify through the selected public trust bundle. The provider
credential stays in `gateway.providerTokenFile`; the worker receives a fresh
allocation token and a placeholder provider key. Save credentials directly into
protected files using your credential manager, rather than pasting them into
chat or writing them into the source workspace.

Use the [home configuration example](../../cmd/sproozi/README.md) and
[configuration schema](../../internal/session/home/config.go). Set the exact
approved instruction bytes and their SHA-256, model/options, finite positive
unit allowance, explicit cost allowance, duration and resource/storage limits.
`bounded:false,value:0` means unbounded cost. `bounded:true,value:0` means
exhausted allowance and cannot activate another visit. Bounded model cost needs
administrator pricing in `gateway.pricingFile`.

For a local-only session, omit the `remote` object. For remote moves, configure
its fixed HTTPS origin, principal token file, CA file and checkpoint byte limit.
The remote placement alias must match the profile's `placementName`. All
placements must use the same checkpoint byte limit and approve the same initial
instructions and native version. The original home duration must also fit the
profile service and live policy; Kubernetes policy currently permits at most one
hour. Moving does not restart the lifetime clock.

## Provision the profile service

Follow [the profile setup](../../cmd/session-profile/README.md) to install the
current controller/shared gateway and generated CRDs in an evaluation cluster
with a NetworkPolicy-enforcing CNI. The administrator supplies the managed native
runtime, approved template/policy, immutable certificate-only worker trust bundle,
shared gateway TLS and gateway-owned provider credentials.

The profile service uses either its protected host kubeconfig or an in-cluster
ServiceAccount with the documented scoped Run/registration/account/Pod permissions.
It fixes the principal, Run namespace, template, ceilings and maximum duration in
private JSON. Incoming move requests cannot select images, environment variables,
Kubernetes credentials or arbitrary commands. One process owns its private durable
state directory. Run it under a service manager and retain the directory across
restarts.

The private principal token needs at least 32 non-whitespace characters. TLS 1.3
verifies the profile certificate against the configured CA. The profile client
sends the token only to that exact origin, with no redirect or environment-proxy
fallback. Keep the token and CA file paths in the home configuration; do not copy
the profile's kubeconfig to the developer, worker or portable pair.

## Start the daemon and conversation

Keep the daemon process independent of terminal attachments. For a local trial:

```sh
umask 077
mkdir -p /absolute/private/home
chmod 700 /absolute/private/home
nohup bin/sproozi serve --config /absolute/private/session.json \
  </dev/null >/absolute/private/home/daemon.log 2>&1 &
```

Use an ordinary Git repository with a committed HEAD. Stop writers to that source
before capture. The checkpoint retains Git history and branch references,
staged/unstaged changes, deletions, ordinary untracked contents and executable
modes. The source directory remains untouched; all subsequent native work occurs
in the managed Docker workspace.

Start and retain the printed session and operation UUIDs:

```sh
bin/sproozi start --socket /absolute/private/home/control.sock \
  --workspace /absolute/path/to/stopped-repository
```

Ignored files are excluded by default. Start applies the same rules as your Git:
`.gitignore` files, `.git/info/exclude` and the effective `core.excludesFile`
(from the repository config, then your global config, then
`~/.config/git/ignore`). It reads that value without following config includes.
To carry an individual ignored input, use repeatable flags at start:

```sh
bin/sproozi start --socket /absolute/private/home/control.sock \
  --workspace /absolute/path/to/stopped-repository \
  --include-ignored needed.bin --include-ignored cache/chosen.dat
```

The ignored-file selection cannot change after start. It remains recorded for
every visit and usage receipt. It accepts at most 32 clean relative file paths
of at most 512 bytes each. It rejects traversal, absolute paths, `.git` control
paths, symlinks, external mounts and special files. Each selected path must
remain an ignored regular file at capture; a missing or unsupported selection
fails the checkpoint. Do not include credential files in the checkpoint.

An initial status can have an empty native UUID. The managed native process creates
it, and the first successful stopped checkpoint binds that observed UUID once.

## Attach, move and return

Attach to the selected native TUI:

```sh
bin/sproozi attach --socket /absolute/private/home/control.sock --id SESSION_UUID
```

Terminal resize reaches the harness. Ctrl-] detaches the local client while its daemon
and native process remain alive; attach again with the same command. Ctrl-C while
attached reaches the harness. A reconnect replays a bounded terminal output tail. Avoid
native `/quit` when you intend to move; the placement owns graceful quit and the
independent container stop/checkpoint boundary.

Move to the configured profile service, attach there, then return locally:

```sh
bin/sproozi move --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --to remote
bin/sproozi attach --socket /absolute/private/home/control.sock --id SESSION_UUID
bin/sproozi move --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --to local
bin/sproozi attach --socket /absolute/private/home/control.sock --id SESSION_UUID
```

A graceful move blocks new terminal input and waits for the current turn and
supported tools to finish within the original deadline. Source gateway access
remains available during that wait. After the turn and tools finish, the
placement revokes access and confirms that the exact agent container has
stopped. It finalizes usage accounting and saves the checkpoint before preparing
another agent. The destination restores state without starting the agent, then
checks a fresh allocation. It cannot reopen a terminal Run or reuse old worker
credentials.

The original native UUID, instructions, ignored-file selection and absolute
deadline persist. Remaining allowance decreases by finalized usage, including
outstanding reservations. A move does not refill a budget or extend the
deadline. Detaching also leaves the writer's deadline and spending rules in
effect.

The positional `SESSION_UUID` form also works for all these client commands.
An explicit `--socket` selects home; `--executor docker --config FILE` uses the
configured home socket. For remote task-owner inspection, select that owner with
`--executor NAME`. `move --to NAME` still asks home to manage a placement visit.
See [session commands](../reference/session-commands.md) for the complete routing
and identity contract.

## Recover or cancel

For an uncertain response, first inspect the trusted journal:

```sh
bin/sproozi status --socket /absolute/private/home/control.sock --id SESSION_UUID
bin/sproozi recover --socket /absolute/private/home/control.sock --id SESSION_UUID
```

`start`, `move` and `cancel` print an operation UUID before sending their request.
Retain it even if the command fails. Recover continues the retained stage. To
retry explicitly, reuse the same session UUID, operation UUID and exact intent:

```sh
bin/sproozi start --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --operation START_OPERATION_UUID \
  --workspace /absolute/path/to/stopped-repository --include-ignored needed.bin
bin/sproozi move --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --operation MOVE_OPERATION_UUID --to remote
```

The start retry must repeat every original ignored-file flag. A changed operation
or destination cannot supersede an unfinished move. Remote control requests
default to 45 seconds; a slower completed-turn wait can return uncertainty with
input gated. Repeat the exact intent or recover after the backend can establish
its retained state. Attachment disconnects do not cancel the native turn.

If stop is unknown, the execution is unreachable or usage is missing, the
allocation stays held. The destination cannot start. A lost reply is not proof
of absence or zero consumption. Keep the home and profile journals intact,
restore backend connectivity, and recover the existing operation. Restart a
daemon/profile with the same protected state and configuration; stopping those
processes alone doesn't prove that the agent stopped. The native original
deadline still applies.

Managed remote Pods keep a Sproozi finalizer until the profile service records
that their harness container stopped. A stopped helper or lost profile service
can leave a Pod stuck terminating. Restart the same profile service and run
`recover` for the original operation, and keep its journal and checkpoint files.
Don't remove the finalizer or force-delete the Pod: Sproozi then can't tell
whether the agent stopped, so the allowance stays held and the session can't
move.

Local Docker visits remove their `spz-...` containers and workspace volume
after the stop and finalized usage are saved. A visit whose usage is still
unknown keeps its containers until a retried cancel or recover finalizes it.
Don't remove a visit's containers by hand before then: Sproozi treats a missing
container it did not remove as an unknown stop.

Cancellation forces the execution to stop:

```sh
bin/sproozi cancel --socket /absolute/private/home/control.sock \
  --id SESSION_UUID --operation CANCEL_OPERATION_UUID
```

Cancel persists intent, revokes access first and forces the exact execution to
stop. It keeps the last saved checkpoint and doesn't wait for the current turn
to finish. Until usage is finalized, the allowance stays held. A
cancelled session cannot move again. Retry with the same cancellation UUID or
recover; editing a journal or deleting a registration cannot safely refill it.

## Current limits

The checkpoint must match the selected harness and a real conversation UUID.
Codex needs its approved initial instructions and a finished final turn with its
tools. Claude Code needs a compatible primary transcript and the placement's
saved acknowledgement that the turn finished; a final assistant reply alone is
not enough. Selected
native history travels with sanitized Git state;
whole home directories, authentication files, hooks and Git config do not.

Workspace capture currently rejects unborn, conflicted, shallow/partial,
sparse/split-index and linked-worktree repositories, submodules, selected symlinks
and special files. Large archives, unsupported state and bounded storage failures
stop explicitly. Journals and retained artifacts also have limits; there is no
automatic garbage-collection or budget-reset command.

GUI and IDE attachment, adoption of existing sessions, large or compacted
histories, background tools and subagents are outside the supported workflow.

The reasons for these boundaries are recorded in
[ADR 0008](../adr/0008-selected-native-and-git-checkpoints.md),
[ADR 0009](../adr/0009-runtime-backends-and-sealed-accounting.md) and
[ADR 0010](../adr/0010-managed-session-authority-and-admission.md).
