# Getting started

Sproozi runs an agent in a disposable Kubernetes Pod. The agent reaches approved
services through a shared gateway, which checks the run's identity and policy.
Provider credentials stay in the gateway.

Choose the path you want:

- **Explore locally:** follow the Kind demo below. Codex investigates a broken
  workload and opens a pull request in your disposable GitHub repository.
- **Use an existing Kubernetes cluster:** follow [Install on Kubernetes](install-kubernetes.md).
  Start with a Pod that lists other Pods through the gateway. That first check
  needs no model account or GitHub App.

Sproozi is a preview. Start in an evaluation environment and read the
[deployment limitations](../reference/security-hardening.md) before using it for
other workloads. The demo proposes a correction; it does not merge the PR or
repair the running workload.

## What you will create

The Kind demo creates a dedicated two-node cluster with Calico for NetworkPolicy
enforcement, Sproozi's controller and gateway, and a deliberately failing
workload. It submits one `AgentRun` and checks the resulting PR, policy denials
and sandbox cleanup.

These four resources describe a run:

| Resource | Purpose |
| --- | --- |
| `AgentRuntime` | Fixes the image, command and Pod security settings. |
| `AgentPolicy` | Limits capabilities, resource access, consumption and lifetime. |
| `AgentTemplate` | Selects a runtime and policy and supplies trusted instructions. |
| `AgentRun` | Requests one task and a subset of the policy's capabilities. |

The first three are administrator-owned. Creating a run does not let the caller
choose a different image or grant itself more access.

## 1. Prepare your computer

Use macOS or Linux, or a Linux shell under WSL2. Install the following tools and
make sure they are on your `PATH`:

