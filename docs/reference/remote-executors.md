# Remote executor access through a kubeconfig context

The CLI and this machine's Sproozi service reach a
[remote executor](../../GLOSSARY.md#remote-executor) through a kubeconfig
[context](../../GLOSSARY.md#context): the developer's Kubernetes identity finds
the executor in a namespace, opens a port-forward to its Pod and runs the
executor's TLS protocol over it. No load balancer, hand-copied CA or token file
is needed. [ADR 0036](../adr/0036-remote-executors-through-kubeconfig-contexts.md)
records why.

[Remote commands](session-commands.md#remote-commands) take `--context` and
`--namespace`. `run --remote`, `sessions --remote`, `status`, `cancel` and
`recover` reach the cluster from the CLI. `move --remote` and `pull` send the
selection to the service, which reaches the cluster from its own environment.

## Selecting the context and namespace

| Input | Meaning |
| --- | --- |
| no `--context` | The context named `sproozi` in the default kubeconfig: the files in `KUBECONFIG`, else `~/.kube/config`. |
| `--context NAME` | That context, when the default kubeconfig defines a context with exactly this name. |
| `--context PATH` | Otherwise, an existing kubeconfig file; its current context is used. A context of the same name wins over a file. |
| `--namespace NS` | The executor's namespace. Without it, the context's namespace, else `default`. |

The service records each executor it reaches in `executors.json` in its
directory (mode `0600`): the executor's name, harness and checkpoint limit, and
the absolute kubeconfig files, context and namespace that reached it. It keeps
using them after the command exits and after a restart, and connects only when
a session needs the executor. A later `kubectl config use-context`, or a
different `KUBECONFIG` in the service's environment, does not change them. The
service reloads the files on each new connection, so a refreshed credential is used.
Exec credential plugins run in the service's environment, so their binaries
must be on its `PATH`. Reaching the same executor name through another context
or file points the record at that one.

`move ID --local` needs no context: the service already knows where the
session is. `pull ID` uses the default context unless `--context` names
another; a pull the service already began continues from its recorded executor
whatever the retry names.

The context's own credential goes only to the Kubernetes API server, never to
the executor. On each connection the CLI or service asks the API server for a
short-lived token for the `sproozi-cli` ServiceAccount in the executor's
namespace (a TokenRequest, valid for at most an hour), bound to the audience
`sproozi-remote-executor`, and sends the executor that token alone. The
executor's TokenReview accepts only that audience, and the API server does not
accept it, so an executor that is compromised or impersonated cannot replay a
caller's token against the cluster. The service connects again, minting a new
token, five minutes before the current one expires.

Any context that may create that token works: the `sproozi` context below, or
an administrator's context, including one that signs in with an exec plugin or a
client certificate. Whatever context you use, the executor sees the
`sproozi-cli` identity, and RBAC decides on that identity.

| Message begins | Fix |
| --- | --- |
| `the kubeconfig … has no context named "sproozi"` | Create the context as shown [below](#create-the-sproozi-context), or pass `--context`. |
| `--context "X" is neither a context in the kubeconfig … nor an existing kubeconfig file` | Pass a context from the listed names, or a path to a kubeconfig file. |
| `--context "X" was read as the kubeconfig file …, which sets no current context` | Run the printed `kubectl --kubeconfig FILE config use-context NAME`. |
| `no remote executor in namespace "NS"` | Pass `--namespace` with the executor's namespace, or ask the administrator to install it there. |
| `your Kubernetes user (…) may not …` | Pass `--namespace` if the executor is elsewhere; otherwise sign in as `sproozi-cli`, whose Role grants it. |
| `the cluster rejected the credentials of …` | The context's token is invalid or expired; write a new token into it. |
| `the remote executor … refused your Kubernetes user` | The identity lacks `use` on `remoteexecutors.sproozi.com`; bind the `sproozi-cli` Role. |
| `your Kubernetes user (…) may not create a token for the ServiceAccount sproozi-cli` | Bind the `sproozi-cli` Role, which grants `create` on `serviceaccounts/token` for `sproozi-cli`, or use a context that has it. |
| `namespace "NS" has no ServiceAccount sproozi-cli` | Ask the administrator to apply `cli-access` (`config/cli-access`, or `sproozi-release/cli-access/`) in that namespace. |
| `the remote executor … could not authenticate the sproozi-cli token` | The executor's TokenReview rejected the minted token. An executor older than this client does not accept the `sproozi-remote-executor` audience; ask the administrator to upgrade it. |
| `… has no ready Pod` | The executor is not running; check its Pods. |
| `the remote executor's certificate is not trusted` | The executor's certificate must be issued by the published CA and cover `SERVICE.NAMESPACE.svc`. |
| `the remote executor … accepts checkpoints up to N bytes, but local.maxCheckpointBytes is M` | Make the two limits equal. |
| `the remote executor is named "local", which is this machine's own name` | Ask the administrator to give the executor another `placementName`. |
| `the Sproozi service does not know which cluster holds session …` | Pass `--context` to `pull`. |

## What the executor publishes

One remote executor per namespace. The administrator provides:

| Object | Name | Content |
| --- | --- | --- |
| Service | any, labelled `sproozi.com/remote-executor: "true"` | A port named `https` targeting the executor. |
| ConfigMap | `sproozi-remote-executor` | `ca.crt`: the CA that issued the executor's serving certificate. Created with `immutable: true`. |
| Serving certificate | in the executor's own Secret | Covers `SERVICE.NAMESPACE.svc`. |
| ClusterRoleBinding | any | Binds the executor's ServiceAccount to `system:auth-delegator`, for TokenReview and SubjectAccessReview. |

See [the executor setup](../../cmd/session-profile/README.md#create-private-configuration).
The executor accepts a caller as its home principal when a TokenReview
authenticates the bearer token for the audience `sproozi-remote-executor` and a
SubjectAccessReview allows `use` on
`remoteexecutors.sproozi.com`, named by the executor's `placementName`, in its
`namespace`. Removing the RoleBinding revokes access on the next request. The
[retained-work example](../../examples/portable-workflows/kubernetes/owner.yaml)
carries the Service label and the binding. Publish the CA from its file as an
immutable ConfigMap. The executor may update other ConfigMaps in its namespace,
so immutability is what stops it replacing the CA the CLI trusts for it:

```sh
{ kubectl -n sproozi-sessions create configmap sproozi-remote-executor \
    --from-file=ca.crt=/path/to/executor-ca.crt --dry-run=client -o yaml
  echo 'immutable: true'; } | kubectl create -f -
```

To replace the CA, delete the ConfigMap and create it again the same way.

The executor describes itself on request: its placement name, harness and
`maxCheckpointBytes`. The placement name is the executor's name in session rows
and commands, whatever the context is called; its checkpoint limit must equal
the service's `local.maxCheckpointBytes`.

## The CLI's ServiceAccount

`config/cli-access`, released as `sproozi-release/cli-access/`, holds the CLI's
identity in the executor's namespace. Apply it there with the rest of your
manifests. It targets `sproozi-sessions`, the example executor's namespace: the
checkout's `kustomization.yaml` sets it, and the release's `cli-access.yaml`
is rendered with it. An overlay with its own `namespace` changes it. Never apply it in `sproozi-system`:
there `pods/portforward` would reach the gateway and controller.

| Object | Name | Purpose |
| --- | --- | --- |
| ServiceAccount | `sproozi-cli` | The CLI and the service's identity. |
| Role, RoleBinding | `sproozi-cli` | Exactly the permissions below. |
| Secret | `sproozi-cli-token` | A long-lived token Kubernetes fills for `sproozi-cli`. |

| API group | Resource | Verbs | Why |
| --- | --- | --- | --- |
| core | `services` | `list` | Find the Service labelled `sproozi.com/remote-executor=true`. |
| `discovery.k8s.io` | `endpointslices` | `list` | Find a ready executor Pod. |
| core | `pods/portforward` | `create` | Open the TLS tunnel to that Pod. |
| core | `configmaps` named `sproozi-remote-executor` | `get` | Trust the executor's certificate. |
| core | `serviceaccounts/token` named `sproozi-cli` | `create` | Mint the short-lived, audience-bound token the executor accepts. It is a token for the same identity, so it grants nothing new. |
| `sproozi.com` | `remoteexecutors` | `use` | Act as the executor's home principal. No such API resource exists; the executor checks the rule with a SubjectAccessReview. Add `resourceNames` with a placement name to allow one executor only. |

`pods/portforward` covers every Pod in the namespace. A dedicated namespace for
the executor keeps the grant to the executor alone.

## Create the sproozi context

With an administrator context for the cluster, apply the CLI access and wait
for Kubernetes to fill the token:

```sh
NS=sproozi-sessions
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
`set-credentials` command again with the new token. The service picks it up on its
next connection.
