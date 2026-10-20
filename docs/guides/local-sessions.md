# Run Codex or Claude Code locally with Docker

This guide sets up Sproozi on one computer and starts a native Codex or Claude
Code terminal in a restricted Docker container. You need a running Docker
engine; you do not need a Sproozi checkout, Go, OpenSSL, Python, kubectl or a
Kubernetes cluster.

`sproozi init --local` provisions everything the home daemon needs. It finds
the Docker engine, prepares the native image, creates a Docker network,
works out the address workers use to reach the gateway, creates a private
certificate authority, stores your provider key, writes the configuration and
starts the home daemon. It reports ready only after a test container on the
restricted worker path verifies the gateway's TLS identity and fails to reach
the internet, the provider directly, other gateway ports, cloud metadata,
direct DNS and IPv6.

The [local installation reference](../reference/local-installation.md) lists
the files, defaults, limits and supported platforms.

## Before you start

- macOS with Docker Desktop, or Linux with Docker Engine, using Linux
  containers. Automatic setup has been verified on macOS with Docker Desktop.
  It also supports Docker Engine on Linux, but no recorded acceptance run
  covers that yet. Windows needs the [manual home configuration](../../cmd/sproozi/README.md).
- An OpenAI API key for Codex or an Anthropic API key for Claude Code. Setup
  stores the key for the gateway; agents never receive it.
- The model name to use, for example the one you use with the stock CLI.
- The Sproozi CLI from [Homebrew or a release download](install.md).

Sproozi is a preview. Read [what a container does not protect](#what-this-does-not-protect)
before using it with sensitive repositories.

## Set up

Run setup once. It asks for the model and the API key, without echoing the key:

```sh
sproozi init --local --harness codex
```

For Claude Code, use `--harness claude-code`. The output ends with:

```text
Ready. Start a session with:
  sproozi start --workspace "$PWD" --harness codex
```

Setup is safe to repeat. A repeated or interrupted run reuses the same network,
certificate authority, credential and configuration. It keeps any values you
edited in the configuration.

For scripts, supply every answer and read a JSON result:

```sh
sproozi init --local --harness codex --model MODEL \
  --credential-file /absolute/path/to/openai-key --json
```

The key file must be owned by you with mode `0600`. You can pipe the key with
`--credential-stdin` instead. A secret is never accepted as a command-line value.
Setup checks the key by listing the provider's models; this makes no inference
request. Use `--skip-credential-check` to skip it.

## Start a session

Stop other programs writing to the repository, then start a session from it:

```sh
cd /path/to/repository
sproozi start --workspace "$PWD" --harness codex
```

Sproozi copies the repository's Git history, branches, staged and unstaged
changes and ordinary untracked files into a managed workspace. Ignored files
stay behind unless you name them with `--include-ignored`. Your directory is not
changed; the agent works in its own copy.

`start` prints the session ID and opens the stock terminal. Press Ctrl-] to
detach; Ctrl-C reaches the agent. Detaching leaves the agent running within its
deadline. Use `--detach` to start without attaching.

```sh
sproozi sessions
sproozi attach SESSION_UUID
sproozi stop SESSION_UUID
sproozi resume SESSION_UUID
```

`stop` waits for the current turn, stops the container and keeps the
conversation and workspace. If the home daemon is not running, these commands
start it. The [session commands reference](../reference/session-commands.md)
covers every command and flag.

## Add the other harness

Each session keeps the harness it started with. To use both, run setup again
for the other harness:

```sh
sproozi init --local --harness claude-code
sproozi start --workspace "$PWD" --harness claude-code
```

The second harness gets its own gateway port and provider key. Its workers can
reach only that port, so a Codex worker cannot use the Anthropic key and the
reverse. Setup restarts the home daemon to load the new harness when no session
is active. If a session is running, setup writes the configuration and asks you
to restart the home daemon once the session stops.

## Share your instructions

Put instructions that every session should follow in
`~/sproozi/agents/shared/AGENTS.md`. Add harness-specific ones in
`~/sproozi/agents/codex/AGENTS.md` or `~/sproozi/agents/claude/CLAUDE.md`.
Sproozi reads them freshly for each launch and gives the agent a read-only
copy. Edit them on your computer; changes apply to the next launch. Approved
developer instructions still come from the configuration and take precedence.

