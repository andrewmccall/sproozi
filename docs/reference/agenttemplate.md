# AgentTemplate

`AgentTemplate` defines a named type of task. An administrator selects its
runtime and policy and supplies trusted instructions. Each `AgentRun` references
one template.

## What it defines

- one runtime reference
- one policy reference
- trusted operating instructions
- named egress profiles selected for this class of work

## Key fields

```yaml
metadata:
  labels:
    sproozi.com/webhook-receiver: alertmanager
  annotations:
    sproozi.com/webhook-secret: alertmanager-secret
spec:
  runtimeRef:
    name: codex
  policyRef:
    name: sre
  instructions: >
    Investigate the failing workload and open a pull request.
  egressProfiles:
    - go-modules
```

## Relationships

- every `AgentRun` references exactly one template
- the template chooses one runtime and one policy
- the webhook resolves a receiving template by `sproozi.com/webhook-receiver`

## Optional webhook receiver metadata

The base install deploys the webhook, but these metadata fields and a Secret
in `sproozi-webhooks` with key `hmac-key`, at least 32 random bytes, must be
configured before it can receive events. They are not needed for the manual
`AgentRun` path. The template has no external success
condition field; lifecycle success uses the workload's process exit code.

The webhook path depends on object metadata as well as `spec`:

- label: `sproozi.com/webhook-receiver=<receiver>`
- annotation: `sproozi.com/webhook-secret=<secret-name>`

The webhook uses this metadata to find the receiving template.
