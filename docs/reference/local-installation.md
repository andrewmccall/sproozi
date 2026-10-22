# Local installation

`sproozi init` sets up a local installation from a running Docker
engine. This page lists what it creates, its defaults and limits, and the
supported platforms. The [local sessions guide](../guides/local-sessions.md)
walks through setup and daily use. [ADR 0029](../adr/0029-local-provisioning-and-lifecycle.md)
records the design decisions.

Local setup never reads kubeconfig, discovers cluster contexts, installs CRDs
or contacts a Kubernetes API.

## Installation root

Every command finds the installation the same way: `--root PATH`, else the
`SPROOZI_ROOT` environment variable, else the root `init --root PATH` recorded
in the platform user configuration directory as `sproozi/root`, else
`~/.sproozi`. A root chosen by `SPROOZI_ROOT` is not recorded. The
configuration is always the root's `config.json`.

The root must be new, empty or an existing installation: setup refuses a Git
checkout and a directory holding other files, with a `root` error that suggests
`--root`. It never asks you to change such a directory's permissions.

| Path | Contents | Reaches workers |
| --- | --- | --- |
| `config.json` | Service configuration. You may edit tunable values; setup keeps them. | No |
| `config.json.backup-*` | The previous configuration, saved before setup changes it. | No |
| `installation.json` | Provisioning journal: installation ID, Docker endpoint, image, network, gateway address and harnesses. | No |
| `profile/` | Your [profile](#profile): instructions and skills for every session. | A copy at each launch |
| `credentials/` | Provider keys, one file per provider, and `PROVIDER-ca.pem` copies of `--provider-ca-file` bundles. | No |
| `pki/` | Certificate authority keys, the serving leaf, trust generations and the trust-consumer record. | Only the public trust bundle |
| `state/` | Journals, accounting, grants, placement records, retained checkpoints and the control socket. | No |
| `docker-cli/` | A private Docker CLI configuration with no registry credentials. | No |
| `logs/service.log` | Service log. | No |
| `service.pid` | Process ID of a service started by `sproozi service start`. | No |
| `restart-pending` | Why the running service still uses earlier settings, written when `init` or `credentials set` could not restart it over running sessions; starting the service removes it. `doctor` and `service status` report it. | No |

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
| Image | Reuses the recorded image when `--image` names it again. Otherwise pulls the native image digest built into the CLI release, or uses `--image REPOSITORY@sha256:DIGEST` or a local `sha256:` content ID, or builds with `--build SOURCE_DIRECTORY`. The image must report helper contract `1`. |
| Network | Creates or reuses the bridge network `sproozi-ID`, labelled `sproozi.com/installation=ID`. Docker chooses a subnet that does not overlap its other networks. A same-named network that this installation does not own is rejected. |
| Gateway address | Serves a one-time value from the host and fetches it from a container on the network. It tries, in order: host loopback reached through the engine's host name, the bridge gateway address, then each of those on all interfaces. The first address that works is recorded. |
| Credentials | With `--sign-in`, stores no key: the harness signs in to its own provider account in its first session. Otherwise stores the provider key from a hidden prompt, `--credential-file` or `--credential-stdin`. With `--provider-ca-file`, copies that PEM bundle to `credentials/PROVIDER-ca.pem` and points the harness's gateway at the copy; a later run with the flag replaces it. It then lists the provider's models to check the key, unless `--skip-credential-check` is given. |
| Certificates | Creates the certificate authority and a serving leaf covering the gateway address and the enabled provider authorities. |
| Configuration | Writes `config.json`. On a later run it adds a new harness and keeps every other value. It refuses when the existing configuration's image, network, gateway address or approved instructions differ from the installation. |
| Service | Starts `sproozi service run` detached from the terminal. After a configuration or credential change it restarts the service only when no session is running; otherwise it saves the change, skips the readiness check and prints `Saved. N session(s) are running, so the service keeps its previous settings until they stop; ...`, exiting 0. |
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
| Maximum checkpoint (`local.maxCheckpointBytes`) | 32 MiB; it also bounds the workspace's expanded Git objects at `run`. The highest value home accepts is 64 MiB. |
| Retained checkpoint storage (`local.retainedBytes`) | 256 MiB; at least twice the maximum checkpoint |
| Model | None: each harness uses its own default model unless `--model` names one |
| Model allowance | 200,000 accounted tokens |
| Monetary allowance | Unbounded; a bound needs administrator pricing |
| External services | Model inference only |
| First gateway port | 18443; each further harness uses the next free port |
| Approved instructions | A short product default, or `--instructions-file` on the first run |

Setup never rewrites these values in an existing `config.json`, so an
existing installation keeps its checkpoint and retained limits until you
raise them. When a workspace exceeds the
limit, `run` names the size and `local.maxCheckpointBytes`.

The image, network, gateway address and approved instructions bind retained
sessions. Changing them in place is not supported; setup and
`sproozi images prepare` refuse while local session state exists.

## Harnesses

Each harness is a separate local placement with its own gateway port, grants
and provider credential. The first harness is the configuration's `local` placement;
later ones are listed under `harnesses`. Every placement shares the image,
network, gateway address, limits, allowances and certificate authority.

| Harness | Placement | Provider | Provider credential |
| --- | --- | --- | --- |
| `codex` | `local` when first, otherwise `local-codex` | `https://api.openai.com` | Key file `credentials/openai`, which the gateway holds |
| `claude-code` | `local` when first, otherwise `local-claude` | `https://api.anthropic.com` | Key file `credentials/anthropic`, or with `--sign-in` Claude Code's own sign-in in Docker volume `sproozi-<installation>-<placement>-login` |

`--provider-url` and `--provider-ca-file` select a private HTTPS provider
endpoint for the harness being added. The CA bundle is public, so its source
may be readable by anyone, but setup refuses one every account can write. The
gateway reads only the private copy. Codex receives `OPENAI_API_KEY` as a
placeholder; the gateway replaces it with the stored key on approved requests.

With `--sign-in`, Claude Code signs in itself with `/login`
([ADR 0047](../adr/0047-harnesses-sign-in-to-their-own-model-providers.md)).
Its configuration has no `providerTokenFile`, and it receives no placeholder
key. Its sign-in lives in a persistent Docker volume, created on the first
Claude Code session and labelled `sproozi.com/login=claude-code`. The helper
image prepares it as an empty mode-0600 file owned by the agent user, and
every Claude Code agent of the installation mounts that one file at
`/workspace/session/claude/.credentials.json`; no other container mounts it.
Removing the installation leaves the volume; `docker volume rm` deletes the
sign-in. The gateway forwards the sign-in to `api.anthropic.com`, relays
Claude Code's token refresh on `platform.claude.com` (`POST /v1/oauth/token`
only), and covers that host in its certificate. Setup again with a key
(`--credential-file`, `--credential-stdin` or the prompt) switches the harness
back to a gateway-held key; setup without either keeps its current choice.

## Profile

`profile/` is your [profile](profile.md): `AGENTS.md` for every harness,
`codex/AGENTS.md` and `claude/CLAUDE.md` for one harness each, and
`skills/<name>/`. Setup creates the empty folders. The service archives the
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
| Check interval | Every minute while the service runs, and at startup |

`sproozi certificates status` reports the serving and trusted generations, the
issuer fingerprint, expiry and renewal times, and any running sessions a
rotation is waiting for. The trust bundle contains certificates only. Sproozi
never adds its authority to the host's trust store, and the authority never
verifies upstream providers.

A missing or unreadable authority key, or a lost `pki/state.json`, stops
renewal and new launches; it never creates a replacement automatically. After a
switch, the service checks its own listeners against the new trust before
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