- [Docker](https://docs.docker.com/get-started/get-docker/) with a running engine.
  On macOS, start Docker Desktop.
- [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/) and
  [kubectl](https://kubernetes.io/docs/tasks/tools/). The demo pins Kubernetes
  1.33.1, so use kubectl 1.32, 1.33 or 1.34, within Kubernetes' supported
  [version skew](https://kubernetes.io/releases/version-skew-policy/#kubectl).
- Git and the [GitHub CLI](https://cli.github.com/).
- [Go 1.26.6](https://go.dev/dl/), Make, OpenSSL, Python 3, curl and `shasum`.
  Go 1.26.6 is the repository's CI toolchain.

Check Docker and sign in to GitHub:

```sh
docker info
gh auth login
gh auth status
```

Clone the repository and run the rest of this guide from its root:

```sh
git clone https://github.com/andrewmccall/sproozi.git
cd sproozi
mkdir -p .local
```

If your host has a newer kubectl, install the demo-compatible client separately
and put it first on `PATH`. On macOS or Linux with an AMD64 or ARM64 CPU:

```sh
mkdir -p .local/bin
SPROOZI_KUBECTL_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
SPROOZI_KUBECTL_ARCH=amd64
case "$(uname -m)" in arm64|aarch64) SPROOZI_KUBECTL_ARCH=arm64 ;; esac
SPROOZI_KUBECTL_URL="https://dl.k8s.io/release/v1.34.1/bin/$SPROOZI_KUBECTL_OS/$SPROOZI_KUBECTL_ARCH/kubectl"
curl --fail --location "$SPROOZI_KUBECTL_URL" -o .local/bin/kubectl
curl --fail --location "$SPROOZI_KUBECTL_URL.sha256" -o .local/bin/kubectl.sha256
printf '%s  %s\n' "$(cat .local/bin/kubectl.sha256)" .local/bin/kubectl | shasum -a 256 -c -
chmod +x .local/bin/kubectl
export PATH="$PWD/.local/bin:$PATH"
kubectl version --client
```

You also need either an OpenAI API key with access to the demo's `gpt-6.1-sol`
model or a ChatGPT plan that supports the repository's sign-in flow. The
credential wizard lets you choose. API-key mode incurs API usage charges;
ChatGPT mode consumes your plan's allowance. See
[model provider authentication](model-provider-auth.md) for the details.

### Cluster dependencies and external CRDs

You need Docker and the host tools above before starting. `make demo` installs
the cluster components below automatically in its dedicated Kind cluster.

| Dependency | Needed for this demo? | How it is supplied |
| --- | --- | --- |
| Sproozi CRDs: `AgentRuntime`, `AgentPolicy`, `AgentTemplate`, `AgentRun` | Yes | `make install` installs the four definitions from this checkout. |
| NetworkPolicy-enforcing CNI | Yes | Setup downloads pinned Calico 3.30.4. The demo installs its controller components, `crd.projectcalico.org` CRDs, and its bundled `AdminNetworkPolicy` and `BaselineAdminNetworkPolicy` definitions under `policy.networking.k8s.io`. |
| External Kubernetes Agent Sandbox controller and CRDs | No | The current Sproozi adapter creates ordinary Pods in `sproozi-agents`. |
| cert-manager and its CRDs | No | The demo creates its gateway certificate with OpenSSL. |
| Prometheus Operator and monitoring CRDs | No | Only the optional monitoring overlays need an existing monitoring installation. |
| gVisor, Kata Containers or a custom `RuntimeClass` | No | The demo uses Kind's default container runtime. |
| Model account and GitHub App | Yes for the PR demo | You configure these in steps 2 to 4. They are external services, not cluster CRDs. |

Here, "sandbox" means Sproozi's disposable agent Pod and its run-scoped
identity, network policy and input contract. It does not mean the `Sandbox`
resource from the separate
[Kubernetes Agent Sandbox project](https://github.com/kubernetes-sigs/agent-sandbox).
You do not need that project's `Sandbox`, `SandboxTemplate`, `SandboxClaim` or
`SandboxWarmPool` CRDs or controllers. Installing them does not switch Sproozi
to that adapter; integration is planned.

Calico's pinned manifest is for this disposable Kind cluster. Sproozi requires
an enforcing CNI, rather than Calico specifically. The
[manual demo guide](manual-pr-demo.md) describes custom CNI inputs and optional
monitoring. The base demo needs no external sandbox operator, admission webhook
certificate manager or monitoring stack.

### Optional: check dependencies before configuring providers

From the repository root, you can first prove that Kind, the controller and
NetworkPolicy enforcement work without a model account or GitHub App:

```sh
python3 hack/verify/quickstart.py doctor
python3 hack/verify/quickstart.py dependencies
```

This runs the repository's real Kind gate with cert-manager and optional metrics
disabled. It checks a working connection, blocks it with deny-egress, then
restores it after removing the policy. It observes the installed CRDs and
removes its temporary cluster. Logs and `result.json` remain under
`.local/verification/quickstart/`. This check does not run Codex or open a PR.

## 2. Create the disposable GitHub repository

The agent will push a branch and open a real PR here. Use an unused repository
name. These commands create a private repository with the expected file on its
`main` branch:

```sh
export SPROOZI_DEMO_REPOSITORY="$(gh api user --jq .login)/sproozi-demo"
git init --initial-branch=main .local/demo-repository
cp examples/sre-demo/demo-workload.yaml .local/demo-repository/demo-workload.yaml
git -C .local/demo-repository add demo-workload.yaml
git -C .local/demo-repository commit -m "Add the failing Sproozi demo workload"
gh repo create "$SPROOZI_DEMO_REPOSITORY" --private \
  --source=.local/demo-repository --remote=origin --push
```

If Git asks for your identity, configure your author name and email and repeat
the commit. If you already have a disposable repository, use that instead and
put `demo-workload.yaml` from the example at its root on `main`. The automatic
setup does not create or seed a repository.

## 3. Create a GitHub App

Sproozi uses a GitHub App installation to access the disposable repository.
Your host's `gh` login lets the demo inspect the PR; it is a separate credential
and never goes into the agent Pod.

Follow GitHub's [App registration instructions](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app):

1. Open your account's **Settings**, **Developer settings**, **GitHub Apps**,
   then **New GitHub App**.
2. Give it a unique name and use the Sproozi repository URL as its homepage.
   Leave the callback URL empty and disable the App's webhook by clearing **Active**.
3. Set repository permissions for **Contents** and **Pull requests** to
   **Read and write**. Keep the other permissions at their defaults.
4. Create the App. Copy its **App ID**, then generate and download a private key.
5. Select **Install App**, install it on your account, and select only the
   disposable demo repository. Copy the installation ID from the installation
   page's URL, the number after `/installations/`.

Keep the App ID and installation ID ready for the next step. Save the downloaded
private key in the checkout's gitignored local directory. Replace the download
path below with the file GitHub gave you:

```sh
mkdir -p .local/credentials
chmod 700 .local/credentials
install -m 600 "$HOME/Downloads/your-github-app.private-key.pem" \
  .local/credentials/github-app.pem
```

The wizard will ask for the absolute path, which is
`$PWD/.local/credentials/github-app.pem` from the repository root. It saves that
path, rather than copying the key into the environment file.

## 4. Configure the demo

```sh
export IMG=controller:sproozi-demo
make demo-setup
bash hack/demo/configure.sh
make demo-check
```

`make demo-setup` downloads and verifies the pinned Calico manifest and builds
a tools image containing stock Codex, kubectl, Git and gh. It starts a local
Docker registry on `127.0.0.1:5001` and pins the tools image by digest. The first
build can take several minutes. No external image registry is required.

### Save the credentials

Run `bash hack/demo/configure.sh` in your own terminal. The wizard saves each
answer immediately in gitignored `.local/demo.env` with permissions `0600`,
so you can interrupt it and resume. Enter these values at its prompts:

| Prompt | What to enter | Where it is saved |
| --- | --- | --- |
| Authentication mode | `api_key` or `chatgpt` | `MODEL_AUTH_MODE` in `.local/demo.env`. |
| OpenAI API key, for `api_key` mode | Your provider API key at the hidden-input prompt | `OPENAI_API_KEY` in `.local/demo.env`. |
| Browser sign-in, for `chatgpt` mode | Complete the browser consent flow | Private `.local/chatgpt-session.json` and `.local/chatgpt-host.json`; the session path is saved as `CHATGPT_SESSION_FILE` in `.local/demo.env`. |
| GitHub App ID | The App ID from the App settings page | `GITHUB_APP_ID` in `.local/demo.env`. |
| GitHub App installation ID | The ID from the installation URL | `GITHUB_APP_INSTALLATION_ID` in `.local/demo.env`. |
| Local private-key file path | The full path to `.local/credentials/github-app.pem` | `GITHUB_APP_PRIVATE_KEY_FILE` in `.local/demo.env`; the key stays in its own file. |

Choose one model authentication mode. The wizard opens the API-key page when
needed or starts the ChatGPT sign-in helper. Setup also saves the repository,
Codex image digest and CNI configuration. You do not need to construct the
environment file yourself or put secrets into example YAML. Keep credential
values in your terminal rather than chat, shell command arguments or commits.

Check the saved file permissions without printing their contents:

```sh
ls -l .local/demo.env .local/credentials/github-app.pem
# In ChatGPT mode, also check:
# ls -l .local/chatgpt-session.json .local/chatgpt-host.json
python3 hack/verify/quickstart.py doctor --env-file "$PWD/.local/demo.env"
```

The private files should have permissions `-rw-------`. The doctor reports
missing variable names without printing credential values. `make demo-check`
also checks prerequisites and the configured files. Neither launches the agent.

The demo loads these saved settings automatically on later runs. To replace
credentials, edit `.local/demo.env` in a local editor, remove the settings you
want the wizard to ask for again, and rerun it. The wizard keeps existing values.
For an independent configuration, set `SPROOZI_DEMO_ENV_FILE` to an absolute
gitignored path before running setup and the wizard; pass that same file to the
verification helper's `--env-file` option.

For ChatGPT mode, each session belongs to one active gateway. Before launching
another cluster, renew consent with `bash hack/demo/chatgpt-login.sh --reauthorize`.
The local bootstrap file does not receive the live gateway's token refreshes.
See [model provider authentication](model-provider-auth.md) for the lifecycle.

`make demo-check` should end with:

```text
Demo prerequisites and private configuration are available
```

## 5. Run it

```sh
make demo IMG="$IMG" KIND_CLUSTER=sproozi-demo
```

This command creates the isolated cluster, installs Calico, builds and loads
the Sproozi image, creates gateway credentials and trust material, deploys the
example resources, and submits the run. The controller creates an agent Pod in
`sproozi-agents` and starts a noninteractive `codex exec` session inside it.
Codex investigates the workload and creates a real GitHub PR using the shared
gateway. The command then waits for the acceptance checks,
up to 20 minutes by default. Keep the local registry running during the demo.

The command uses a dedicated kubeconfig. It refuses to reuse a cluster named
`sproozi-demo`; choose a new name or delete your previous demo cluster before
trying again.

To watch while it runs, open another terminal in the repository root:

```sh
export KUBECONFIG="$PWD/.local/demo/sproozi-demo.kubeconfig"
kubectl get pods -n sproozi-system
kubectl get pods -n sproozi-demo
kubectl get agentruns -n sproozi-system -w
```

The target workload is deliberately broken. Its failing Pod is expected.
The agent Pod appears in `sproozi-agents` and is removed when its run ends.

## 6. Inspect the result and explore

After `make demo` finishes, select the demo context in your terminal:

```sh
export KUBECONFIG="$PWD/.local/demo/sproozi-demo.kubeconfig"
ls -t .local/verification/live-*.json
gh pr list --repo "$SPROOZI_DEMO_REPOSITORY" --state open
kubectl get agentruntimes,agentpolicies,agenttemplates -n sproozi-system
kubectl get crds
kubectl get agentpolicy sre -n sproozi-system -o yaml
kubectl logs -n sproozi-system deployment/sproozi-gateway --tail=50
```

Open the newest report with `python3 -m json.tool <report-path>`. A passing report
contains a real PR URL, the run identity, allowed and denied operations, model
usage and the cleanup result. Review the PR's diff. The expected correction is
limited to `demo-workload.yaml`.

The acceptance script deletes its run after collecting evidence. The cluster,
templates, broken workload and PR remain so you can explore. To submit another
run yourself:

```sh
kubectl create -f examples/sre-demo/manual-agentrun.yaml
kubectl get agentruns -n sproozi-system -w
```

This creates another agent execution and can create another PR. Press Ctrl-C
to leave the watch; that does not cancel the run. While a run is active, inspect
its status, Pod and NetworkPolicy. For the sample run above:

```sh
kubectl get agentrun manual-sre-remediation -n sproozi-system -o yaml
kubectl get pods,serviceaccounts,networkpolicies -n sproozi-agents
```

To cancel it:

```sh
kubectl patch agentrun manual-sre-remediation -n sproozi-system \
  --type=merge -p '{"spec":{"cancel":true}}'
```

Run success means the `agent` container exited with code zero. PR creation is a
separate acceptance check. To repeat a manual run, delete its terminal
`AgentRun` first or give the new run a different name. See the
[manual demo guide](manual-pr-demo.md) for probes, optional monitoring and MCP links.

## If something goes wrong

| Symptom | What to check |
| --- | --- |
| Docker is unavailable | Start the engine and rerun `docker info`. |
| Repository already exists | Use another disposable name, or seed the existing repository's `main` branch. |
| Missing configuration | Run `bash hack/demo/configure.sh`, then `make demo-check`. |
| Cluster already exists | Inspect the old cluster, then choose a new `KIND_CLUSTER` or remove that demo cluster. |
| Image pull fails | Check `docker ps -a --filter name=sproozi-demo-registry`. Restart it with `docker start sproozi-demo-registry`. |
| Gateway does not start | Read `kubectl logs -n sproozi-system deployment/sproozi-gateway` in the demo context. Check credentials and certificate errors. |
| Model access or PR creation fails | Check the report, model authentication, and App installation permissions on the selected repository. |

If setup fails before the final message, the isolated kubeconfig is still at
`.local/demo/<cluster-name>.kubeconfig`. Export it before debugging. A failing
provider call is not proof of a policy denial; use the report's acceptance result.

## Clean up

When you have finished exploring:

```sh
kind delete cluster --name sproozi-demo
docker stop sproozi-demo-registry
unset KUBECONFIG
```

These commands keep the local configuration, image cache and GitHub repository.
Close the demo PRs and remove the disposable repository and App installation
through GitHub when you no longer need them. Before another local demo, restart
the registry with `docker start sproozi-demo-registry`.

## Repeatable verification for contributors

The repository includes the
[`verify-sproozi` skill](../../.agents/skills/verify-sproozi/SKILL.md) for checking
this Kind guide. After configuring your demo environment, run:

```sh
python3 hack/verify/quickstart.py doctor --env-file "$PWD/.local/demo.env"
python3 hack/verify/quickstart.py demo --env-file "$PWD/.local/demo.env"
```

The helper runs the same `make demo` path against an isolated copy of the current
checkout, so deployment-time manifest edits stay out of your working tree. It
retains the command log, source and guide hashes, observed CRDs and live report,
then removes its owned cluster and scratch files. This command consumes model
usage and creates a real branch and PR. Interactive App creation and sign-in
remain the setup steps above.

Add `--keep-cluster` to inspect a passing demo. Set
`SPROOZI_QUICKSTART_EVIDENCE` to the exact directory printed by the helper, then:

```sh
export KUBECONFIG="$SPROOZI_QUICKSTART_EVIDENCE/cluster.kubeconfig"
python3 hack/verify/quickstart.py doctor --evidence "$SPROOZI_QUICKSTART_EVIDENCE"
```

Use the exploration commands in step 6. Finish with:

```sh
python3 hack/verify/quickstart.py cleanup "$SPROOZI_QUICKSTART_EVIDENCE"
unset KUBECONFIG
```

Only one helper invocation can drive the shared local Docker images and registry
at a time. A retained demo holds its lock until cleanup. For ChatGPT mode, use a
newly authorised session that no other gateway is refreshing. Keep the generated
proof under `.local`; keep the skill and its feature map current with
`/maintain-verification-skill`.
