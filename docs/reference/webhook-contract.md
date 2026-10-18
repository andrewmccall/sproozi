# Webhook contract

The cluster-internal webhook converts a signed event into one `AgentRun`. The
template and policy determine its runtime and permissions. To connect
Alertmanager, you must supply a notification converter and HMAC signer.

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

- The webhook stores replay keys atomically in ConfigMaps in `sproozi-webhook-state`. Set `REPLAY_NAMESPACE` to use another namespace.
- Records survive restarts and are shared across replicas
- The webhook deletes expired records once per replay window, so a record outlives its window by at most one more window
- The webhook records the event before creating the AgentRun. A failed creation can consume the event ID. Replay protection does not guarantee delivery.
- Alertmanager integration needs a notification converter and HMAC signer; this repository does not supply that adapter
- For broader deployment guidance, see [Security hardening](security-hardening.md)

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

If zero or multiple templates match, the request is denied with `404` or `409`.
A transient failure to read the template or its policy returns `503`; retry
the event.

## What the webhook creates

The webhook creates an `AgentRun` with:

- `spec.templateRef` fixed to the resolved template
- `spec.task` fixed to `Process incoming webhook event`
- `spec.capabilities` copied from the policy-approved capability set
- `spec.eventContext` copied from the verified request body

The webhook never lets the caller override runtime, policy, or lifecycle decisions.
