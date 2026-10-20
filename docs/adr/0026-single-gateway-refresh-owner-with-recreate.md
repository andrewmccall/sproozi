# One gateway process owns the ChatGPT refresh, with a Recreate rollout

Status: accepted
Date: 2026-10-19

## Problem and constraints

In ChatGPT mode the gateway authorizes model requests with an administrator's
ChatGPT credential, an OAuth sign-in held in the `model-gateway-chatgpt` Secret. The provider rotates
the refresh token on every refresh, and the
[rotating-session requirements](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions)
allow one active refreshing client per credential. If two processes refresh with
the same refresh token, one of them holds a token the provider has already
replaced, and the credential can need a new browser sign-in.

The gateway serializes refreshes inside one process with a mutex, and keeps an
unsaved replacement so a failed write is retried without a second rotation.
Its store writes the Secret only if the stored tokens still match the ones it
refreshed from. That check stops a stale writer from overwriting newer tokens,
but it runs after the provider has rotated, so it cannot stop two processes
from both spending the same refresh token.

A Deployment's default `RollingUpdate` strategy starts the new Pod before the
old one stops, so every configuration change or image update would briefly run
two refresh owners.

## Decision

The gateway Deployment runs one replica with `strategy: Recreate`. Kubernetes
stops the old Pod before starting its replacement, so no two gateway processes
hold the ChatGPT credential at the same time. The single-replica, non-overlapping rollout
is part of the install contract, and a manifest test fails if it changes.

Among Sproozi's ServiceAccounts, only the gateway's may `get` and `update` the
`model-gateway-chatgpt` Secret, so no other Sproozi component can refresh it.
Sproozi's RBAC doesn't limit cluster administrators or other operators whose
roles already grant Secret access in `sproozi-system`; they can still read the
credential and must not use it.
Administrators must not scale the gateway, share its ChatGPT credential with another
cluster or run another refreshing client against it.

## Alternatives

- **`RollingUpdate` with `maxSurge: 0` and `maxUnavailable: 1`.** With one
  replica this also stops the old Pod first, but a later edit to the surge
  settings would silently restore overlap. `Recreate` states the requirement
  directly and is what the test checks.
- **Leader election among several replicas.** Only the leader would refresh,
  and the others would read the saved tokens. This allows more than one
  replica, but adds a Lease, election timing and a window where a deposed
  leader may still be mid-refresh. Lease expiry is not a fence against a
  provider call already in flight, so it would still need a token-level guard.
  One gateway replica is enough for current installs.
- **An external refresh service.** A separate process would own refresh and
  publish short-lived access tokens to gateways. It would allow many gateway
  replicas and rolling updates, but adds another trusted component holding the
  credential, its own availability and deployment, and a new trust boundary.

## Tradeoffs and consequences

Every gateway rollout, including a ConfigMap, Secret or image change, has a
short window with no gateway. Workloads cannot reach model, GitHub, Kubernetes
or MCP services until the new Pod is ready, and in-flight runs may fail and
need to be retried. The gateway is a single point of failure in both
authentication modes, because API-key mode uses the same Deployment.

A crash after the provider rotates but before the Secret is written can still
lose the credential and require a new sign-in. The local bootstrap file is not
updated after refresh, so provisioning another cluster needs a new sign-in.

## Revisit conditions

Revisit if the gateway needs more than one replica or zero-downtime rollouts,
if API-key mode moves to its own Deployment, if the provider allows several
concurrent refreshers or non-rotating tokens, or if a refresh service already
exists for another reason.

## Links

- [Model provider authentication](../guides/model-provider-auth.md)
- [Required Secrets and configuration](../reference/required-secrets-and-config.md)
- `config/manager/gateway.yaml` (`strategy: Recreate`, `replicas: 1`)
- `config/rbac/shared_gateway_credentials_role.yaml`
- `internal/modelauth/auth.go` (`ChatGPT`, `SessionStore`)
- `internal/modelauth/kubernetes/store.go` (`SessionStore.Replace`)
- `test/manifests/model_auth_test.go` (`TestChatGPTGatewayUsesSingleRefreshOwnerAndScopedCredentialAccess`)
