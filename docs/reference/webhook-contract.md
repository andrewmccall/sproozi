# Webhook contract

**Audience:** operators, contributors

The webhook is a cluster-internal adapter that turns a signed event into one bounded `AgentRun`.

## URL shape

```text
POST /webhook/<namespace>/<receiver>
```

Example:

```text
http://sproozi-webhook.sproozi-system.svc.cluster.local:8083/webhook/sproozi-system/alertmanager
```

## Required headers

| Header | Meaning |
| --- | --- |
| `X-Sproozi-Timestamp` | unix seconds used for replay protection |
| `X-Sproozi-Signature` | `sha256=<hex-hmac>` computed over `timestamp || rawBody` |

## Operational caveats

- the deployed webhook stores replay keys atomically in Kubernetes ConfigMaps in `sproozi-webhook-state`, configurable with `REPLAY_NAMESPACE`
- records survive restarts and are shared across replicas
- expired records are removed when the same event key is checked again; unique records have no periodic cleanup
- replay recording happens before AgentRun creation; a creation failure can consume the event ID, so replay storage does not guarantee successful delivery
- Alertmanager integration needs a notification converter and HMAC signer; this repository does not supply that adapter
- for broader deployment guidance, see [Security hardening](security-hardening.md)

## Body

```json
{
  "id": "alertmanager-123",
  "eventContext": {
    "alertname": "CrashLoopBackOff",
    "namespace": "sproozi-demo"
  }
}
```

`id` must be non-empty. `eventContext` is a string-to-string map. The timestamp
must fall within the replay window, five minutes by default. Receiver metadata
and a Secret containing `hmac-key` are required; the manual example supplies neither.

## Receiver resolution

The webhook looks up exactly one `AgentTemplate` in `<namespace>` with:

- label `sproozi.com/webhook-receiver=<receiver>`
- annotation `sproozi.com/webhook-secret=<secret-name>`

If zero or multiple templates match, the request is denied.

## What the webhook creates

The webhook creates an `AgentRun` with:

- `spec.templateRef` fixed to the resolved template
- `spec.task` fixed to `Process incoming webhook event`
- `spec.capabilities` copied from the policy-approved capability set
- `spec.eventContext` copied from the verified request body

The webhook never lets the caller override runtime, policy, or lifecycle decisions.
