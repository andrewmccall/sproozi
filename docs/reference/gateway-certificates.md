# Gateway certificates

Workers trust the gateway through a private CA. This page covers renewing and
rotating that CA on Kubernetes. The local service renews its own; see
[Local sessions](local-installation.md#certificates).

## Self-managed CA

The certificate controller renews the gateway's 30-day leaf after two thirds of
its lifetime. It checks every five minutes. The gateway reloads the renewed
Secret within about a minute of the kubelet updating it, without restarting.
Status is on the authority Secret:

```sh
kubectl get secret -n sproozi-system sproozi-certificate-authority \
  -o jsonpath='{.metadata.annotations}'
```

`sproozi.com/leaf-not-after`, `sproozi.com/renew-at`,
`sproozi.com/issuer-fingerprint`, `sproozi.com/authority-not-after`,
`sproozi.com/rotate-at`, `sproozi.com/trust-configmap` and
`sproozi.com/rotation-stage` report the current state. Start a rotation before
`rotate-at`. If the authority Secret loses its data, the controller stops with
an error instead of creating a new authority; restore the Secret from a backup.
`sproozi.com/last-error` explains a refused request. If renewal
fails, the previous certificate stays in service and the controller log names
the cause. Renew immediately with:

```sh
kubectl annotate secret -n sproozi-system sproozi-certificate-authority \
  sproozi.com/rotation-request=renew --overwrite
```

The controller's certificate covers the gateway Service and
`kubernetes.default.svc`. When you enable model, GitHub or MCP modules, add a
`--dns NAME` argument to the `sproozi-certificates` Deployment for each extra
authority the gateway inspects, such as `api.openai.com`.

Rotate the CA in four steps. Trust ConfigMaps are immutable, so each step
creates a new one; existing Pods keep the bundle they started with.

1. Prepare. The controller creates a second authority and the combined bundle
   `sproozi-sandbox-trust-1-2`. The gateway still serves a leaf from the first
   authority.

   ```sh
   kubectl annotate secret -n sproozi-system sproozi-certificate-authority \
     sproozi.com/rotation-request=prepare --overwrite
   ```

2. Point every AgentRuntime at the combined bundle, then wait until every Pod
   started with the old bundle has finished:

   ```sh
   kubectl patch agentruntime -n sproozi-system RUNTIME --type=merge \
     -p '{"spec":{"clientConfig":{"trustBundleConfigMap":{"name":"sproozi-sandbox-trust-1-2","key":"ca-bundle.pem"}}}}'
   kubectl get pods -n sproozi-agents
   ```

   Profile and task services that verify the gateway with their own trust
   bundle need the combined bundle too.

3. Switch the gateway to a leaf from the new authority with
   `sproozi.com/rotation-request=switch`. Both authorities remain trusted. To
   undo the switch, use `rollback`.

4. Retire the old authority with `sproozi.com/rotation-request=retire`. The
   controller creates `sproozi-sandbox-trust-2` and destroys the old key. If
   that is interrupted, every later reconcile destroys it until it is gone.
   Point the runtimes at the new bundle, and delete the old ConfigMaps once no
   runtime or Pod uses them.

Use `replace` only for a lost or compromised authority, or a refused
certificate record. It trusts only a new authority at once, so every running
Pod loses trust in the gateway. Cancel running work first, and point runtimes
at the new bundle afterwards.

### A refused certificate record

The controller refuses an authority Secret whose `state.json` describes no
[rotation phase](../../GLOSSARY.md#rotation-phase), or one of whose
`ca-N.pem` certificates, trusted or kept as history, is not the one
`state.json` records. Every reconcile then fails, and the controller logs,
every five minutes:

```text
Failed to reconcile gateway certificates err="pki: corrupt state: <what is wrong>"
```

or `err="pki: certificate authority files do not match state: <what is
wrong>"`. That reconcile writes nothing: the authority Secret, the serving
Secret and the trust ConfigMaps are unchanged, the annotations keep their last
values, and any request annotation other than `replace` stays in place,
refused. The gateway keeps serving its current certificate, across restarts,
but nothing renews it, so it stops working when that certificate expires, at
most 30 days after it was issued. Trust ConfigMaps for new Pods are unchanged.

Restore the authority Secret from a backup, or replace the authority:

```sh
kubectl annotate secret -n sproozi-system sproozi-certificate-authority \
  sproozi.com/rotation-request=replace --overwrite
```

Replace numbers the new authority after every generation in the record or the
Secret, retires all of them, destroys every earlier authority key and serving
key, and keeps earlier certificates as history. It publishes a new trust
ConfigMap, `sproozi-sandbox-trust-N`, and its cost is the one above: every
running Pod loses the gateway until it is restarted with the new bundle. Replace
refuses only a `state.json` it cannot parse, or one written by a different
record version; restore those from a backup.

### Cleanup the controller reports

Each reconcile also removes from the authority Secret what the record no
longer needs: the key of every retired authority, serving-certificate files
(`leaf-<hex>.pem` and `.key`) other than the one the record names, and the
files of an authority a prepare wrote but never recorded. A trusted
authority's key is read only by the step that uses it, and then checked. A
problem with either does not stop the reconcile: it renews and publishes as
usual, then fails with, for example:

```text
Failed to reconcile gateway certificates err="pki: destroy retired keys: <the removal that failed>"
Failed to reconcile gateway certificates err="pki: certificate authority key: generation 2 key ca-2.key does not match its certificate: <cause>"
```

A key that cannot be read reports `is unreadable: <cause>` instead. Every
reconcile retries, so the line repeats until the file is fixed or removed.
A missing serving key is reported the same way (`… is missing; the next
renewal will fail`) while the certificate is still valid. A step that needs a
broken serving key - renewing from it, switching to it, or retiring the
authority before it, which would destroy the last key that works - fails
with `pki: serving CA key unavailable; recover it or replace the CA:
generation N key ca-N.key: <cause>`, where the cause is that it cannot be
read, `pki: one PKCS#8 private key required`, or `pki: private key does not
match certificate`. A refused `retire` request is reported in
`sproozi.com/last-error`.

### When the record outgrows what the controller reads

The controller reads `state.json` up to 64 KiB. A `prepare` or `replace`
starts only while the record leaves 1024 bytes below that, so a rotation
it starts can always be switched, rolled back and retired. After several
hundred generations one is refused, before anything is written, with `pki:
refusing to prepare: the record is N bytes, and starting it needs 1024 bytes
below the 65536 bytes load reads`. A `--dns` change that would grow the record
into those 1024 bytes is refused the same way before a certificate is issued
(`pki: refusing to change the serving names: … the serving names list does
not fit`): the gateway keeps its current certificate, which is not renewed
until the names list is shortened again. Renewal with unchanged names
continues. The history cannot be shortened in place. Back up the authority Secret, then remove all of its data
and its `sproozi.com/trust-configmap` annotation: the controller then creates
a new authority, generation 1, whose trust ConfigMap is
`sproozi-sandbox-trust`. Delete the old ConfigMap of that name first, once no
runtime references it, and move every runtime to the new bundle; as with
`replace`, every running Pod loses the gateway. A `replace` on a refused
record also refuses, naming the cause, when a stray `ca-N` key in the Secret
or a generation id in `state.json` would number the new authority beyond that
size: remove that key from the Secret, or correct or restore `state.json`,
and request `replace` again.

### What validation does not protect

These checks find damage and mistakes, such as a truncated restore, a stray
file or a hand edit that contradicts itself. They cannot authenticate the
record: whoever can write `state.json` can write every file beside it, and on
Kubernetes editing one key of the authority Secret needs write access to the
whole Secret. The controller's RBAC and who may update the authority Secret
are what protect it. Two consequences of an edit to `state.json` alone:

- From a replaced store (generation 1 retired, 2 serving), un-retiring
  generation 1 and recording `switched 1 to 2` with both trusted loads; the
  next reconcile puts the retired authority's certificate back in the trust
  bundle. Its missing key is the accepted lost-key state.
- Marking a generation retired makes the next reconcile destroy its key,
  without a `retire` request and without the operator confirming that older
  Pods stopped.

## Your own certificates

The `quickstart-external-ca` overlay deploys no signer. Before applying it,
create:

- the Secret `sproozi-gateway-tls` in `sproozi-system`, with `tls.crt` and
  `tls.key`: a serving certificate covering `sproozi-gateway.sproozi-system.svc`,
  `sproozi-gateway.sproozi-system.svc.cluster.local` and
  `kubernetes.default.svc`, plus each extra authority the gateway inspects;
- the ConfigMap `sproozi-sandbox-trust` in `sproozi-agents`, created with
  `immutable: true`, whose `ca-bundle.pem` holds the issuing CA.

Renew the certificate before it expires by replacing `tls.crt` and `tls.key` in
`sproozi-gateway-tls`; the gateway reloads them without restarting. To change
the issuing CA, create a new immutable trust ConfigMap and follow the same
switch order: new bundle trusted by all runtimes first, new leaf second, old
bundle removed last.

## Audit records

The gateway writes one JSON audit record per request to its log. Collect
`kubectl logs -n sproozi-system deployment/sproozi-gateway` with your cluster's
log pipeline and keep it for your retention period; Pod logs disappear when the
Pod is replaced.

A record names the execution (`runID`, `runUID`, `executionBackend`, and
`saName`, `saNamespace`, `saUID` on Kubernetes), the policy (`policyName`,
`policyGeneration`), the route (`capability`, `semanticLevel`), the
`operation` and, where it applies, `service`, `resource`, `artifactURL`,
`providerAuthMode` and `upstreamStatus`. `allowed` says whether the gateway
permitted the request and `denyReason` why not. Model calls add `model`,
`tokensUsed`, `inputTokens`, `outputTokens`, `usageSource`, `endReason`,
`tokenBudget` and `tokensRemaining`. A record never contains request or
response bodies, headers, credentials or query strings.

`outcome` says how the request ended
([ADR 0070](../adr/0070-gateway-routes-declare-admission-and-requests-record-one-decision.md)):

| `outcome` | Meaning |
| --- | --- |
| `served` | A response was relayed, whatever its status. `endReason` says how a model call ended. |
| `refused_request` | The request was malformed or unsupported. |
| `refused_policy` | The run, policy or a credential rule does not allow it. |
| `refused_budget` | The allocation or budget is exhausted, or too many model calls are pending. |
| `unavailable` | The gateway could not account, build or authenticate the upstream request, or is stopping. |
| `upstream_failed` | Nothing usable came back before anything was relayed. |
| `cancelled` | The workload left before a response was relayed. |
| `unrecorded` | The handler wrote no record; the dispatcher wrote this one for the route. It names no request detail. |

`allowed` is unchanged by `outcome` and not always its opposite: a model call
that ends while it waits for its budget because the gateway is stopping is
`allowed` `true` with `outcome` `unavailable`, and a provider's own error is
`allowed` `true` on OpenAI routes. Read `outcome` to tell a budget refusal
from a policy refusal or an upstream failure.

These requests have no record by design while their auditing is undecided:
named MCP discovery (`initialize`, `tools/list`, `ping`, `server/discover`
and notifications), Kubernetes `/mcp` discovery and its shape and argument
refusals, and the `sproozi` control connection. Requests without a valid
gateway identity are refused before routing and have no record.

