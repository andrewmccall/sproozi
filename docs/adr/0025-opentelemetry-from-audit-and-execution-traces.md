# OpenTelemetry from audit records and execution traces

Date: 2026-10-18

Status: accepted

## Context

Operators need to see what a session did: which visits it made, which model,
MCP and other calls passed the gateway, how long they took and what they cost.
Sproozi already writes a secret-free JSON audit record for every gateway
decision, but only to stdout, with no timing, no correlation to sessions and no
metrics. Operators want one OpenTelemetry Collector to gather everything.

Constraints:

- Telemetry crosses a trust boundary to the operator's backend. It must keep the
  audit record's rule: no prompts, responses, tool arguments, tool results,
  shell output or credentials.
- A session can last days and spans several executions, visits and processes:
  the home daemon, profile service, controller and gateways. Durable
  journals decode strictly, so a new field is a schema change.
- The sandbox is untrusted and can reach only the gateway. Its own trace headers
  must not become parents or travel to providers.
- Harnesses emit their own OpenTelemetry. Codex and Claude Code read
  `TRACEPARENT` and export over OTLP. GitHub Copilot turns on its child
  harnesses' telemetry through environment variables, keeps content capture off
  by default and uses the GenAI conventions (`invoke_agent`, `chat`,
  `execute_tool`).
- The GenAI and MCP semantic conventions are unreleased and still changing.

## Decision

**Configure every component with the standard `OTEL_*` environment and export
only OTLP/HTTP protobuf.** `internal/telemetry` installs trace, metric and log
providers when an endpoint is set and is a no-op otherwise. Telemetry is an
optional add-on: an unsupported protocol or exporter, or a relay without a
collector, is reported once and the component runs without it, so a stray
shell-wide `OTEL_*` setting never stops a gateway or daemon. Copilot, Claude
Code and OpenCode use the same environment contract.

**Derive call telemetry from the audit record.** `gateway.AuditEvent` binds the
request context. `telemetry.AuditWriter` wraps the stdout logger: each record
annotates the span it was made in, becomes an OTel log record and updates the
decision and token metrics, then reaches stdout with its `traceID` and `spanID`.
Handlers keep one exit path for audit and gain telemetry without new code at
each exit. The audit record gains the accepted `model` and the input and output
token split, which are audit-safe.

**Open one server span per inspected request in the dispatcher** and record
request duration there. MCP tool calls get a child `tools/call {tool}` span,
because tool latency differs from connect and discovery.

**Give each execution one trace.** Its trace ID and root span ID are derived from
a hash of the execution ID, so the home daemon, profile service, controller and
gateways join one trace without shared state. The owner emits the root
`invoke_agent` span when it creates the execution: the home daemon for
session executions, and the controller for an AgentRun without a session. The
gateway carries the execution ID in its grant for this purpose only. The session
ID is an attribute, because a trace that spans days is impractical in trace
backends.

**Keep trace context inside Sproozi.** The proxy removes the workload's
`traceparent`, `tracestate` and `baggage`. Sproozi sets no global propagator, so
no upstream receives context. Only the authenticated home-to-profile call and
the task service's MCP `params._meta` carry context.

**Relay harness telemetry through the gateway, opted in at both ends.** The
gateway serves `/otlp/v1/{signal}` on its own authority, beside named MCP
servers, when `SPROOZI_HARNESS_TELEMETRY=relay`. A runtime opts its managed
harness in with `nativeSession.telemetry: Relay`. Sproozi configures Codex and
Claude Code with content capture off and passes `TRACEPARENT`. It reserves Codex
`otel.*` options. The relay overwrites `sproozi.*` resource attributes with the
authenticated identity, marks the data `sproozi.telemetry.source=harness`,
removes content-bearing attribute names and bounds every value.

**Use the GenAI and MCP names that Copilot emits today.** These are
`gen_ai.usage.*` and `gen_ai.client.token.usage`, rather than names that exist
only on the conventions' main branch.

## Alternatives

- **Instrument each handler directly.** This gives precise spans, but each
  handler has many denial exits and the audit record already describes them.
  Rejected because two parallel records per decision could drift.
- **`otelhttp` client spans on upstream transports.** These add upstream timing,
  but propagate context unless propagators are carefully removed. They are not
  needed while the request span covers the call. Deferred.
- **Contrib `autoexport` for gRPC, console and Prometheus exporters.** This gives
  more choices, but upgrading the Kubernetes dependencies broke the build, and
  one protocol is enough for any Collector. Rejected.
- **One trace per session.** Simple to browse, but traces would stay open for
  days and grow without bound. Rejected. **One trace per request** was rejected
  because it gives no timeline.
- **Store the execution start time** for an accurate root span. This needs a
  durable journal schema change and breaks downgrade. Rejected. The root marks
  creation instead.
- **A new `CapabilityKind` for the relay.** This would allow per-policy control,
  but changes the CRD enum and policy model for operator-owned plumbing. The
  gateway switch and runtime opt-in suffice today.
- **Let harnesses reach the collector directly.** This needs new sandbox egress,
  bypasses identity stamping and trusts self-reported identity. Rejected.

## Consequences

- One Collector receives Sproozi's traces, metrics and logs, and optionally
  harness telemetry, with no new egress from sandboxes.
- Telemetry detail is bounded by the audit record. A new audited field must be
  audit-safe before it can appear in telemetry.
- The root span has no duration. Trace backends show execution extent from its
  children.
- The relay's attribute filter is heuristic, so content capture off in the
  harness remains the primary control. A harness that puts content under an
  unlisted name would pass it through. Strings are capped at 1 KiB.
- Interactive Claude Code ignores `TRACEPARENT`, so its relayed spans form
  separate traces. They still carry `sproozi.execution.id`.
- Harness export through the relay depends on each harness's exporter using
  the sandbox proxy and trust settings. Unit tests and a real Collector check the
  relay; this change does not exercise a live harness exporting through it.
- Metric names follow conventions that are still in development and may be
  renamed.

## Revisit when

- `semantic-conventions-genai` tags a release. Adopt its names, such as the
  per-direction `gen_ai.client.inference.*` metrics.
- An operator needs gRPC or a pull-based Prometheus endpoint.
- A harness is seen exporting content under a name the filter misses, or
  operators need per-policy control of harness telemetry.
- Journals gain an execution start time for another reason.

## References

- [Telemetry reference](../reference/telemetry.md) and
  [guide](../guides/telemetry.md)
- `internal/telemetry`, `internal/gateway/telemetry.go`,
  `internal/endpoints/otlp`, `internal/session/telemetry.go`,
  `internal/controller/telemetry.go`, `internal/tasks/telemetry.go`
- [ADR 0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md)
  for dispatcher audit records
