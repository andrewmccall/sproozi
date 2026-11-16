# Verify Claude Code on the cluster

This driver proves, on a dedicated Kind cluster, that the
[Claude Code executor example](../../../examples/portable-workflows/kubernetes/claude/kustomization.yaml)
runs stock Claude Code sessions and tasks that the remote executor owns
([ADR 0057](../../../docs/adr/0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
sections 10 and 11). It drives the public `sproozi` CLI through the
`sproozi-cli` context only, with the stock client in its terminal through the
production PTY. CI runs it on every pull request and push to main (E2E Tests,
"Claude Code on the cluster"); it first passed locally at `f5a7892`, with the
stop scenarios repeated four more times.

## Run it

From the repository root:

```sh
make verify-claude-remote-kind
```

It needs Docker with Buildx, Kind, kubectl, Go, Git, OpenSSL and Python 3 with
PyYAML, and network access to pull the base images, the pinned clients and the
Calico manifest. It takes the Kind node image from the Makefile. It builds the
CLI, the PTY helper and this checkout's native and control images, creates its
own `sproozi-mobility-claude-<run>` cluster with Calico, runs every scenario,
and deletes the cluster. Evidence stays in
`.local/verification/claude-remote-kind/<run>/`: `proof/report.json`, each CLI
command's output, the terminals' text, and the executor, gateway, controller
and provider logs. Private keys are removed at the end.

| Variable | Effect |
| --- | --- |
| `SPROOZI_CLAUDE_AUTH=gateway-key` | Removes the example runtime's `signIn` and gives the gateway the fixture's Anthropic key, as the example's comment describes. Default `sign-in`. |
| `SPROOZI_PRESERVE_ON_FAILURE=1` | Keeps a failed run's cluster; its kubeconfig is in the run directory. |
| `SPROOZI_NO_CACHE=1` | Rebuilds the client layers without the Docker cache. |
| `SPROOZI_CNI_MANIFEST` | A Calico manifest already fetched with `hack/verify/fetch-calico.sh`. |

Arguments after the script name go to `verify.py`; for example
`bash hack/verify/claude-remote-kind/run.sh --scenario task` runs one scenario
(`KIND_NODE_IMAGE` must then be set, as the Makefile does).

## What it proves

Each scenario is its own session in `sproozi-claude-sessions`, started with
`sproozi run --remote --namespace sproozi-claude-sessions --harness claude-code
--repository sproozi`:

| Scenario | A pass proves |
| --- | --- |
| `snapshot` | Started from the approved repository, the Pod reaches Claude Code's prompt with no onboarding, folder-trust, bypass-permissions or API-key dialog. One turn completes through `attach`; `stop --remote` binds the conversation and retains a Claude checkpoint. `resume --remote` continues it: the provider refuses a request that lacks the first turn, and the second stop keeps the same conversation ID. |
| `upload` | Started from an uploaded `PATH`, the prompt has no dialog, and Claude Code's Bash reads a file only the upload holds. `/exit` after that turn is `retained` within `--exit-timeout` (30 seconds: the executor's 15-second exit sweep, then the stop), never `unknown-stop`, and the first visit keeps no conversation. |
| `exit` | `/exit` before any turn is `retained` within the same bound, without `unknown-stop` or a conversation. |
| `resumed` | `/exit` in a resumed visit is `retained` within the same bound, keeping the conversation and checkpoint the visit started from. |
| `task` | `sproozi run --remote ... --task` completes unattended: `retained` with a Claude checkpoint, the prompt submitted once, and the result Claude's final reply. |
| `tools` | A turn calls `mcp__sproozi__diagnose` through the gateway's control connection and loads the profile skill `profile:remote-check` with the `Skill` tool, then `stop` retains it. The diagnose result and the skill's body, which alone holds its marker, reach the model. |

In the default `sign-in` mode every model request must carry the fixture
sign-in's token, so the runtime's login claim, not a gateway key, signed
Claude Code in.

## How the fixture differs from a real installation

Only test inputs are substituted ([`render.py`](render.py)):

- **Model provider.** [`provider.py`](provider.py) is a deterministic Anthropic
  Messages fixture running in the cluster, in `sproozi-fixture`, on the native
  image's Python. The gateway's `ANTHROPIC_UPSTREAM_URL` points at it. Each turn
  is chosen by the marker in its prompt, and a turn that needs earlier history
  is refused without it. Its request log holds no bodies.
- **Sign-in.** The example's login claim, `sproozi-login-claude-code-default`
  in `sproozi-agents`, is created before the first session and seeded with a
  placeholder `.credentials.json`, as `/login` would leave it. `/login` itself
  is not exercised.
- **Repository.** The executor approves an offline Git snapshot that an init
  container seeds, instead of the example's GitHub URL.
- **Trust and images.** A fresh CA signs one leaf for the gateway, inspected
  model hosts, the executor and the provider. The images are this checkout's,
  imported by digest.
- The template, instructions, policy, roles, network policy, executor
  configuration otherwise and CLI access are the example's.

Not proven: the Anthropic service, a paid model, `/login`, compaction, or
containment probes (the [session movement driver](../session-mobility/README.md)
covers those for Claude Code).

## Offline checks

`make verify-workflow-fixtures` runs [`provider_test.py`](provider_test.py)
and [`render_test.py`](render_test.py): the fixture's scripted turns, and that
the render keeps the shipped example's sign-in, template and network policy.

