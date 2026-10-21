# Run Codex or Claude Code locally with Docker

This guide sets up Sproozi on one computer and runs Codex or Claude Code in a
restricted Docker container. You need a running Docker engine; you do not need
a Sproozi checkout, Go, OpenSSL, Python, kubectl or a Kubernetes cluster.

`sproozi init` sets up everything: the agent image, a private Docker network,
a certificate authority, your provider key, the configuration and a background
service. It reports ready only after a test container verifies the gateway's
identity and fails to reach the internet, the provider directly, other gateway
ports, cloud metadata, direct DNS and IPv6.

The [local installation reference](../reference/local-installation.md) lists
the files, defaults, limits and supported platforms.

## Before you start

- macOS with Docker Desktop, or Linux with Docker Engine, using Linux
  containers. Setup has been verified on macOS with Docker Desktop; Docker
  Engine on Linux is supported but has no recorded acceptance run yet. Windows
  needs a [hand-written configuration](windows.md).
- An OpenAI API key for Codex or an Anthropic API key for Claude Code. Setup
  stores the key for the gateway; agents never receive it.
- The Sproozi CLI from [Homebrew or a release download](install.md).

Sproozi is a preview. Read [what a container does not protect](#what-this-does-not-protect)
before using it with sensitive repositories.

## Set up

Run setup once. It asks for your API key without echoing it:

```sh
sproozi init
```

That sets up Codex. For Claude Code, run `sproozi init --harness claude-code`.
The output ends with:

```text
Ready. In a Git repository, start an agent with:
  sproozi run
```

Setup is safe to repeat: it reuses the same network, certificate authority,
key and configuration, and keeps values you edited. For scripts, read the key
from a file only you can read, or from standard input, and print JSON:

```sh
sproozi init --credential-file /absolute/path/to/openai-key --json
```

A key is never accepted as a command-line value. Setup checks it by listing
the provider's models, which makes no inference request; `--skip-credential-check`
skips that.

## Run an agent

In any Git repository with at least one commit:

```sh
cd /path/to/repository
sproozi run
```

Sproozi copies the repository's history, branches, staged and unstaged changes
and untracked files into the session. Ignored files stay behind unless you name
them with `--include-ignored`. Your directory is not changed; the agent works on
its own copy.

`run` prints the session ID and opens the agent's terminal. Press Ctrl-] to
detach; Ctrl-C reaches the agent. Detaching leaves the agent running until its
deadline. `run -d` starts without opening the terminal.

```sh
sproozi ls            # list sessions
sproozi attach 3f2b   # open its terminal again: the start of the ID is enough
sproozi stop 3f2b     # stop after the current turn and keep the work
sproozi resume 3f2b   # carry on later, with a new deadline
sproozi rm 3f2b       # delete a stopped session and its saved work
```

`cancel` stops a session at once, discarding the agent's current turn. These
commands start the background service if it is stopped; `ls` does not, and
says how to start it.

Each agent uses its own default model and settings. To pass the agent's own
flags, put them after `--`:

```sh
sproozi run -- --model gpt-5.5
```

Flags that Sproozi controls, such as Codex's `--sandbox`, are refused with the
reason; the [command reference](../reference/session-commands.md#agent-arguments)
lists them.

The gateway refuses the agent's own services that Sproozi doesn't allow, such
as Claude Code's remote settings. Claude Code then shows `Remote managed
settings failed to load (access denied (403))`; the agent still works.
[Sproozi integration](../reference/sproozi-integration.md) explains how the
agent can ask why a request was refused.

## Add the other agent

Each session keeps the agent it started with. To use both, run setup again for
the other one, then choose it per session:

```sh
sproozi init --harness claude-code
sproozi run --harness claude-code
```

The second agent gets its own gateway port and provider key, so a Codex
container cannot use the Anthropic key, or the reverse. If a session is
running, setup saves the change and says to restart the service once the
session stops.

## Add your instructions and skills

Your [profile](../reference/profile.md) is `~/.sproozi/profile`. Put
instructions for every session in `AGENTS.md` there, and agent-specific ones in
`codex/AGENTS.md` or `claude/CLAUDE.md`. Add skills as `skills/<name>/SKILL.md`.
Each launch gets a copy, so changes apply to the next session. Approved
instructions from the configuration take precedence. `sproozi doctor` checks
the profile and reports files no agent reads.

## Check and maintain the installation

`sproozi doctor` checks the installation without changing it and names the next
step for anything that fails.

| Task | Command |
| --- | --- |
| Check everything, including each stored key | `sproozi doctor --check-provider` |
| See whether the service runs | `sproozi service status` |
| Replace a provider key | `sproozi credentials set codex` (or `claude-code`) |
| See which keys are stored | `sproozi credentials list` |
| Stop or start the service | `sproozi service stop`, `sproozi service start` |
| See certificate expiry and renewal | `sproozi certificates status` |

The gateway reads keys only when it starts. `credentials set` restarts the
service when no session is running; otherwise it says it keeps the previous key
until the sessions stop. `service stop` refuses while sessions run; stop or
cancel them first, or use `--leave-running` to let them run on until their
deadlines.

The service renews its certificate and rotates its certificate authority by
itself. `sproozi certificates replace-ca` is only for a lost or compromised
authority, and refuses while any session runs. The
[local installation reference](../reference/local-installation.md#certificates)
has the details.

## Troubleshooting

| Message | Next step |
| --- | --- |
| `docker: engine unavailable` | Start Docker Desktop or the Docker service, then repeat the command. |
| `root: … is a Git checkout, not a Sproozi installation` | The installation directory holds something else. Move it, or choose another with `--root`. |
| `root: … exceeds the 103-byte Unix socket limit` | Choose a shorter `--root`. |
| `network: no host address is reachable from the worker network` | A firewall may block the gateway port; choose another with `--port`. |
| `registry: pull failed` | Check registry access, or build the image with `--build SOURCE_DIRECTORY` from a release source bundle or checkout. |
| `image: … does not provide helper contract` | The image belongs to another Sproozi release. Use the release image or rebuild. |
| `provider: the provider rejected the credential` | Replace the key with `sproozi credentials set`. |
| `workspace: the repository has no commits` | Make a first commit, then `sproozi run` again. |
| `workspace: byte limit exceeded` | The repository's history is larger than one checkpoint. The message gives the size and how to raise `local.maxCheckpointBytes`, up to 64 MiB. |
| `the Sproozi service is not running` | Run `sproozi service start`. |
| `gateway.listen "127.0.0.1:PORT": address already in use` | Another program holds the gateway port. Stop it, then `sproozi service start`. |
| `service: the service exited during startup` | Read the log the message names, usually `~/.sproozi/logs/service.log`. |

## What this does not protect

- The agent can run any command inside its container, including tools that
  change its copy of the workspace. Review its changes before using them.
- Sproozi has no command yet to bring the agent's changes back into your
  repository; see [returning a session's work](../explanations/future-architecture/returning-work-to-the-user.md).
- Every local session uses the same profile. It is a convenience for one user,
  not isolation between untrusted users or agents.
- The background service controls Docker. Anyone who controls your account or
  Docker engine controls Sproozi too.
- Token allowances are accounting limits, not a guarantee against provider
  spend. The monetary allowance is unbounded unless you configure pricing and a
  bound.

## Remove the installation

Delete your sessions with `sproozi rm`, stop the service, then remove the
installation's Docker network and directory (`~/.sproozi` unless you chose
another with `--root`). `sproozi doctor` names the network, `sproozi-ID`:

```sh
sproozi service stop
docker network rm sproozi-ID
rm -rf ~/.sproozi
```

Removing the network never touches Docker resources Sproozi did not create.
Deleting the directory removes saved sessions, provider keys and the
certificate authority.
