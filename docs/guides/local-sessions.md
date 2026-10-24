# Run Codex, Claude Code or Copilot locally with Docker

This guide sets up Sproozi on one computer and runs Codex, Claude Code or
GitHub Copilot CLI in a restricted Docker container. You need a running Docker
engine; you do not need a Sproozi checkout, Go, OpenSSL, Python, kubectl or a
Kubernetes cluster. OpenCode and Hermes run only on Kubernetes; see
[harnesses](../reference/harnesses.md).

`sproozi init` sets up everything: the agent image, a private Docker network,
a certificate authority, your provider key, the configuration and a background
service. An agent can instead use your own subscription; see
[Choose how an agent signs in](#choose-how-an-agent-signs-in). It reports ready only after a test container verifies the gateway's
identity and fails to reach the internet, the provider directly, other gateway
ports, cloud metadata, direct DNS and IPv6.

The [local installation reference](../reference/local-installation.md) lists
the files, defaults, limits and supported platforms.

## Before you start

- macOS with Docker Desktop, or Linux with Docker Engine, using Linux
  containers. Setup has been verified on macOS with Docker Desktop; Docker
  Engine on Linux is supported but has no recorded acceptance run yet. Windows
  needs a [hand-written configuration](windows.md).
- An OpenAI API key for Codex or Copilot, or an Anthropic API key for Claude
  Code, or the subscription you want an agent to sign in with. Setup stores a
  key for the gateway; agents never receive it.
- The Sproozi CLI from [Homebrew or a release download](install.md).

Sproozi is a preview. Read [what a container does not protect](#what-this-does-not-protect)
before using it with sensitive repositories.

## Set up

Run setup once. It asks for your API key without echoing it:

```sh
sproozi init
```

That sets up Codex. For Claude Code, run `sproozi init --harness claude-code`;
for Copilot, `sproozi init --harness copilot`.
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

With `--json`, the result goes to standard output and the progress lines to
standard error. A key is never accepted as a command-line value. Setup checks it by listing
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
detach; Ctrl-C reaches the agent. Detaching prints the session ID and the next
commands, and leaves the agent running until its deadline: 30 minutes by
default, set by `maxDuration` in `~/.sproozi/config.json`. `run -d` starts
without opening the terminal.

```sh
sproozi ls            # list sessions
sproozi attach 3f2b   # open its terminal again: the start of the ID is enough
sproozi stop 3f2b     # stop after the current turn; ls then shows it as retained
sproozi resume 3f2b   # carry on later and reconnect (-d skips that), with a new deadline
sproozi rm 3f2b       # delete a stopped session and its saved work
```

`cancel` stops a session at once, discarding the agent's current turn. The
background service only needs to run while you use sessions. These commands
start it if it is stopped; `ls` does not, and says how to start it. In `ls`,
`PLACEMENT` is where a session runs: `local` is Docker on this computer, and
any other name is a Kubernetes cluster ([placement](../../GLOSSARY.md#placement)).

Each agent uses its own default model and settings. To pass the agent's own
flags, put them after `--`:

```sh
sproozi run -- --model gpt-5.5
```

Flags that Sproozi controls, such as Codex's `--sandbox`, are refused with the
reason; the [command reference](../reference/session-commands.md#agent-arguments)
lists them.

The gateway refuses the agent's own services that Sproozi doesn't allow, and
the agents say so; both are expected and the agent still works. Claude Code
shows `Remote managed settings failed to load (access denied (403))`. Codex
may show `⚠ 1 warning` because its telemetry (`ab.chatgpt.com`) is refused;
in a real terminal, F2 shows the warning.
[Sproozi integration](../reference/sproozi-integration.md) explains how the
agent can ask why a request was refused.

## Add another agent

Each session keeps the agent it started with. To use more than one, run setup
again for each, then choose it per session:

```sh
sproozi init --harness claude-code
sproozi run --harness claude-code
```

Each agent gets its own gateway port, so a Codex container cannot use the
Anthropic key, or the reverse. The first takes 18443 and a later one the next
free port after it; `--port` picks where that agent's search starts. Codex and
Copilot can use the same OpenAI key file; setup stores a copy for each agent.

If a session is running, setup saves the change and says so. To apply it
without stopping your sessions, run `sproozi service stop --leave-running`,
then `sproozi service start`.

## Choose how an agent signs in

Each agent uses one of three ways to authenticate, chosen when you run setup
for it: an API key, a sign-in token, or its own sign-in. Codex also offers a
ChatGPT plan held by the gateway. [How credentials reach an agent](../explanations/credentials.md)
explains who holds each credential, and why a token or plan sign-in held by
the gateway may breach your provider's terms.

| Option | Setup | Who holds the credential |
| --- | --- | --- |
| API key | `sproozi init --harness NAME` (the default) | The gateway; the agent never sees it |
| Sign-in token | `sproozi init --harness NAME --token --credential-file FILE`, with a long-lived token from your provider, such as the one `claude setup-token` prints, or for Copilot a GitHub token with Copilot access | The gateway, which adds it to each request |
| ChatGPT plan, held by the gateway | `sproozi init --harness codex --chatgpt`, which opens your browser | The gateway, which refreshes it |
| The agent's own sign-in | `sproozi init --harness NAME --sign-in`, then sign in inside the first session | The agent, in a login store of its own |

Copilot with an API key calls OpenAI in its bring-your-own-key mode and needs
no Copilot plan; it uses `gpt-6.1-sol` unless you choose another model. With a
token or its own sign-in it calls GitHub's Copilot service with your plan.

### The agent's own sign-in

With `--sign-in`, sign in inside the agent's first session as you would
outside Sproozi:

- **Claude Code:** type `/login`, open the link it shows, and paste the code
  back into the terminal.
- **Codex:** choose to sign in with ChatGPT using a device code, open the page
  it shows and enter the code. The browser sign-in that returns to
  `localhost` can't reach the container.
- **Copilot:** type `/login` and follow GitHub's device-code steps.

The agent keeps its sign-in in a [login store](../../GLOSSARY.md#login-store),
a Docker volume named after your installation, the agent and the login. Every
session of that login shares it: a sign-in, refresh or sign-out in one session
reaches the others within about a second. Sproozi never copies a sign-in from
your own machine.
The gateway forwards the sign-in to the provider, still counts every model
call against the session's budget, and refuses ChatGPT's own plugins and
connectors. The sign-in never enters a checkpoint, so a session moved to
Kubernetes signs in again there. Two things differ from a gateway-held key:

- The agent can read its own sign-in. The gateway refuses any request that
  carries it to another destination it inspects, but it can't see inside an
  encoded or split copy.
- Each refresh replaces the refresh token. If an agent keeps its own copy of
  the token rather than reading the shared file again, a second session of
  the login may need to sign in again after the first refreshed.

### Several accounts

Each agent can have several named logins, such as one for work. Set one up,
then start sessions on it:

```sh
sproozi init --harness copilot --sign-in --login work
sproozi run --harness copilot --login work
```

Without `--login`, a session uses the `default` login. `sproozi ls` shows each
session's login. A session keeps its login when it resumes or moves.

To switch an agent, run setup again with the option you want: `--sign-in`,
`--token`, `--chatgpt`, or a key through `--credential-file` or the prompt.
Setup without any of them keeps the agent's current choice.

## Add your instructions and skills

Your [profile](../reference/profile.md) is `~/.sproozi/profile`. Put
instructions for every session in `AGENTS.md` there, and agent-specific ones in
`codex/AGENTS.md`, `claude/CLAUDE.md` or `copilot/copilot-instructions.md`. Add skills as `skills/<name>/SKILL.md`.
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
| Replace a provider key | `sproozi credentials set codex` (or `claude-code` or `copilot` when it uses a key or token) |
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
| `Not logged in · Please run /login` in Claude Code | Type `/login` in the session; see [The agent's own sign-in](#the-agents-own-sign-in). |
| `the harness sent no provider credential; sign the harness in to its model provider` | The agent has no sign-in yet. Sign in inside the session. |
| `workspace: the repository has no commits` | Make a first commit, then `sproozi run` again. |
| `workspace: byte limit exceeded` | The repository's history is larger than one checkpoint. The message gives the size and how to raise `local.maxCheckpointBytes`, up to 64 MiB. |
| `the Sproozi service is not running, so this machine's sessions can't be listed` | Run `sproozi service start`, or any session command. |
| `doctor` shows `restart pending` | You added an agent or key while sessions ran. Stop them, then `sproozi service stop` and `sproozi service start`. |
| `setup at … is incomplete` | `init` stopped part-way, for example with no terminal to ask for the key. Run `sproozi init` again. |
| `provider: the provider's certificate is not trusted` | For a private endpoint, pass its CA bundle with `--provider-ca-file`. |
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
installation's Docker volumes, network and directory (`~/.sproozi` unless you
chose another with `--root`). `sproozi doctor` names the network, `sproozi-ID`;
the volumes, including each login store, start with the same name:

```sh
sproozi service stop
docker volume rm $(docker volume ls -q --filter name=sproozi-ID)
docker network rm sproozi-ID
rm -rf ~/.sproozi
```

These commands never touch Docker resources Sproozi did not create. Removing
the volumes signs every agent out; deleting the directory removes saved
sessions, provider keys and the certificate authority.
