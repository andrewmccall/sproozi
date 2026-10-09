# Persistent assistant orchestration

A persistent assistant owns its conversations, memory, channels and schedules.
Sproozi owns the admission, identity, budget, deadline and cleanup of every
delegated `AgentRun`. The assistant runs in its own Deployment and persistent
volume, outside the disposable worker lifecycle.

The task service speaks native Streamable HTTP MCP at `https://<service>:8443/mcp`.
Its administrator configuration maps workflow names to fixed templates and
capability subsets in one namespace. The model supplies task text and operation
identity; it cannot supply a runtime, image, policy, credential, provider URL or
Kubernetes document. The controller still evaluates live policy at admission.

The initial integration uses Hermes. Any conforming native MCP client can use
the same task interface without a client-specific Sproozi plugin.

## Task service configuration

The release image contains `/tasks`. It requires these environment variables:

| Variable | Value |
| --- | --- |
| `SPROOZI_TASK_CONFIG` | Path to administrator-owned JSON configuration |
| `SPROOZI_TASK_TOKEN_FILE` | Secret file containing a bearer token of at least 32 non-whitespace characters |
| `SPROOZI_TASK_CERT_FILE` | Server certificate covering the task service hostname |
| `SPROOZI_TASK_KEY_FILE` | Matching private key |

```json
{
  "namespace": "sproozi-system",
  "principal": "hermes",
  "workflows": {
    "investigate": {
      "template": "hermes-investigate",
      "capabilities": ["model.inference", "kubernetes.read"]
    }
  }
}
```

Each deployment represents one durable principal. Authentication applies to
every HTTP request; an MCP session identifier grants no authority. Use separate
deployments, tokens and principal names for separately authorized assistants.
The bearer token grants the configured workflows and access to that principal's
tasks; it does not identify individual users of a shared bot.

The service needs namespaced `get/create/patch` on AgentRuns and `get` on
AgentTemplates and AgentPolicies. It needs no Pod, Secret, RBAC, runtime,
policy-write or status-write permission. The assistant has no Kubernetes API
credentials. The [deployment example](../../examples/orchestration/task-service.yaml)
provides private Service ingress, TLS and namespaced permissions.

## Tools

| Tool | Arguments | Behavior |
| --- | --- | --- |
| `submit` | `workflow`, `requestId`, `expiresAt`, `task` | Atomically creates the fixed workflow's Run or returns the exact matching retained Run |
| `status` | `id`, `uid` | Returns the caller-owned exact incarnation's lifecycle and optional untrusted result |
| `wait` | `id`, `uid`, `seconds` | Observes for 1–30 seconds; a nonterminal response requires another wait |
| `cancel` | `id`, `uid` | Sets cancellation to true with current UID and resource-version preconditions |

`requestId` is a stable operation key of 1–128 ASCII letters, digits, dots,
underscores or hyphens, beginning with a letter or digit. Task text is nonempty
UTF-8, at most 16,384 characters. `expiresAt` is an exact UTC RFC3339 timestamp
ending in `Z`, no more than one hour ahead when creating work. Preserve all
four fields after an uncertain response. Reusing a key with different task,
workflow, deadline or resolved authority conflicts.

Submission names are deterministic for the configured principal and key.
An identical retained Run can be returned after submission expiry. A missing
expired request cannot create work. Status and cancellation require the returned
UID, so a replacement with the same name is unavailable rather than a new target.
Waiting neither extends the worker deadline nor cancels it on client disconnect.

Before creating a Run, the service reads the workflow's live template and policy
and requires retention longer than 65 minutes. This exceeds the maximum replay
window with a margin. Retention begins after terminal completion. Ordinary
restarts and uncertain create responses converge on the existing Run; the service
has no separate receipt volume or forever-retained submission ledger.

Administrative deletion, cluster data loss or shortening a served policy's
retention during a replay window resets this guarantee. New submissions under
shortened retention are denied, but the service cannot undo administrative
deletion of prior work. This is bounded replay protection, not exactly-once
execution across destructive operations. Intentional new work needs a new key
and deadline. A scheduled routine must generate a distinct key for each intended
invocation and preserve it while recovering that invocation.

## Results and retention

Responses contain `id`, `uid`, `phase`, `cancellationRequested`, optional
timestamps, and optional `result: {"text": "...", "truncated": false}`.
Phase comes from actual worker process completion, independently of answer text.
An absent result means unavailable; available empty text is distinct. Present
truncated text as incomplete. Cancellation or deadline expiry can have no answer.

The controller captures a valid explicit termination-file result and persists
it with terminal status before deleting the Pod. The versioned envelope is bound
to the Run UID and limited to 3,584 encoded bytes, beneath Kubernetes' normal
termination-message cap. Invalid, missing or oversized envelopes are unavailable.
Only `terminationMessagePolicy: File` is accepted; fallback error logs never
become answers. Native launch adapters publish final text without implementing
the agent loop or using model text to decide lifecycle success.

Result text is untrusted and can contain sensitive scoped tool data. It lives in
`AgentRun.status.result` and is visible to authorized AgentRun readers. It does
not enter audit-safe summary, conditions or lifecycle audit events. Treat it as
data, and apply the same fixed workflow authority to follow-on work. The answer
survives Pod cleanup and controller restart, but expires with AgentRun retention.
After Run deletion, status returns unavailable; persistent assistant conversation
history has its own separate retention.

## Execution and inference ownership

The existing single global worker slot remains: several delegated jobs queue;
this integration does not implement concurrent worker teams or a DAG scheduler.
The persistent assistant does not occupy that slot. Its model account, memory
and channel credentials belong to the trusted assistant deployment. Worker
inference uses Sproozi's projected Run identity, gateway-owned provider credentials
and per-Run budgets. Coordinator inference is accounted for separately by its
configured provider; worker budgets do not cap the coordinator's spending.

The [orchestration ADR](../adr/0003-persistent-assistants-and-retained-results.md)
records the separation, alternatives, bounded replay and result tradeoffs.
