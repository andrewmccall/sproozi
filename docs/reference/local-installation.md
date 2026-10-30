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
| Questions | Before any step, when standard input and standard error are terminals and `--json` is not given: without `--harness`, asks which agent to set up; without `--sign-in`, `--token`, `--chatgpt`, `--credential-file` or `--credential-stdin`, asks how it signs in, offering first to keep an agent's current choice. Enter picks the first choice. Without a terminal, a missing `--harness` means `codex`. |
| Docker | Uses `docker` from `PATH` and the current context's local Unix socket. Remote TCP engines are rejected. The engine must run Linux containers. |
| Image | Reuses the recorded image when `--image` names it again. Otherwise pulls the native image digest built into the CLI release, or uses `--image REPOSITORY@sha256:DIGEST` or a local `sha256:` content ID, or builds with `--build SOURCE_DIRECTORY`. The image must report helper contract `1`. |
| Network | Creates or reuses the bridge network `sproozi-ID`, labelled `sproozi.com/installation=ID`. Docker chooses a subnet that does not overlap its other networks. A same-named network that this installation does not own is rejected. |
| Gateway address | Serves a one-time value from the host and fetches it from a container on the network. It tries, in order: host loopback reached through the engine's host name, the bridge gateway address, then each of those on all interfaces. The first address that works is recorded. |
| Credentials | With `--sign-in`, stores no key: the harness signs in to its own provider account in its first session. Otherwise stores the provider key from a hidden prompt, `--credential-file` or `--credential-stdin`. With `--provider-ca-file`, copies that PEM bundle to `credentials/PROVIDER-ca.pem` and points the harness's gateway at the copy; a later run with the flag replaces it. It then lists the provider's models to check the key, unless `--skip-credential-check` is given. |
| Certificates | Creates the certificate authority and a serving leaf covering the gateway address and the enabled provider authorities. |
| Configuration | Writes `config.json`. On a later run it adds a new harness and keeps every other value. It refuses when the existing configuration's image, network, gateway address or approved instructions differ from the installation. |
| Service | Starts `sproozi service run` detached from the terminal. After a configuration or credential change it restarts the service only when no session is running; otherwise it saves the change, skips the readiness check and prints `Saved. N session(s) are running, so the service keeps its previous settings; ...`, naming `sproozi service stop --leave-running` and `sproozi service start` to apply them, and exits 0. |
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
| Model allowance | No limit; usage is still counted. Set `allowances.model.inference.units` to `{"bounded": true, "value": N}` for one |
| Monetary allowance | Unbounded; a bound needs administrator pricing |
| External services | Model inference only |
| First gateway port | 18443; each further harness uses the next free port |
| Approved instructions | A short product default, or `--instructions-file` on the first run |

Setup never rewrites these values in an existing `config.json`, so an
existing installation keeps its checkpoint and retained limits until you
raise them. One exception: a model allowance still at the earlier default of
200,000 units becomes no limit
([ADR 0051](../adr/0051-interactive-sessions-may-run-without-a-model-budget.md)).
A session without a model limit can't move to a Kubernetes cluster, whose
ceilings need one. When a workspace exceeds the
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
| `codex` | `local` when first, otherwise `local-codex` | `https://api.openai.com` | Key file `credentials/openai`; `--token` stores a sign-in token there; `--chatgpt` keeps a ChatGPT plan sign-in in `credentials/chatgpt-session.json`; `--sign-in` uses Codex's own sign-in in its login volume |
| `claude-code` | `local` when first, otherwise `local-claude` | `https://api.anthropic.com` | Key file `credentials/anthropic`; `--token` stores a sign-in token there; `--sign-in` uses Claude Code's own sign-in in its login volume |

`--provider-url` and `--provider-ca-file` select a private HTTPS provider
endpoint for the harness being added. The CA bundle is public, so its source
may be readable by anyone, but setup refuses one every account can write. The
gateway reads only the private copy. Codex receives `OPENAI_API_KEY` as a
placeholder; the gateway replaces it with the stored key on approved requests.

