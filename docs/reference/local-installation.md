# Local installation

`sproozi init --local` provisions a local installation from a running Docker
engine. This page lists what it creates, its defaults and limits, and the
supported platforms. The [local sessions guide](../guides/local-sessions.md)
walks through setup and daily use. [ADR 0029](../adr/0029-local-provisioning-and-lifecycle.md)
records the design decisions.

Local setup never reads kubeconfig, discovers cluster contexts, installs CRDs
or contacts a Kubernetes API.

## Installation root

The root is `~/sproozi` unless `init --local --root PATH` chooses another. A
non-default root is recorded in the platform user configuration directory as
`sproozi/root`, so later commands find it. An existing platform
`sproozi/config.json` (from a manual installation) stays the default
configuration for session commands.

| Path | Contents | Reaches workers |
| --- | --- | --- |
| `config.json` | Home daemon configuration. You may edit tunable values; setup keeps them. | No |
| `config.json.backup-*` | The previous configuration, saved before setup changes it. | No |
| `installation.json` | Provisioning journal: installation ID, Docker endpoint, image, network, gateway address and harnesses. | No |
| `profile/` | Your [profile](#profile): instructions and skills for every session. | A copy at each launch |
| `credentials/` | Provider keys, one file per provider. | No |
| `pki/` | Certificate authority keys, the serving leaf, trust generations and the trust-consumer record. | Only the public trust bundle |
| `state/` | Journals, accounting, grants, placement records, retained checkpoints and the control socket. | No |
| `docker-cli/` | A private Docker CLI configuration with no registry credentials. | No |
| `logs/service.log` | Home daemon log. | No |
| `service.pid` | Process ID of a service started by `sproozi service start`. | No |

Directories are created with mode `0700` and files with `0600`. Setup and the
service refuse existing paths with broader permissions, a different owner or a
symbolic link, and never repair them.

The Unix control socket path, `state/control.sock`, must be at most 103 bytes.
Setup fails early with a `root` error when a long `--root` would exceed that.

## What setup does

Each step records its result in `installation.json`. One `setup.lock` prevents
concurrent runs. A repeated or interrupted run reuses the recorded network,
certificate authority, credentials and configuration.

| Step | Behaviour |
| --- | --- |
| Docker | Uses `docker` from `PATH` and the current context's local Unix socket. Remote TCP engines are rejected. The engine must run Linux containers. |
| Image | Pulls the native image digest built into the CLI release, or uses `--image REPOSITORY@sha256:DIGEST` or a local `sha256:` content ID, or builds with `--build SOURCE_DIRECTORY`. The image must report helper contract `1`. |
| Network | Creates or reuses the bridge network `sproozi-ID`, labelled `sproozi.com/installation=ID`. Docker chooses a subnet that does not overlap its other networks. A same-named network that this installation does not own is rejected. |
| Gateway address | Serves a one-time value from the host and fetches it from a container on the network. It tries, in order: host loopback reached through the engine's host name, the bridge gateway address, then each of those on all interfaces. The first address that works is recorded. |
| Credentials | Stores the provider key from a hidden prompt, `--credential-file` or `--credential-stdin`. It then lists the provider's models to check the key, unless `--skip-credential-check` is given. |
| Certificates | Creates the certificate authority and a serving leaf covering the gateway address and the enabled provider authorities. |
| Configuration | Writes `config.json`. On a later run it adds a new harness and keeps every other value. It refuses when the existing configuration's image, network, gateway address or approved instructions differ from the installation. |
| Service | Starts `sproozi serve` detached from the terminal. After a configuration or credential change it restarts the home daemon only when no session is active; otherwise it stops with a `service` error saying the restart is still needed. |
| Readiness | For each harness, starts a network anchor and a restricted worker on the network and verifies the gateway's TLS identity. It then confirms the worker cannot reach a public address, either provider directly, another gateway port, cloud metadata, direct DNS or IPv6. |

On Docker Desktop, workers reach the gateway at the engine's host address and
the gateway listens only on `127.0.0.1`. On Linux Docker Engine it listens on
the bridge gateway address. A wildcard listener is used only when neither works;
`sproozi doctor` then warns and names the ports to restrict in the host firewall.

## Defaults

Setup writes these starter values. Edit them in `config.json`, then restart the
service with `sproozi service stop` and `sproozi service start`.

| Setting | Default |
| --- | --- |
| Execution deadline (`maxDuration`) | 30 minutes |
| Worker CPU, memory, processes | 1 CPU, 1 GiB, 256 |
| Checkpoint helper | 0.5 CPU, 512 MiB, 128 processes |
| Network anchor | 0.1 CPU, 64 MiB, 32 processes |
| Managed workspace volume | 512 MiB |
| Maximum checkpoint | 8 MiB |
| Retained checkpoint storage | 128 MiB |
| Model allowance | 200,000 accounted tokens |
| Monetary allowance | Unbounded; a bound needs administrator pricing |
| External services | Model inference only |
| First gateway port | 18443; each further harness uses the next free port |
| Approved instructions | A short product default, or `--instructions-file` on the first run |

The image, network, gateway address and approved instructions bind retained
sessions. Changing them in place is not supported; setup and
`sproozi images prepare` refuse while local session state exists.

## Harnesses

Each harness is a separate local placement with its own gateway port, grants
and provider key. The first harness is the configuration's `local` placement;
later ones are listed under `harnesses`. Every placement shares the image,
network, gateway address, limits, allowances and certificate authority.

| Harness | Placement | Provider | Key file |
| --- | --- | --- | --- |
| `codex` | `local` when first, otherwise `local-codex` | `https://api.openai.com` | `credentials/openai` |
| `claude-code` | `local` when first, otherwise `local-claude` | `https://api.anthropic.com` | `credentials/anthropic` |

`--provider-url` and `--provider-ca-file` select a private HTTPS provider
endpoint for the harness being added. Codex receives `OPENAI_API_KEY` and Claude
Code `ANTHROPIC_API_KEY` as placeholders; the gateway replaces them with the
stored key on approved requests.

## Profile

`profile/` is your [profile](profile.md): `AGENTS.md` for every harness,
`codex/AGENTS.md` and `claude/CLAUDE.md` for one harness each, and
`skills/<name>/`. Setup creates the empty folders. The home daemon archives the
profile for every launch and sends it with the launch configuration; nothing in
the running container can change your folder. `sproozi doctor` checks the
profile, warns when it nears the size limit and lists entries no harness reads.

## Certificates

| Setting | Value |
| --- | --- |
| Key type | ECDSA P-256 |
| Certificate authority lifetime | 1 year |
| Serving leaf lifetime | 30 days, never past its authority's expiry |
| Leaf renewal | After two thirds of its lifetime |
| Routine rotation | Begins 60 days before the authority expires |
| Check interval | Every minute while the home daemon runs, and at startup |

`sproozi certificates status` reports the serving and trusted generations, the
issuer fingerprint, expiry and renewal times, and any running sessions a
rotation is waiting for. The trust bundle contains certificates only. Sproozi
never adds its authority to the host's trust store, and the authority never
verifies upstream providers.

A missing or unreadable authority key, or a lost `pki/state.json`, stops
renewal and new launches; it never creates a replacement automatically. After a
switch, the home daemon checks its own listeners against the new trust before
it retires the previous authority. `certificates replace-ca` creates a new
authority, trusts only it and destroys every older key.

## Supported platforms

| Host | Engine | Automatic setup |
| --- | --- | --- |
| macOS | Docker Desktop, Linux containers | Verified |
| Linux | Docker Engine | Supported; no recorded acceptance run yet |
| Windows | Docker Desktop | Not supported; use the [manual configuration](../../cmd/sproozi/README.md) |

## Release images

A release CLI contains the digest of its native image, published as a
multi-architecture image (`linux/amd64`, `linux/arm64`). `sproozi version`
prints it. The release's `images.json` asset lists the image and platform
digests, the installed Codex and Claude Code versions, the helper contract and a
hash of the build recipe. `sproozi images status` shows the installed and
release images. Development builds have no release image; use `--image` or
`--build`.
