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
an error instead of creating a new authority; restore the Secret from a backup. `sproozi.com/last-error` explains a refused request. If renewal
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
   controller creates `sproozi-sandbox-trust-2` and destroys the old key. Point
   the runtimes at the new bundle, and delete the old ConfigMaps once no runtime
   or Pod uses them.

Use `replace` only for a lost or compromised authority. It trusts only a new
authority at once, so every running Pod loses trust in the gateway. Cancel
running work first, and point runtimes at the new bundle afterwards.

## Your own certificates

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