The [harness table](../reference/local-installation.md#shared-agent-files)
shows how each harness receives them. `sproozi doctor` reports any file in
`agents/` that no harness reads.

## Check and maintain the installation

`sproozi doctor` checks the installation without changing it. It names the
failing component and the next step. Its final check starts a temporary network
anchor and worker on the restricted path, repeats the TLS and denial probes, and
removes both containers.

```sh
sproozi doctor
sproozi doctor --check-provider   # also validates each stored key
sproozi service status
```

| Task | Command |
| --- | --- |
| Replace a provider key | `sproozi credentials add openai` or `sproozi credentials add anthropic` |
| Stop the home daemon | `sproozi service stop` |
| Start it again | `sproozi service start` |
| See certificate expiry and renewal | `sproozi certificates status` |
| Renew the gateway certificate now | `sproozi certificates renew` |
| Begin certificate authority rotation | `sproozi certificates rotate-ca` |

The gateway reads provider keys only when it starts. `credentials add` restarts
the home daemon when no session is active; otherwise it says so and keeps the
daemon running with the previous key.

`service stop` refuses while sessions are active. Stop or cancel them first, or
use `--leave-running`: containers then keep running until their deadlines, and
stopping the home daemon is not evidence that they stopped.

### Certificates

The home daemon renews its gateway certificate automatically. It checks every
minute, which also covers a computer waking from sleep. A renewed certificate
applies to new connections without a restart.

Routine certificate authority rotation is also automatic. It begins 60 days
before the authority expires. The home daemon first gives new sessions a bundle
that trusts both the old and the new authority. It switches the gateway to the
new authority only once every running session has that bundle or has stopped,
then removes the old authority. `sproozi certificates status` lists the
sessions a rotation is waiting for. `rotate-ca` starts a rotation early.

`sproozi certificates replace-ca` is for a lost or compromised authority. It
refuses while sessions are active or any container runs on the installation's
network, because they would lose trust in the gateway. It never runs
automatically.

## Troubleshooting

| Message | Next step |
| --- | --- |
| `docker: engine unavailable` | Start Docker Desktop or the Docker service, then repeat the command. |
| `root: … exceeds the 103-byte Unix socket limit` | Choose a shorter `--root`. |
| `network: no host address is reachable from the worker network` | A firewall may block the gateway port; choose another with `--port`. |
| `registry: pull failed` | Check registry access, or build the image locally with `--build SOURCE_DIRECTORY` from a release source bundle or checkout. |
| `image: … does not provide helper contract` | The image belongs to another Sproozi release. Use the release image or rebuild. |
| `provider: the provider rejected the credential` | Replace the key with `sproozi credentials add`. |
| `config: the existing configuration's placement differs` | The configuration no longer matches the installation's image, network or gateway. Restore the backup next to `config.json` or run `sproozi doctor`. |
| `service: home daemon exited during startup` | Read the log named in the message, usually `~/sproozi/logs/service.log`. |

## What this does not protect

- The agent can run any command inside its container, including tools that
  change the managed workspace. Review its changes before using them.
- Every local session uses the same `agents/` directory. Anything you place
  there reaches every Codex session. It is a convenience for one user, not
  isolation between untrusted users or agents.
- The trusted home daemon controls Docker. Anyone who controls your account or
  Docker engine controls Sproozi too.
- Token allowances are accounting limits, not a guarantee against provider
  spend. Setup leaves the monetary allowance unbounded unless you configure
  pricing and a bound.

## Remove the installation

Stop every session, then stop the home daemon and delete the files:

```sh
sproozi service stop
docker network ls --filter label=sproozi.com/installation
docker network rm sproozi-INSTALLATION_ID
rm -rf ~/sproozi
```

The network name is in `~/sproozi/installation.json` and in `sproozi doctor`
output. Removing it never touches Docker resources that Sproozi did not create.

Deleting `~/sproozi` removes retained conversations, checkpoints, provider keys
and the certificate authority. Back up anything you want to keep first.
