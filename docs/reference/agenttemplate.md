# AgentTemplate

**Audience:** operators, contributors

`AgentTemplate` is the trusted, administrator-managed description of a class of work. It is the object that turns a runtime and a policy into a named operational workflow.

## What it owns

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
with key `hmac-key` must be configured before it can receive events. They are not
needed for the manual `AgentRun` path. The template has no external success
condition field; lifecycle success uses the workload's process exit code.

The webhook path depends on object metadata as well as `spec`:

- label: `sproozi.com/webhook-receiver=<receiver>`
- annotation: `sproozi.com/webhook-secret=<secret-name>`

That keeps receiver discovery explicit and per-template.
