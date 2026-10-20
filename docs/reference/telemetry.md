# Telemetry

Sproozi exports its own traces, metrics and logs with OpenTelemetry. It can
also relay a managed harness's own telemetry through the gateway. The
[Collect traces, metrics and logs](../guides/telemetry.md) shows how to collect both with one
OpenTelemetry Collector. [ADR 0025](../adr/0025-opentelemetry-from-audit-and-execution-traces.md)
records why it works this way.

Telemetry never contains prompts, model responses, tool arguments, tool
results, shell output or credentials. Audit records already exclude them, and
call telemetry is derived from audit records.

## Configuration

Telemetry is optional and off by default. Every component reads the standard
OpenTelemetry environment. Nothing is exported until an endpoint is set; an
unset endpoint never means `localhost`. A setting Sproozi cannot honour is
reported once on stderr, and the component runs without telemetry.

| Variable | Effect |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector OTLP/HTTP base URL, for example `http://collector:4318`. Enables all three signals. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `_METRICS_ENDPOINT`, `_LOGS_ENDPOINT` | Full URL for one signal. Setting one enables that signal. |
| `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` (default) or `none`. Any other value turns telemetry off with a warning. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | Only `http/protobuf` is supported. `grpc` or `http/json` turns telemetry off with a warning. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_COMPRESSION`, `_CERTIFICATE` | Standard exporter settings. |
| `OTEL_SDK_DISABLED=true` | Turns all signals off. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Override the service name or add resource attributes. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Standard samplers. See [Sampling](#sampling). |
| `OTEL_METRIC_EXPORT_INTERVAL` | Metric export interval in milliseconds; default 60000. |

Each binary has a fixed default service name:

| `service.name` | Process |
| --- | --- |
| `sproozi-gateway` | Shared Kubernetes gateway |
| `sproozi-home` | `sproozi serve`, including its Docker gateway |
| `sproozi-controller` | Controller manager |
| `sproozi-webhook` | Webhook receiver |
| `sproozi-session-profile` | Profile service |
| `sproozi-tasks` | Task service |

Other binaries, including the CLI client commands and `native-session`, export nothing.

## Execution traces

Each execution has one trace. Its trace ID and root span ID are derived from
the execution ID (`executionId`), so every process that serves the execution
joins the same trace without coordinating. A session with several executions
has one trace per execution; use `sproozi.session.id` to find them all.

| Span | Kind | Emitted by | When |
| --- | --- | --- | --- |
| `invoke_agent` | internal | home daemon, or controller for an AgentRun without a session | Once, when the execution is created. It is the root span and marks creation; its children give the trace's extent. |
| `start execution`, `move execution`, `finalize execution`, `cancel execution` | internal | home daemon | One session operation. |
| `stop visit`, `prepare visit`, `activate visit` | internal | home daemon | One stage of one visit, under its operation. |
| `profile prepare`, `profile activate`, `profile stop`, `profile inspect`, `profile abort` | server | profile service | One placement request from the home daemon, under its stage span. |
| `transition AgentRun` | internal | controller | One persisted AgentRun phase change. |
| `{METHOD} {host:port}` | server | gateway | One inspected request. Model requests are renamed `chat {model}`. |
| `tools/call {tool}` | client | gateway | One approved MCP tool call made upstream, under its request. |

Common attributes:

| Attribute | Meaning |
| --- | --- |
| `sproozi.execution.id` | Execution ID. On a plain AgentRun it is the AgentRun UID. |
| `sproozi.session.id` | Durable session ID; equals the execution ID when the execution is its own session. Home daemon spans only. |
| `sproozi.visit.id`, `sproozi.visit.epoch`, `sproozi.placement` | Visit (`allocationId`), its epoch and its placement. |
| `sproozi.run.id`, `sproozi.run.uid`, `sproozi.backend` | The gateway's view of one visit, as the audit record's `runID` and `runUID`. |
| `sproozi.policy.name` | Policy that governed the gateway decision. |
| `sproozi.capability`, `sproozi.tier`, `sproozi.service`, `sproozi.operation`, `sproozi.resource` | Audit decision fields. |
| `sproozi.allowed`, `sproozi.deny_reason` | Audit outcome. |
| `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | Model calls, after the provider accepts the request. |
| `gen_ai.agent.name`, `gen_ai.conversation.id` | Root span: harness and session. |
| `mcp.method.name`, `gen_ai.tool.name` | MCP requests and approved tool calls. |