`providerTokenKind` says what `providerTokenFile` holds: omitted for an API
key, `token` for a sign-in token the gateway sends as a bearer (with
Anthropic's OAuth beta header for Claude Code), or `chatgpt` for a ChatGPT plan
sign-in the gateway refreshes
([ADR 0047](../adr/0047-harnesses-sign-in-to-their-own-model-providers.md)).

With `--sign-in` the configuration has no `providerTokenFile`, and the agent
receives no placeholder key. Its sign-in lives in a persistent Docker volume,
created on its first session and labelled `sproozi.com/login=<harness>`. The
helper image prepares it as an empty mode-0600 file owned by the agent user,
and every session of that agent mounts that one file in its native home
(`/workspace/session/claude/.credentials.json` or
`/workspace/session/codex/auth.json`); no other container mounts it. Removing
the installation leaves the volume; `docker volume rm` deletes the sign-in.
The gateway forwards the sign-in on every host of the agent's provider
(`api.anthropic.com`; `api.openai.com` and `chatgpt.com`), relays only its
sign-in and refresh requests on `platform.claude.com` or `auth.openai.com`,
and covers those hosts in its certificate. On `chatgpt.com` it relays only
`codex/models`, `wham/accounts/check` and `wham/settings/user` besides model
calls. Setup again with a key
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


## Build container images explicitly

Published images remain the default. Building is an administrator operation and
requires Docker with Buildx. The host does not need Go or npm: those tools run
in the image build stages. Base images and pinned dependencies still need
network access on the first build.

To build both the native agent image and the control image without setting up
an installation:

```sh
sproozi images build
```

The command builds from the first source it finds:

| Source | Used when |
| --- | --- |
| `--build DIR` | Given. A development checkout uses its own recipes and pins and may contain custom changes. |
| Installed release source | The CLI's package installed it in `PREFIX/share/sproozi`, beside `PREFIX/bin/sproozi`, and its `release.json` names the CLI version. The Homebrew formula installs it. |
| Release source bundle | Otherwise, for a released CLI. Downloads `sproozi_VERSION_source.tar.gz` and `checksums.txt` from the CLI's GitHub release, checks the archive against the checksum, builds in a temporary directory and removes it afterwards. The checksum comes from the same release, so it detects a damaged download, not a replaced release. |

A source-built CLI (`dev`) has no release source and needs `--build DIR`. Any
source with a `release.json` must match the released CLI version. The command
prints the source it uses on standard error.

This uses the same pinned Docker Bake recipe as release CI and loads the images
into the selected Docker engine as `sproozi-local/sproozi-native:VERSION` and
`sproozi-local/sproozi:VERSION`. `VERSION` is the CLI version, or `dev` for a
source-built CLI. Use `--image-prefix PREFIX` and `--image-tag TAG` to select
other names; use a distinct tag for modified source. `--platform linux/amd64`
or `--platform linux/arm64` selects one architecture; otherwise Buildx uses its
builder's default.

For a registry your Kubernetes nodes can access, authenticate with `docker login`
and explicitly publish:

```sh
sproozi images build \
  --image-prefix ghcr.io/YOUR-ACCOUNT --image-tag custom \
  --platform linux/amd64,linux/arm64 --push
```

The command uses your selected Docker context, Buildx builder and registry login.
Multiple platforms require `--push`. It reports the two image references; `--json`
returns `native`, `control` and `pushed`, with build progress on standard error.
A failed build returns an error and no success result. It never changes a running
installation and never builds automatically after a failed pull.

For local sessions, `sproozi init --build /path/to/sproozi` or
`sproozi images prepare --build /path/to/sproozi` builds and selects only the
native image. To use a native image built by `images build`, obtain its local
content ID with `docker image inspect --format '{{.Id}}' IMAGE` and pass that
`sha256:...` ID to `sproozi init --image`.

For Kind, load each locally built image with `kind load docker-image IMAGE
--name CLUSTER`. For other clusters, use `--push`, inspect the published images
with `docker buildx imagetools inspect IMAGE`, and configure the installation
with their digest-pinned references. Building does not deploy images or modify
cluster manifests. The control image supplies the controller, gateway, task
service and certificate controller; the native image supplies the managed
agents, checkpoint helper and network anchor.
