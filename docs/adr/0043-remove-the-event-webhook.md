# 0043: Remove the event webhook until a supported sender exists

Status: accepted
Date: 2026-10-21

## Problem and constraints

Every Kubernetes install ran `sproozi-webhook`, a signed HTTP receiver that
turned an external event into an AgentRun. Nothing used it. No demo, guide or
verifier sent it an event; every demo creates its AgentRun with `kubectl apply`.
Its only planned sender, the home-ops Alertmanager integration, was never built.
The Kind e2e checked only that the Deployment started.

Two review rounds on 2026-10-21 still found defects in it: events lost after a
failed create, store errors reported as replays, unbounded recursion on expired
records, receiver mapping by unauthenticated callers, and, after those were
fixed, duplicate runs when a receiver is reassigned between deliveries. Every
fix was proven only against a fake Kubernetes client. Meanwhile the receiver
added a Deployment, a ServiceAccount, two namespaces, an ingress exception, a
Secret-reading Role and an audit event type to every install.

## Decision

- **Remove the receiver.** Delete [cmd/webhook](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/cmd/webhook), [internal/webhook](https://github.com/andrewmccall/sproozi/tree/01bac1b47cf22ab66ebd8f8c9d7a16236f8f46f8/internal/webhook), its
  Deployment, Service, ServiceAccount and Roles, the `sproozi-webhooks` and
  `sproozi-webhook-state` namespaces, the `allow-webhook-traffic`
  NetworkPolicy, the `sproozi.com/webhook-*` label and annotation contract, the
  webhook audit event and the webhook contract reference page.
- **Users and tools create AgentRuns.** `kubectl`, the `sproozi` CLI and the task
  service already do. The integration tests now submit runs directly.
- **Keep what was learned.** The design, its limits and fourteen gotchas are in
  [event webhooks](../explanations/future-architecture/event-webhooks.md). The
  code before the unreleased rework ADR 0042 describes is at commit `01bac1b`.
- **Guard the removal.** A manifest test fails if the install renders any
  object named for the webhook.

This supersedes [ADR 0022](0022-webhook-hmac-secrets-in-own-namespace.md),
[ADR 0042](0042-idempotent-webhook-delivery.md) and the replay-record part of
[ADR 0017](0017-runtime-admission-and-replay-retention.md).

## Alternatives

- **Keep it and fix the duplicate-run defect.** Possible, but every fix so far
  added protocol and RBAC to a component with no user, and still lacked an
  end-to-end test against a real API server.
- **Keep the code but leave it out of the install.** Unshipped code still needs
  maintenance and review, and drifts from the API it creates objects through.
- **Ship it disabled by default.** The Deployment, RBAC and namespaces would
  still be installed and reviewed for an unused feature.

## Consequences

- Every install has one fewer network-reachable service, two fewer namespaces
  and no Role that reads Secrets for HMAC keys.
- External systems cannot start runs by themselves. The home-ops playbook and
  its acceptance plan now create runs by hand.

## Revisit when

- A supported sender exists, with an adapter Sproozi ships or documents end to
  end.
- Someone needs runs started by events badly enough to fund the Kind test in
  gotcha 14 of the future-architecture note.

## Links

- [Event webhooks](../explanations/future-architecture/event-webhooks.md)
- `test/manifests/manifest_test.go` (`TestInstallBundleHasNoEventWebhook`)
- [Install on Kubernetes: Upgrade](../guides/install-kubernetes.md#upgrade)
