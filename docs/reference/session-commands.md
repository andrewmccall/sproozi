# Commands

The executable is `sproozi`. [Install the CLI](../guides/install.md) covers
Homebrew, downloads and source builds, and the
[local sessions guide](../guides/local-sessions.md) shows the commands in use.
[ADR 0037](../adr/0037-one-command-surface.md) records why the surface looks
like this.

A [session](../../GLOSSARY.md#session) is one agent conversation and its
workspace. It keeps its ID while it stops, resumes and moves. Each start or
resume is a new [execution](../../GLOSSARY.md#execution) with its own approval,
deadline and usage allowance. Knowing a session ID gives no permission to run it.

## Commands

`sproozi --help` (or `sproozi help`) lists the commands in three groups and
exits 0; `sproozi` with no command prints the same list to standard error and
exits 2. `sproozi help COMMAND` and `sproozi COMMAND -h` print one command's
usage, a short description and one line per flag. An unknown help topic fails.

| Group | Command | What it does |
| --- | --- | --- |
| Sessions | `run [PATH] [-- AGENT ARGS]` | Start an agent on a copy of the Git repository at `PATH` (default: the current directory) and connect the terminal to it. With `--remote`, submit a task to a cluster instead. |
| Sessions | `sessions`, `ls` | List sessions on this machine and in the cluster. Never starts the service. |
| Sessions | `status ID` | Show one session. |
| Sessions | `attach ID` | Connect the terminal to a running session; `Ctrl-]` detaches. |
| Sessions | `stop ID` | Stop after the agent's current turn and keep the work. |
| Sessions | `resume ID [-- AGENT ARGS]` | Continue a stopped session where it left off, with a new deadline and allowance. |
| Sessions | `cancel ID` | Stop now, discarding the agent's current turn. Cancelling a cancelled session succeeds. |
| Sessions | `rm ID` | Delete a stopped or cancelled session and its saved work. |
| Remote | `move ID --local\|--remote` | Move a running session after its current turn: `--local` brings it to this machine, `--remote` sends it to a cluster. |
| Remote | `pull ID` | Bring a stopped session from a cluster to this machine; `resume` then continues it. |
| Setup | `init` | Set up this machine: image, network, certificates, provider key and service. Run again to add an agent with `--harness` or to repair. |
| Setup | `doctor` | Check the installation without changing it. |
| Setup | `service status\|start\|stop` | Show, start or stop the background service. |
| Setup | `credentials set AGENT\|PROVIDER`, `credentials list` | Store or replace a gateway-held provider key or token (`codex` or `openai`, `claude-code` or `anthropic`; for `copilot`, the OpenAI key or the GitHub token, whichever it uses), or list which are stored. An agent set up with `--sign-in` has no key to set. Keys are never printed. |
| Setup | `images status\|prepare\|build` | Show or prepare the installation’s agent image, or explicitly build native and control images from source. |
| Setup | `certificates status\|renew\|rotate-ca\|replace-ca` | Show or maintain this machine's certificate authority. |
| Setup | `version`, `-v`, `--version` | Show the version and the release's agent image. |

Two commands are not listed in help: `recover ID` finishes an operation that
was interrupted (error messages name it when it applies), and `service run`
runs the service in the foreground, which `service start` does for you.

## Conventions

- **Session IDs.** Every command that takes `ID` accepts the full UUID or any
  unique start of it, as `sproozi ls` shows it (8 characters). A prefix that
  matches several sessions fails and lists them.
- **Output.** Commands print text. `--json` prints one indented JSON document
  instead, on every command except `attach` and `help`. With `--json`, an
  error is one JSON line on standard error instead, such as
  `{"error":"no Sproozi installation at DIR","next":"run \"sproozi init\" to
  set one up, or choose another with --root","exitCode":1}`. Flags may also
  come before the command: `sproozi --json ls` is `sproozi ls --json`.
- **Exit status.** 0 on success; 2 for a command line that cannot run as
  written (an unknown command or flag, a missing operand, a bad value); 1 for
  anything else. Every error is one line on standard error starting `error:`.
  A misspelt command, verb or flag gets `did you mean ...?`.
- **Checked first.** Flags are checked before anything is read or sent, so
  `sproozi run --harness bogus` fails with `--harness must be codex or
  claude-code` and touches nothing.
- **Retries.** Only a request whose outcome is unknown, such as a dropped
  connection, ends with `the outcome is unknown: check with "sproozi status
  ID", and to try again repeat the command with --operation OP`. A refused
  request, an unknown session or a service that is not running has no retry
  hint: nothing was recorded, so run the command again as it is.
- **The service.** The service only needs to run while you use sessions, and
  it may not be running: that is normal, not a problem. `status`, `attach`,
  `stop`, `resume`, `cancel`, `rm`, `move`, `pull` and `run` start it if it is
  stopped, and say so. `sessions` never does: a listing has no side effects,
  and this machine's sessions live in records only the service reads safely.
  It fails with `the Sproozi service is not running, so this machine's
  sessions can't be listed; start it with "sproozi service start" (run,
  attach, stop and resume start it too)`. `service status` says whether it
  runs; `service stop` prints `Stopped the Sproozi service.`, or `The Sproozi
  service was not running.`
- **Plain names.** This machine is `local` everywhere: in `move --local`, in
  the `placement`, `ownership` and `executor` values, and in text output.

## Command flags

Each command accepts only the flags listed here; any other fails before
anything is read or sent. `-d` is short for `--detach`.

| Command | Flags |
| --- | --- |
| `run` | `--harness`, `--login`, `--detach`, `--include-ignored`, `--remote`, `--context`, `--namespace`, `--task`, `--task-file`, `--repository`, `--project`, `--json`, `--root` |
| `sessions` | `--remote`, `--context`, `--namespace`, `--project`, `--json`, `--root` |
| `status` | `--remote`, `--context`, `--namespace`, `--json`, `--root` |
| `attach` | `--root` |
| `stop` | `--json`, `--root` |
| `resume` | `--detach`, `--json`, `--root` |
| `cancel` | `--remote`, `--context`, `--namespace`, `--json`, `--root` |
| `rm` | `--json`, `--root` |
| `move` | `--local`, `--remote`, `--context`, `--namespace`, `--json`, `--root` |
| `pull` | `--context`, `--namespace`, `--json`, `--root` |
| `recover` | `--remote`, `--context`, `--namespace`, `--json`, `--root` |
| `init` | `--harness`, `--model`, `--credential-file`, `--credential-stdin`, `--skip-credential-check`, `--token`, `--chatgpt`, `--sign-in`, `--login`, `--provider-url`, `--provider-ca-file`, `--instructions-file`, `--image`, `--build`, `--port`, `--no-service`, `--timeout`, `--json`, `--root` |
| `doctor` | `--check-provider`, `--json`, `--root` |
| `service` | `--leave-running`, `--json`, `--root` |
| `credentials` | `--credential-file`, `--credential-stdin`, `--skip-credential-check`, `--json`, `--root` |
| `images` | `--image`, `--build`, `--image-prefix`, `--image-tag`, `--platform`, `--push`, `--json`, `--root` |
| `certificates` | `--json`, `--root` |
| `version` | `--json` |

| Flag | Meaning |
| --- | --- |
| `--root PATH` | Installation directory. Without it: `$SPROOZI_ROOT`, else the root `init --root` recorded, else `~/.sproozi`. |
| `--harness NAME` | `codex`, `claude-code` or `copilot`. For `run`, defaults to the first agent set up; for `init`, the agent to add, default `codex`. |
| `-d`, `--detach` | Start or resume without connecting the terminal. Without a terminal on standard input, `run` and `resume` start the session and say they did not attach. |
| `--include-ignored PATH` | Also copy this Git-ignored file into the session, on every visit. Repeatable. |
| `--remote` | Work in a cluster: `run --remote` submits a task there (it needs `--task` or `--task-file` and takes no `PATH`, `--detach`, `--include-ignored` or agent arguments); `move --remote` sends the session there; `sessions`, `status`, `cancel` and `recover` with `--remote` read or act on the cluster's sessions only. Implied by `--context` or `--namespace`. |
| `--context NAME` | The cluster, as a kubeconfig context name or a kubeconfig file. Default: the context named `sproozi`. See [Remote commands](#remote-commands). |
| `--namespace NS` | Namespace of the remote executor. Default: the context's namespace, else `default`. |
| `--task TEXT`, `--task-file FILE` | Task for a remote run, inline or from a UTF-8 file of at most 16 KiB. Choose one. |
| `--repository NAME`, `--project ID` | Approved repository and [project](projects-and-revisions.md) for a remote run. `sessions --project` lists only that project's sessions. |
| `--local` | Move the session to this machine. `move` needs `--local` or `--remote`. |
| `--model MODEL` | Model the agent added by `init` requests. Without it the agent uses its own default; `run -- --model NAME` chooses per session. |
| `--credential-file FILE`, `--credential-stdin` | Read the provider key from a file only you can read, or from standard input, instead of a hidden prompt. A key is never a command-line value. |
| `--skip-credential-check` | Do not ask the provider to check the key. |
| `--sign-in` | Let the agent sign in to its own provider account instead of storing a key. Refused with `--credential-file`, `--credential-stdin` or `--token`. |
| `--login NAME` | A named login of an agent that signs in itself, so one agent can use several accounts. `init --sign-in --login NAME` sets it up; `run --login NAME` starts a session on it. Default `default`. Lowercase letters, digits and hyphens, at most 24. A session keeps its login when it resumes or moves. |
| `--chatgpt` | Sign the gateway in to your ChatGPT plan in the browser, for an OpenAI agent such as Codex; the gateway keeps the sign-in in `credentials/chatgpt-session.json` and refreshes it. Reuses a valid stored sign-in. |
| `--token` | The stored credential is a sign-in token the gateway attaches, such as one from `claude setup-token`, rather than an API key. Skips the credential check. |
| `--provider-url URL`, `--provider-ca-file FILE` | A private provider endpoint and its PEM CA bundle, for the agent `init` adds. |
| `--instructions-file FILE` | Approved instructions for every session, at most 16 KiB, on the first `init`. |
| `--image REF`, `--build DIR` | Agent image as `REPOSITORY@sha256:DIGEST` or a local `sha256:` ID, or build it from a Sproozi source directory. |
| `--port PORT` | First port to try for the service; default 18443. |
| `--no-service` | Write the configuration without starting the service. |
| `--timeout DURATION` | Limit for `init`; default 30 minutes. |
| `--check-provider` | Make `doctor` ask each provider to check its key (lists models only). |
| `--leave-running` | Stop the service even while sessions run; they keep running until their deadline. |
| `--json` | Print JSON instead of text. |

Scripts and hand-written administrator setups may also use flags that help
does not show: `--id UUID` and `--operation UUID` (the session and the
operation to repeat), `--config FILE` (a hand-written configuration instead of
an installation, as in [session mobility](../guides/session-mobility.md)),
`--socket PATH` (the service's control socket; with it alone, no
configuration is read), and `--workspace-from`, `--checkpoint-digest` and
`--revision` for a [remote run from a recorded commit](projects-and-revisions.md).

## Agent arguments

Arguments after `--` on `run` and `resume` go to the agent program for that
execution, after the flags Sproozi sets:

```sh
sproozi run -- --model gpt-5.5 -c model_reasoning_effort=high
sproozi resume 3f2b8c1e -- --model gpt-5.5
```

They stay with the execution: `move --local` keeps them, and a later
`resume` takes its own. When they choose a model (`--model`, or Codex `-m`
and `-c model=...`), the agent uses it. The gateway still decides which
requests and models are allowed and charges them to the session's allowance.

Sproozi refuses, before creating the execution, an argument that would
replace a flag it controls, for example `harness argument "--sandbox" is
refused: Sproozi runs the harness unattended with its own sandbox and approval
mode for codex sessions; remove --sandbox`. The refused flags are:

| Agent | Refused |
| --- | --- |
| `codex` | `--sandbox`, `-s`, `--ask-for-approval`, `-a`, `--dangerously-bypass-approvals-and-sandbox`, `--yolo`, `--approve-for-me`, `--full-auto`, `--cd`, `-C`, `--worktree`, `--oss`, `--local-provider`, `--profile`, `-p`, `--remote`, `--remote-auth-token-env`, `--no-daemon`, `--no-alt-screen`; `-c`/`--config` for `developer_instructions`, `instructions`, `model_instructions_file`, `experimental_instructions_file`, `model_provider`, `model_providers`, `mcp_servers.sproozi`, `projects`, `sandbox_mode`, `sandbox_workspace_write`, `approval_policy`, `approvals_reviewer`, `check_for_update_on_startup`, `profile`, `profiles`, `notice` and keys below them |
| `claude-code` | `--permission-mode`, `--permission-prompts`, `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, `--tools`, `--allowedTools`, `--allowed-tools`, `--mcp-config`, `--strict-mcp-config`, `--plugin-dir`, `--plugin-url`, `--settings`, `--setting-sources`, `--bare`, `--safe-mode`, `--restricted`, `--system-prompt`, `--system-prompt-file`, `--append-system-prompt`, `--append-system-prompt-file`, `--resume`, `-r`, `--continue`, `-c`, `--session-id`, `--fork-session`, `--from-pr`, `--teleport`, `--no-session-persistence`, `--print`, `-p`, `--output-format`, `--input-format`, `--remote-control`, `--cloud`, `--environment`, `--bg`, `--background`, `--desktop`, `--tmux`, `--worktree`, `-w` |

A second `--` is refused too. Other words pass through, so a Codex positional
becomes its first prompt. At most 64 arguments of 4096 bytes each are
accepted. Agent arguments apply on this machine: moving such a session to a
cluster fails with `ID was started with harness arguments, which apply only at
home; it cannot move to NAME`.

## Stopping, removing and recovering

`stop` and `move` wait for the agent's current turn. After 30 seconds they
print `Waiting for the agent's current turn in session ID to finish; run
"sproozi cancel ID" in another terminal to stop it now instead.` After about
two minutes without a finished turn they fail with, for example, `the stop is
still waiting for the current Claude Code turn to finish; the session keeps
running with its terminal input paused: run "sproozi stop ID" to keep waiting,
or "sproozi cancel ID" to force-stop it` (a stuck `move` names `sproozi
recover ID` instead). The session stays `stopping`. Running `stop` again, with
any operation, continues the same stop, and `recover`, `attach` and `resume`
name `sproozi stop ID` too, so there is one way on. A session with no
conversation yet stops within a few seconds; `resume` then starts a new
conversation from the workspace it started from. A resumed session stopped
without a new prompt stops at once and keeps its conversation.

If the agent exits by itself, for example on an argument it rejects or after
Codex's `/quit`, the service notices within about five seconds and stops the
session through the same path. A nonzero exit code leaves it `failed`;
`status` prints `Exited     code N` and the last lines the agent printed, and
`attach` refuses with `session ID stopped because its agent exited with code
N; see "sproozi status ID", then "sproozi resume ID" to continue it or
"sproozi rm ID" to remove it`. Code 0 leaves it `retained`. Codex keeps the
conversation of a completed turn it reported before exiting; otherwise the
session keeps its previous checkpoint, or the workspace it started from.

A stopped session's phase is `retained`, or `failed` after its agent exited
with an error. `resume` continues either; `attach`, `move`, `cancel` and
`recover` refuse it with `session ID is stopped; "sproozi resume ID" continues
it`, or the `failed` message above. `resume` of a running session fails with
`session ID is running; attach to it with "sproozi attach ID", or stop it with
"sproozi stop ID" before resuming`; of a stopping one, with `session ID is
still stopping; run "sproozi stop ID" to finish, or "sproozi cancel ID" to
force-stop it`. `attach` refuses a session that isn't running before it
prints `Attached`. Each `resume` is a new execution, so the
`executionId` changes while `id` and `threadId` stay the same.

`rm ID` deletes a `retained`, `failed` or `cancelled` session: its record, its executions'
journals, and the saved workspaces, checkpoints and profiles no other session
uses. It keeps the accounting and grant records, and the service's record of
each finished container. It refuses:

| Reply | Cause | Run |
| --- | --- | --- |
| `session ID is running; stop it with "sproozi stop ID" or "sproozi cancel ID" first` | The session has a running agent. | `sproozi stop ID` or `sproozi cancel ID`, then `rm`. |
| `session ID has unfinished work (PHASE); run "sproozi recover ID", then remove it` | A stop, move or cancellation was interrupted, or its usage is not settled. | `sproozi recover ID`. |
| `session ID is still being pulled; finish with "sproozi pull ID" before removing it` | A pull has not finished. | The same `pull` command. |
| `another operation is in progress; run the command again shortly` | Another command held the service's records for more than five seconds. | `rm` again. |

When an earlier operation blocks a command, the service refuses it and names
the command that unblocks it. Nothing was recorded, so there is no retry
hint. `ID` is the session and `OP` an operation.

| Reply | Cause | Run |
| --- | --- | --- |
| `session: previous KIND requires recovery; run "sproozi recover ID" to finish it, or "sproozi cancel ID" to stop it now` | A `move` or `stop` while an earlier `start`, `resume` or `move` is unfinished. | `sproozi recover ID`, or `sproozi cancel ID`. |
| `session ID is still stopping; run "sproozi stop ID" to keep waiting, or "sproozi cancel ID" to force-stop it` | A `move` while a stop is unfinished. | `sproozi stop ID`, or `sproozi cancel ID`. |
| `session: cancellation unfinished; run "sproozi recover ID" to finish the cancellation` | A `move` or `stop` while a cancellation hasn't yet proved the agent stopped and its usage settled. | `sproozi recover ID`. |
| `session: the writer stopped, but its usage is not finalized yet; recover again once the placement can finalize it` | `recover` of a cancellation whose usage could not be settled yet. | `sproozi recover ID` again; the service log has the cause. |
| `session: the placement has not yet proved the writer stopped; recover again once it is reachable` | `recover` of a cancellation that could not confirm the agent stopped. | Restore Docker or the cluster, then `sproozi recover ID`. |
| `session ID was cancelled, so it cannot be stopped or resumed; remove it with "sproozi rm ID", and start new work with "sproozi run"` | `stop`, `resume`, `move` or `attach` of a cancelled session. Nothing can continue it. | `sproozi rm ID`. |
| `session ID is being cancelled; run "sproozi recover ID" to finish the cancellation, then "sproozi rm ID" to remove it` | `resume` while a cancellation is unfinished. | `sproozi recover ID`. |

When a Docker command the service runs fails, the error includes Docker's
standard error, cut to its first 4096 bytes and marked ` [output truncated]`.

## Workspace problems

`run` copies the repository at `PATH` into the session. It refuses, with no
retry hint:

| Reply | Run |
| --- | --- |
| `workspace: PATH does not exist; run it on the top-level directory of a Git repository` | `sproozi run` in, or on, a Git repository. |
| `workspace: ordinary Git directory required; PATH is inside the repository at TOP; try: sproozi run TOP` | Run it on the repository's top level, as it says. Elsewhere it says `PATH must be the top-level directory of a Git repository, holding its .git directory (linked worktrees and submodules are unsupported)`. |
| `workspace: ignored file NAME does not exist in the repository`, or `NAME is not ignored by Git, so it is copied already; leave it out of --include-ignored` | Correct or drop that `--include-ignored`. |
| `workspace: the repository has no commits; make a first commit in PATH, then run again` | `git commit`, then `sproozi run`. |
| `workspace: byte limit exceeded: expanded Git objects total N MiB, and M MiB remains after working files; local.maxCheckpointBytes is L MiB: raise it ...` | Raise `local.maxCheckpointBytes` in `config.json` (at most 64 MiB, with `local.retainedBytes` at least twice it), then `sproozi service stop` and `sproozi service start`. |

## Remote commands

`pull`, and `run`, `move`, `sessions`, `status`, `cancel` and `recover` with
`--remote`, reach a [remote executor](../../GLOSSARY.md#remote-executor) in a Kubernetes cluster.
`--context` names a kubeconfig context, or a kubeconfig file whose current
context is used; without it, the context named `sproozi`. A context name wins
over a file of the same name. `--namespace` overrides the context's namespace.
`--context` or `--namespace` alone implies `--remote`.
[Remote executor access](remote-executors.md) shows how to create the
`sproozi` context and lists every error with its fix.

`move --remote` and `pull` hand the selection to the service, which reaches the
cluster itself and remembers the executor, so it keeps working after the
command exits. `move ID --local` needs no context. A `pull` the service already
began continues from the same executor when you repeat it.

`sessions` without `--remote` lists this machine and, when the `sproozi`
context exists, its cluster; an unreachable cluster is reported by its context
on standard error. A remote `status` prints the session's phase, problem and its untrusted final text;
`--json` prints the remote status document.

## Status and listing output

`status ID` prints the session, harness, phase, placement and, while the
session runs, its deadline. `sessions` prints a table with `ID` (the first 8
characters), `HARNESS`, `PHASE`, `PLACEMENT` and `DEADLINE`, plus `LISTED BY`
when rows come from more than one catalogue. A stopped or cancelled session
shows `-` for the deadline, because its last execution's deadline no longer
applies.

With `--json`, `status` and the session commands print the session:

| Field | Meaning |
| --- | --- |
| `id` | Session ID. |
| `executionId` | Current or last execution. A `resume` gets a new one. |
| `allocationId` | Current or last visit to a placement. |
| `incarnation` | The backend's identity for the exact container or Pod. |
| `threadId` | The harness conversation ID. It is not a command operand or a credential. |
| `harness`, `phase`, `deadline` | Agent, lifecycle phase and the current execution's deadline. |
| `placement` | Where it runs or last ran: `local` for this machine, or a cluster's placement name. |
| `ownership` | Who owns it; see below. |
| `session` | The full execution journal, for detailed inspection. `initialStage` repeats `phase`. |

`sessions --json` writes one document with `sessions`, `executors` and
`truncated`. It asks at most 32 catalogues at once, five seconds each, and
returns at most 512 rows sorted by catalogue and ID.

```json
{
  "sessions": [],
  "executors": [
    {"executor": "engineering", "availability": "available"},
    {"executor": "local", "availability": "unavailable", "error": "service_not_running"}
  ],
  "truncated": false
}
```

An unavailable catalogue's `error` is `service_not_running` when this
machine's service is not running, otherwise one of the
[work outcome codes](#handle-work-outcomes). Rows from catalogues that
answered are still listed and the command succeeds, naming each one that did
not on standard error. A cluster that could not be reached at all has
`context` in place of `executor`, and standard error says `Not listed: cluster
(context sproozi) is unreachable: …` with the cause and its fix. When none
answers, `sessions` fails with the reason.

| `executor` | `ownership` | Meaning |
| --- | --- | --- |
| `local` | `local` | This machine owns the session: you ran it here, or a pull finished. |
| `local` | `pulling` | A pull is under way; run the same `pull` again to finish it. |
| a cluster, such as `engineering` | `cluster` | That cluster owns it. Use the command with `--context` for that cluster, or pull it. |
| a cluster | `local` | The cluster's record of a session pulled to a machine, in phase `pulled`. Use the `local` row. |

## Handle work outcomes

A cluster returns one of these error codes. The CLI shows the code and HTTP
status, never upstream error bodies or private transport details.

If status says `unavailable`, Sproozi can't yet confirm that the agent stopped
or how much it used. Run `recover` before starting new work.

| Code | HTTP status | Next step |
| --- | --- | --- |
| `invalid_request` | 400 | Check the session ID and the task options. |
| `denied` | 403 | Check credentials and the approved repository and capabilities. |
| `conflict` | 409 | Check status and retry the original operation. |
| `expired` | 410 | Check status before requesting new work. |
| `unsupported` | 422 | Check agent support and checkpoint compatibility. |
| `unavailable` | 503 | Restore connectivity or storage, check status, then retry the same operation. |

Neither `unavailable` nor `unsupported` lets you start the work elsewhere,
refunds usage or extends the deadline. Sproozi never treats an unknown
filesystem or transport error as a rejection or as proof that nothing is
running.
