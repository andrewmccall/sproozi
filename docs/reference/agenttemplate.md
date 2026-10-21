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

| Field | Limits |
| --- | --- |
| `spec.runtimeRef.name`, `spec.policyRef.name` | 1 to 253 characters. |
| `spec.instructions` | 1 to 16384 characters. |
| `spec.egressProfiles` | At most 32 entries, no repeats. |

## Relationships

- every `AgentRun` references exactly one template
- the template chooses one runtime and one policy

The template has no external success condition field; lifecycle success uses
the workload's process exit code.
