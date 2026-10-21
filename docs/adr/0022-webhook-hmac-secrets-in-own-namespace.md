# 0022: Webhook HMAC Secrets live in their own namespace

Status: superseded by [ADR 0043](0043-remove-the-event-webhook.md)
Date: 2026-10-18

## Problem and constraints

The event receiver verifies each request with an HMAC key from a Secret named
by its template's `sproozi.com/webhook-secret` annotation. It read that Secret
from the template's namespace, and its Role in `sproozi-system` granted `get`
on every Secret there. `sproozi-system` also holds the model provider
credentials, the GitHub App private key and the gateway's TLS key.

The webhook is reachable before authentication. A flaw in the receiver, or
anyone who can edit an `AgentTemplate`, could therefore reach those
credentials: an annotation that named `capability-proxy-github-app` would make
the webhook read it.

Receiver names and Secret names are chosen by administrators when they add a
receiver, so the installed RBAC cannot list them in advance.

## Decision

HMAC Secrets live in a dedicated namespace, `sproozi-webhooks`. The install
creates the namespace, a Role there that grants only `get` on `secrets`, and a
RoleBinding for the webhook ServiceAccount. The webhook's Role in
`sproozi-system` no longer grants any Secret access, and the generated
`manager-role` no longer carries the webhook's Secret marker.

The webhook reads the annotated Secret from `HMAC_SECRET_NAMESPACE`, which
defaults to `sproozi-webhooks`, never from the template's namespace. The
annotation still names the Secret, so receivers need no naming convention.

The install also binds the webhook ServiceAccount to its `sproozi-system` Role
by the rendered name; before this change the binding named an unprefixed
ServiceAccount and the rendered webhook had no binding there.

## Alternatives

- **`resourceNames` on the `sproozi-system` Role.** Secret names come from
  template annotations, so the Role would need editing for every new receiver,
  or Secret names would need a fixed convention such as one name per receiver.
  Either way the HMAC keys would share a namespace with the credentials, and a
  mistake in the list would expose them.
- **Reuse `sproozi-webhook-state`.** It already exists and is bound to the
  webhook, but the webhook creates and deletes the replay records there. HMAC
  Secrets are administrator input. Keeping them apart lets administrators
  write HMAC Secrets without access to replay state, and lets the replay
  namespace move with `REPLAY_NAMESPACE`.
- **Read the Secret in the template namespace, with Secret access granted in
  each template namespace.** This keeps keys beside templates, but every
  template namespace, including `sproozi-system`, would need a Secret grant.

## Tradeoffs and consequences

The install adds a third namespace. Administrators must create HMAC Secrets in
`sproozi-webhooks` instead of beside the template, and existing Secrets must be
moved. A receiver whose Secret is still in the template namespace fails with
`500` until it is moved.

Every receiver's key is readable by the webhook, and anyone who can edit a
template can point it at any key in `sproozi-webhooks`. That was already true
of every Secret in `sproozi-system`; now it is limited to HMAC keys.

## Revisit conditions

Revisit if templates move to many namespaces with separate administrators who
must not share HMAC keys, if receivers adopt a fixed naming convention that
`resourceNames` could express, or if HMAC keys move to an external secret
store.

## Links

- [Webhook contract](https://github.com/andrewmccall/sproozi/blob/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/docs/reference/webhook-contract.md)
- [Permissions and capabilities](../reference/permissions-and-capabilities.md#trusted-service-identities)
- [Security hardening](../reference/security-hardening.md)
- [config/webhook-secrets/](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/config/webhook-secrets)
- [internal/webhook/handler.go](https://github.com/andrewmccall/sproozi/blob/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/internal/webhook/handler.go) (`HandlerConfig.SecretNamespace`)
- `test/manifests/manifest_test.go` at `01bac1b` (`TestInstallBundleScopesWebhookSecretReads`), removed with the webhook by ADR 0043
