# Getting started

Sproozi runs an agent in a disposable Kubernetes Pod. The agent reaches approved
services through a shared gateway, which checks the run's identity and policy.
Provider credentials stay in the gateway.

This guide has two parts:

1. **Check your setup, no accounts needed** (steps 1 and 2). Build Sproozi,
   deploy it to a throwaway Kind cluster and prove that its network enforcement
   works.
2. **Run the investigation-to-PR demo** (steps 3 to 8). Codex investigates a
   broken workload and opens a pull request in your own disposable GitHub
   repository. This needs a model account and a GitHub App.

Everything here runs from a clone of this repository with Make. You don't need
the `sproozi` CLI.

Already have a cluster? Follow
[Install on Kubernetes and run your first Pod](install-kubernetes.md) instead.
Its first agent Pod lists other Pods through the gateway and needs an image
registry, but no model account or GitHub App. Its last part adds model and
GitHub credentials and runs this guide's investigation-to-PR demo on your cluster.

Sproozi is a preview. Start in an evaluation environment and read the
[deployment limitations](../reference/security-hardening.md) before using it for
other workloads. The demo proposes a fix; it does not merge the PR or repair
the running workload. If the resource names are new to you, read
[core concepts](../explanations/core-concepts.md) first.

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
- [Go](https://go.dev/dl/) 1.26 or later, Make, OpenSSL,
  Python 3, curl and `shasum`.

Clone the repository and run the rest of this guide from its root:

```sh
git clone https://github.com/andrewmccall/sproozi.git
cd sproozi
mkdir -p .local
```

Check Docker and your kubectl version:

```sh
docker info
kubectl version --client
```

If `kubectl version --client` reports a version other than 1.32, 1.33 or 1.34,
or kubectl isn't installed, install the demo-compatible client separately
and put it first on `PATH`. On macOS or Linux with an AMD64 or ARM64 CPU:

```sh
mkdir -p .local/bin
SPROOZI_KUBECTL_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
SPROOZI_KUBECTL_ARCH=amd64
case "$(uname -m)" in arm64|aarch64) SPROOZI_KUBECTL_ARCH=arm64 ;; esac
SPROOZI_KUBECTL_URL="https://dl.k8s.io/release/v1.34.1/bin/$SPROOZI_KUBECTL_OS/$SPROOZI_KUBECTL_ARCH/kubectl"
curl --fail --location "$SPROOZI_KUBECTL_URL" -o .local/bin/kubectl
SPROOZI_KUBECTL_SHA256="$(curl --fail --location --silent --show-error "$SPROOZI_KUBECTL_URL.sha256")"
printf '%s  %s\n' "$SPROOZI_KUBECTL_SHA256" .local/bin/kubectl | shasum -a 256 -c - &&
  chmod +x .local/bin/kubectl
export PATH="$PWD/.local/bin:$PATH"
kubectl version --client
```

The export lasts only for this terminal. In each new terminal, including the
one you open to watch the demo in step 7, run
`export PATH="$PWD/.local/bin:$PATH"` again from the repository root.

## 2. Check your setup without any accounts

Run:

```sh
python3 hack/verify/quickstart.py doctor
python3 hack/verify/quickstart.py dependencies
```

`doctor` checks that Docker, Kind, kubectl, Git, Go, Make, OpenSSL, curl and
`shasum` are on your `PATH`. It enforces two versions: kubectl must be 1.32,
1.33 or 1.34, and Go must be at least the version `go.mod` declares. It also
asks the Docker engine for its version, which fails if the engine isn't
running, and prints the Docker server and Kind versions without checking them.
It doesn't check the versions of Git, Make, OpenSSL, curl or `shasum`. It runs
under Python 3, so it doesn't check for it separately. It checks for `gh` only
in step 6, when you pass `--env-file`. It runs every check, even after one
fails, and prints an `ok` line as each one passes; the Go line names the
minimum version `go.mod` requires. A tool that is missing from `PATH` is
reported once in `Missing tools: …`, and its version check is skipped. It ends
with `Doctor checks passed: host tools and versions`, or with every failure in check order, so you can
fix them all before rerunning. One failure stays on the same line as
`Doctor check failed: `. With several, `Doctor check failed:` stands alone and
each failure follows on its own line, indented by two spaces. An unsupported
kubectl version fails with this message, which names the version found and
points to step 1 above:

```text
Use kubectl 1.32, 1.33 or 1.34 for the pinned Kubernetes 1.33.1 demo (found 1.37.1); see "Prepare your computer" in docs/guides/getting-started.md to install a matching client into .local/bin
```

`dependencies` builds the real controller image and deploys it to a temporary
Kind cluster with Calico. It then proves that the network rules work: a test
connection succeeds, a deny-egress policy blocks it, and removing the policy
restores it. It skips the optional metrics check, which the demo doesn't use.

The first run builds images and can take 10 minutes or more; once images are
cached, a run takes about 2 minutes. `dependencies` prints each step as it
starts and finishes, plus a `still running` line every two minutes, and tells
you where its `commands.log` is so you can follow it with `tail -f`. It ends
with `Dependencies check passed` or `Dependencies check failed` and the
reason.

If you close the terminal or press Ctrl-C, the check stops and cleans up. If a
second run reports that another quickstart owns `driver.lock`, it says whether
that run is still going. If it is, wait for it or stop it; it cleans up as it
stops. If it has exited, the message prints the `cleanup` command that releases
it. `cleanup` refuses while the owning run is still going unless you add
`--force`. After a crash, `cleanup` first stops the processes that run left
behind, then deletes its cluster. If a process with the run's PID is still
running but the run's start time wasn't recorded, `cleanup` can't tell whether
it is that run and needs `--force`.

The temporary cluster is deleted when the check finishes, whether it passes or
fails; a passing run reports `Kind cluster <name> removed` before its final
line. Logs and `result.json` stay under `.local/verification/quickstart/`.
This check doesn't run Codex or open a PR.
[Cluster dependencies](../reference/dependencies.md) lists what the demo
installs and what it doesn't need.

## 3. Choose a model account

The investigation-to-PR demo needs either an OpenAI API key with access to the demo's
`gpt-6.1-sol` model or a ChatGPT plan that supports the repository's sign-in
flow. The credential wizard in step 6 lets you choose. API-key mode incurs API
usage charges; ChatGPT mode uses your plan's allowance. See
[Model provider authentication](model-provider-auth.md) for the details.

Sign in to GitHub on your computer too:

```sh
gh auth login
gh auth status
```

## 4. Create the disposable GitHub repository

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

## 5. Create a GitHub App

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

## 6. Configure the demo

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
| Browser sign-in, for `chatgpt` mode | Complete the browser consent flow | Private `.local/chatgpt-session.json` and `.local/chatgpt-host.json`; the ChatGPT credential path is saved as `CHATGPT_SESSION_FILE` in `.local/demo.env`. |
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
every missing variable name at once without printing credential values, and
checks that the private-key file is readable. `make demo-check` checks
credentials first, in under a second, before the slower setup of the local
registry and the Codex image. Neither launches the agent.

The demo loads these saved settings automatically on later runs. A non-empty
value set on the command line or in the environment, such as
`make demo IMG=...` or an exported `SPROOZI_DEMO_REPOSITORY`, takes precedence
over the file. To replace
credentials, edit `.local/demo.env` in a local editor, remove the settings you
want the wizard to ask for again, and rerun it. The wizard keeps existing values.
For an independent configuration, set `SPROOZI_DEMO_ENV_FILE` to an absolute
gitignored path before running setup and the wizard; pass that same file to the
verification helper's `--env-file` option.

For ChatGPT mode, each ChatGPT sign-in belongs to one active gateway. Before
launching another cluster, renew consent with
`make demo-chatgpt-login CHATGPT_REAUTHORIZE=1`
(or `bash hack/demo/chatgpt-login.sh --reauthorize`). Without it, the helper
reuses the saved ChatGPT credential and prints a warning.
The local bootstrap file does not receive the live gateway's token refreshes.
See [Model provider authentication](model-provider-auth.md) for the lifecycle.

`make demo-check` should end with:

```text
Demo prerequisites and private configuration are available
```

## 7. Run it

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

The command uses a dedicated kubeconfig. `KIND_CLUSTER` defaults to
`sproozi-demo`. It refuses to reuse an existing cluster; choose a new name or
delete your previous demo cluster before trying again. If setup fails before the
run is submitted, the command deletes the cluster it created; set
`SPROOZI_PRESERVE_ON_FAILURE=1` to keep it. Once the run is submitted, the
cluster is kept, so you can inspect a failed acceptance check.

To watch while it runs, open another terminal in the repository root:

```sh
export KUBECONFIG="$PWD/.local/demo/sproozi-demo.kubeconfig"
kubectl get pods -n sproozi-system
kubectl get pods -n sproozi-demo
kubectl get agentruns -n sproozi-system -w
```

The target workload is deliberately broken. Its failing Pod is expected.
The agent Pod appears in `sproozi-agents` and is removed when its run ends.

## 8. Inspect the result and explore

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
[Manual AgentRun to pull request](manual-pr-demo.md) for probes, optional monitoring and MCP links.

## If something goes wrong

| Symptom | What to check |
| --- | --- |
| Docker is unavailable | Start the engine and rerun `docker info`. |
| Repository already exists | Use another disposable name, or seed the existing repository's `main` branch. |
| Missing configuration | Run `bash hack/demo/configure.sh`, then `make demo-check`. |
| The environment file `line N is not a plain KEY=value assignment`, or `sets NAME, which the demo refuses` | The demo checks the file before loading it. Quote values that contain spaces, remove lines that would run a command, and remove settings for shell or tool variables such as `BASH_ENV` or `PATH`. |
| `IMG is empty or unset` | Set `IMG` to a non-`latest` local tag, for example `make demo IMG=controller:sproozi-demo`. |
| `Another demo run is using cluster NAME` | A second `make demo` with the same `KIND_CLUSTER` is refused while the first holds `.local/demo/NAME.lock`. Wait for it or choose another name. If no run is active, the lock is stale; remove it with `rm -r .local/demo/NAME.lock`. |
| Cluster already exists | Inspect the old cluster, then choose a new `KIND_CLUSTER` or remove that demo cluster. |
| Image pull fails | Check `docker ps -a --filter name=sproozi-demo-registry`. Restart it with `docker start sproozi-demo-registry`. |
| Gateway does not start | Read `kubectl logs -n sproozi-system deployment/sproozi-gateway` in the demo context. Check credentials and certificate errors. |
| TLS fails with `tls: expired certificate` or a certificate that is not yet valid | The Docker Desktop VM clock has drifted from the host, usually after the host slept. Restart Docker Desktop, then rerun the command. Compare clocks with `date -u; docker run --rm busybox date -u`. |
| Model access or PR creation fails | Check the report, model authentication, and App installation permissions on the selected repository. |

If the run fails after it was submitted, the isolated kubeconfig is still at
`.local/demo/<cluster-name>.kubeconfig`; the command prints the `export`
line. Export it before debugging. To debug an earlier setup failure, rerun with
`SPROOZI_PRESERVE_ON_FAILURE=1`. A failing
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
