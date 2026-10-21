# Remote executor access through a kubeconfig context

The CLI and the [home daemon](../../GLOSSARY.md#home-daemon) reach a
[remote executor](../../GLOSSARY.md#remote-executor) through a kubeconfig
[context](../../GLOSSARY.md#context): the developer's Kubernetes identity finds
the executor in a namespace, opens a port-forward to its Pod and runs the
executor's TLS protocol over it. No load balancer, hand-copied CA or token file
is needed. [ADR 0036](../adr/0036-remote-executors-through-kubeconfig-contexts.md)
records why.

The selection rules and messages below are implemented in
`internal/session/remote`. The `--context` and `--namespace` command flags
arrive with the CLI redesign; until then, home still reads `remote` entries
([session commands](session-commands.md)).

## Selecting the context and namespace

| Input | Meaning |
| --- | --- |
| no `--context` | The context named `sproozi` in the default kubeconfig: the files in `KUBECONFIG`, else `~/.kube/config`. |
| `--context NAME` | That context, when the default kubeconfig defines a context with exactly this name. |
| `--context PATH` | Otherwise, an existing kubeconfig file; its current context is used. A context of the same name wins over a file. |
| `--namespace NS` | The executor's namespace. Without it, the context's namespace, else `default`. |

Home records the absolute kubeconfig files, the context name and the namespace
when it first reaches an executor, and keeps using them after the CLI exits. A
later `kubectl config use-context`, or a different `KUBECONFIG` in home's
environment, does not change them. Home reloads the files on each new
connection, so a refreshed token is used. Exec credential plugins run in home's
environment, so their binaries must be on home's `PATH`.

The context must sign in with a bearer token: a ServiceAccount token, a token
file or an exec plugin that returns a token. A context that signs in only with a
client certificate, such as Kind's admin context, or that impersonates another
user, is refused, because the executor cannot verify it.

| Message begins | Fix |
| --- | --- |
| `the kubeconfig … has no context named "sproozi"` | Create the context as shown [below](#create-the-sproozi-context), or pass `--context`. |
| `--context "X" is neither a context in the kubeconfig … nor an existing kubeconfig file` | Pass a context from the listed names, or a path to a kubeconfig file. |
| `--context "X" was read as the kubeconfig file …, which sets no current context` | Run the printed `kubectl --kubeconfig FILE config use-context NAME`. |
| `no remote executor in namespace "NS"` | Pass `--namespace` with the executor's namespace, or ask the administrator to install it there. |
| `your Kubernetes user (…) may not …` | Pass `--namespace` if the executor is elsewhere; otherwise sign in as `sproozi-cli`, whose Role grants it. |
| `the cluster rejected the credentials of …` | The context's token is invalid or expired; write a new token into it. |
| `the remote executor … refused your Kubernetes user` | The identity lacks `use` on `remoteexecutors.sproozi.com`; bind the `sproozi-cli` Role. |
| `the remote executor … could not authenticate the token` | The executor's TokenReview rejected the token; write a new token into the context. |
| `… has no ready Pod` | The executor is not running; check its Pods. |
| `the remote executor's certificate is not trusted` | The executor's certificate must be issued by the published CA and cover `SERVICE.NAMESPACE.svc`. |
| `the remote executor … accepts checkpoints up to N bytes, but local.maxCheckpointBytes is M` | Make the two limits equal. |

## What the executor publishes

One remote executor per namespace. The administrator provides:

| Object | Name | Content |
| --- | --- | --- |
| Service | any, labelled `sproozi.com/remote-executor: "true"` | A port named `https` targeting the executor. |
| ConfigMap | `sproozi-remote-executor` | `ca.crt`: the CA that issued the executor's serving certificate. |
| Serving certificate | in the executor's own Secret | Covers `SERVICE.NAMESPACE.svc`. |
| ClusterRoleBinding | any | Binds the executor's ServiceAccount to `system:auth-delegator`, for TokenReview and SubjectAccessReview. |

Set `"homeAuthorization": "kubernetes"` in the executor's configuration and omit
`tokenFile`; see [the executor setup](../../cmd/session-profile/README.md#create-private-configuration).
The executor then accepts a caller as its home principal when a TokenReview
authenticates the bearer token and a SubjectAccessReview allows `use` on
`remoteexecutors.sproozi.com`, named by the executor's `placementName`, in its
`namespace`. Removing the RoleBinding revokes access on the next request. The
[retained-work example](../../examples/portable-workflows/kubernetes/owner.yaml)
carries the Service label and the binding. Publish the CA from its file:

```sh
kubectl -n sproozi-system create configmap sproozi-remote-executor --from-file=ca.crt=/path/to/executor-ca.crt
```

The executor describes itself on request: its placement name, harness and
`maxCheckpointBytes`. The placement name is the executor's name in session rows
and commands, whatever the context is called; its checkpoint limit must equal
home's `local.maxCheckpointBytes`.

## The CLI's ServiceAccount

`config/cli-access`, released as `sproozi-release/cli-access/`, holds the CLI's
identity in the executor's namespace. Apply it there with the rest of your
manifests; its `kustomization.yaml` sets `sproozi-system`, which an overlay can
change.

| Object | Name | Purpose |
| --- | --- | --- |
| ServiceAccount | `sproozi-cli` | The CLI and home's identity. |
| Role, RoleBinding | `sproozi-cli` | Exactly the permissions below. |
| Secret | `sproozi-cli-token` | A long-lived token Kubernetes fills for `sproozi-cli`. |

| API group | Resource | Verbs | Why |
| --- | --- | --- | --- |
| core | `services` | `list` | Find the Service labelled `sproozi.com/remote-executor=true`. |
| `discovery.k8s.io` | `endpointslices` | `list` | Find a ready executor Pod. |
| core | `pods/portforward` | `create` | Open the TLS tunnel to that Pod. |
| core | `configmaps` named `sproozi-remote-executor` | `get` | Trust the executor's certificate. |
| `sproozi.com` | `remoteexecutors` | `use` | Act as the executor's home principal. No such API resource exists; the executor checks the rule with a SubjectAccessReview. Add `resourceNames` with a placement name to allow one executor only. |

`pods/portforward` covers every Pod in the namespace. A dedicated namespace for
the executor keeps the grant to the executor alone.

## Create the sproozi context

With an administrator context for the cluster, apply the CLI access and wait
for Kubernetes to fill the token:

```sh
NS=sproozi-system
kubectl apply -k sproozi-release/cli-access      # or config/cli-access from a checkout
kubectl -n "$NS" wait --for=jsonpath='{.data.token}' secret/sproozi-cli-token
TOKEN=$(kubectl -n "$NS" get secret sproozi-cli-token -o jsonpath='{.data.token}' | base64 -d)
```

Write the `sproozi` context into your kubeconfig, reusing the administrator
context's cluster entry:

```sh
CLUSTER=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')
kubectl config set-credentials sproozi-cli --token="$TOKEN"
kubectl config set-context sproozi --cluster="$CLUSTER" --user=sproozi-cli --namespace="$NS"
kubectl --context sproozi auth can-i create pods --subresource=portforward
```

The last command prints `yes`. To keep the context in its own file instead,
for example to pass it as `--context PATH`, add `--kubeconfig FILE` to the
`config` commands, first copying the cluster entry with
`kubectl --kubeconfig FILE config set-cluster NAME --server=URL --certificate-authority=CA_FILE --embed-certs`,
then run `kubectl --kubeconfig FILE config use-context sproozi` and
`chmod 600 FILE`.

To rotate the token, delete the Secret, re-apply `cli-access`, and run the
`set-credentials` command again with the new token. Home picks it up on its
next connection.