The OpenTelemetry GenAI and MCP conventions are still in development. Sproozi
uses the names that GitHub Copilot emits today.

### Trace context

| Boundary | Behaviour |
| --- | --- |
| Sandbox to gateway | `traceparent`, `tracestate` and `baggage` from the workload are removed before any upstream request. They never become a parent. |
| Gateway to providers, GitHub, MCP servers and other destinations | No trace context is sent. |
| Home daemon to profile service | W3C trace context is sent after authentication, so profile spans nest under the home daemon's spans. |
| MCP client to task service | `params._meta.traceparent` from the authenticated caller becomes the parent. |
| Managed harness | Receives `TRACEPARENT` set to the execution root when the [harness relay](#harness-relay) is on. |

### Sampling

Execution spans have a sampled remote parent, so the default parent-based
sampler records every span of an execution. Because the trace ID is the same in
every process, a ratio sampler that ignores the parent, such as
`OTEL_TRACES_SAMPLER=traceidratio`, keeps or drops whole executions
consistently.

## Metrics

| Metric | Type, unit | Attributes |
| --- | --- | --- |
| `sproozi.gateway.request.duration` | histogram, `s` | `sproozi.capability`, `sproozi.tier`, `http.response.status_code` |
| `sproozi.gateway.decisions` | counter, `{decision}` | `sproozi.capability`, `sproozi.tier`, `sproozi.service`, `sproozi.allowed` |
| `gen_ai.client.token.usage` | histogram, `{token}` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.token.type` (`input` or `output`) |
| `mcp.client.operation.duration` | histogram, `s` | `mcp.method.name`, `gen_ai.tool.name`, `sproozi.capability`, `error.type` on failure |
| `mcp.server.operation.duration` | histogram, `s` | Task service: `mcp.method.name`, `gen_ai.tool.name`, `gen_ai.operation.name`, `error.type` on failure |
| `sproozi.webhook.decisions` | counter, `{decision}` | `sproozi.template.namespace`, `sproozi.template.name`, `sproozi.operation`, `sproozi.allowed` |
| `sproozi.telemetry.relay.requests` | counter, `{request}` | `sproozi.telemetry.signal`, `sproozi.telemetry.outcome` (`forwarded`, `rejected`, `failed`) |

Metric attributes never include execution or run identifiers. Tool names come
from the approved upstream catalogue, and a model name is recorded only after
the provider accepts it and is omitted when longer than 128 bytes.

The controller's existing Prometheus metrics endpoint is unchanged.

## Logs

Each audit record also becomes an OTel log record, correlated with the span the
decision was made in. Stdout audit output is unchanged apart from new fields.

| Event name | Severity | Body |
| --- | --- | --- |
| `sproozi.gateway.decision` | `INFO` when allowed, `WARN` when denied | The audit `operation` |
| `sproozi.webhook.decision` | as above | The webhook `operation` |

Gateway audit records gain these JSON fields when they apply:

| Field | Meaning |
| --- | --- |
| `traceID`, `spanID` | The decision's span, for joining stdout audit to traces. |
| `model` | Model the provider accepted. |
| `inputTokens`, `outputTokens` | Split of `tokensUsed` when the provider reports both. |
| `usageSource` | How a model call was charged: `provider` (the provider's reported usage), `estimated` (an [estimated charge](../../GLOSSARY.md#estimated-charge) for an interrupted call), `reservation` (the whole reservation), `recovered` (the ledger had already settled the call's expired lease at its recovery charge, so this call's own charge wasn't recorded; `tokensUsed` is 0) or `unsettled` (the ledger refused the charge and recorded nothing for the call; `tokensUsed` is 0). See [interrupted calls](../guides/model-provider-auth.md#interrupted-calls). |
| `endReason` | How a model call ended: `completed`, `client_cancelled`, `deadline`, `gateway_restart` (the gateway stopped and drained the call), `upstream_error`, `budget_cut`, `parse_limit` or `exceeded` (usage above the call's own reservation, charged up to what the budget leaves after settled usage and other pending reservations). See [how each end reason is charged](../guides/model-provider-auth.md#interrupted-calls), [usage above the reservation](../guides/model-provider-auth.md#usage-above-the-reservation) and [gateway restarts](../guides/model-provider-auth.md#gateway-restarts). |

A reservation that the ledger recovers after a gateway was killed has no audit
record, so it doesn't appear here. The process that recovers it, the gateway,
the home daemon or the service that seals the account, saves the recovery and
then writes `Settled abandoned model reservation` to its own log, or
`Settled abandoned budget reservation` for a GitHub, package, Kubernetes,
egress or MCP request's lease, with
`account`, `endReason` `gateway_restart`, `usageSource` `estimated`, `units`
and `costMicros`. When the shared gateway's or home daemon's periodic sweep
recovers it, `account` is the budget ConfigMap or ledger file name. After an
upgrade from a gateway from before leases, the legacy pass charges the whole
reservation of any pending reservation left without a lease and logs `Settled
legacy budget reservation` with `usageSource` `reservation` and a
`reservations` count. These log lines, like `Charged estimated model usage`,
are not exported as OTel log records.

## Harness relay

The relay forwards a managed harness's own OpenTelemetry export to the same
collector. The harness reaches it only through the gateway, as
`https://<gateway authority>/otlp/v1/{traces,metrics,logs}`.

| Setting | Where | Effect |
| --- | --- | --- |
| `SPROOZI_HARNESS_TELEMETRY=relay` | Shared gateway environment | Serves the relay and forwards to `OTEL_EXPORTER_OTLP_ENDPOINT` with `OTEL_EXPORTER_OTLP_HEADERS`. Without an `http(s)` endpoint the gateway logs a warning and runs without the relay. |
| `clientConfig.nativeSession.telemetry: Relay` | [AgentRuntime](agentruntime.md#managed-native-sessions) | Configures the harness in each managed native session to export to the relay. |
| `gateway.harnessTelemetry: true` | Home configuration | Does both for the home daemon. Requires `gateway.mcpAuthority`, which the relay shares, and a Codex or Claude Code harness. Forwards to the daemon's `OTEL_EXPORTER_OTLP_ENDPOINT`; without it the daemon logs a warning and neither serves the relay nor configures the harness. |

What Sproozi sets in the harness:

| Harness | Configuration |
| --- | --- |
| Codex | `-c` overrides for `otel.exporter`, `otel.trace_exporter` and `otel.metrics_exporter` (OTLP/HTTP, binary protocol), `otel.log_user_prompt=false`, `otel.environment="sproozi"`, and `TRACEPARENT`. `nativeSession.options` cannot set any `otel` key. |
| Claude Code | `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`, OTLP exporters for metrics, logs and traces over `http/protobuf`, and `TRACEPARENT`. Prompt, response and tool detail switches stay unset. Interactive Claude Code does not read `TRACEPARENT`, so its spans form their own traces; their resource still carries `sproozi.execution.id`. |

The relay treats every export as untrusted:

- It requires the authenticated gateway identity of the visit.
- It accepts only `POST` with `Content-Type: application/x-protobuf`, optionally
  gzip-encoded, up to 4 MiB before and after decoding.
- It removes every `sproozi.*` resource attribute the harness sent, then adds
  the visit's identity and `sproozi.telemetry.source=harness`.
- It removes attributes whose last name segment is, or ends in `_` followed by, one of:
  `prompt`, `prompts`, `messages`, `instructions`, `definitions`, `arguments`,
  `args`, `input`, `output`, `result`, `results`, `content`, `contents`,
  `body`, `command`, `parameters`, `query`, `response`, `responses`,
  `completion`, `authorization`, `cookie`, `token`, `secret`, `password`,
  `email`, `api_key`, `apikey` or `header`, and every `http.request.header.*`
  and `http.response.header.*` attribute.
- It truncates every string to 1024 bytes and empties byte values.
- It returns the collector's `429` or `503` so the harness exporter backs off,
  and `502` for any other collector failure.

The name filter is defence in depth. The primary control is that Sproozi
configures each harness with content capture off. Individual relay exports are
not audited; the request span and `sproozi.telemetry.relay.requests` record them.

Harness export depends on the harness's own exporter using the sandbox's proxy
and trust settings. Ordinary, unmanaged sandboxes are not configured by Sproozi;
an administrator can point a harness at the relay URL through the pod template.
