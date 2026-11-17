# Verify Claude Code on the cluster

This driver proves, on a dedicated Kind cluster, that the
[Claude Code executor example](../../../examples/portable-workflows/kubernetes/claude/kustomization.yaml)
runs stock Claude Code sessions and tasks that the remote executor owns
([ADR 0057](../../../docs/adr/0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
sections 10 and 11). It drives the public `sproozi` CLI through the
`sproozi-cli` context only, with the stock client in its terminal through the
production PTY. It has not been run yet; nothing here is recorded evidence.

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
| `SPROOZI_TOKEN_REFRESH=1` | Runs only the `refresh` scenario, on a cluster whose API server caps ServiceAccount tokens at 3607 seconds (`--service-account-max-token-expiration`, its lowest allowed value). `run.sh` checks the cap is in force and raises the example's 30-minute executor and policy duration caps to 90 minutes. It takes about an hour longer. |

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

The `refresh` scenario runs only when chosen, as `SPROOZI_TOKEN_REFRESH=1`
does. It proves a session keeps the gateway after the gateway token its
sandbox Pod started with has expired: the kubelet's refreshed token reaches the
mounted token directory, and the session's gateway forwarder sends it. It
starts a session, checks the sandbox's `/var/run/sproozi/tokens/gateway` holds
only `token`, `..data` and the timestamped directory `..data` names, takes a
turn, waits by the Pod's clock until a minute after the first token's expiry,
and takes a second turn that needs the first in its history. Both turns run in
the same Pod. Only the token's `iat`, `exp` and `jti` claims and the
directory's entries leave the Pod.

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

<!--
Proposed CI job, not enabled. It would fit beside test-e2e in
.github/workflows/test-e2e.yml, with the same pinned actions, and it would add
roughly 25 to 40 minutes: the client images build git and install the pinned
clients. Enable it once a local run has passed.

  verify-claude-remote-kind:
    permissions:
      contents: read
    runs-on: ubuntu-latest
    timeout-minutes: 60
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
        with:
          persist-credentials: false
      - uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0
        with:
          go-version: '1.26.11'
      - name: Install pinned Kind
        run: |
          hack/verify/install-kind.sh "$RUNNER_TEMP/kind-bin"
          sudo install -m 0755 "$RUNNER_TEMP/kind-bin/kind" /usr/local/bin/kind
      - name: Download and verify pinned Calico manifest
        run: hack/verify/fetch-calico.sh "$RUNNER_TEMP/calico-v3.30.4.yaml"
      - name: Install PyYAML
        run: sudo apt-get install -y --no-install-recommends python3-yaml
      - name: Prove Claude Code on the cluster
        run: make verify-claude-remote-kind
        env:
          SPROOZI_CNI_MANIFEST: ${{ runner.temp }}/calico-v3.30.4.yaml
          SPROOZI_CNI_TIMEOUT: 300s
      - name: Retain evidence
        if: always()
        uses: actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02 # v4.6.2
        with:
          name: claude-remote-kind-evidence
          path: |
            .local/verification/claude-remote-kind/*/proof/
            .local/verification/claude-remote-kind/*/*.log
          include-hidden-files: true
          if-no-files-found: warn
          retention-days: 7
-->
