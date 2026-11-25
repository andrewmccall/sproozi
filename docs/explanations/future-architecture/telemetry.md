# Telemetry

Status: future architecture, not implemented. This note promises nothing and
gives no dates.

## What exists today

The gateway writes one JSON audit record per request to its standard output
([audit records](../../reference/gateway-certificates.md#audit-records)). A
record names the execution, the policy, the route and the outcome of the
request, and says whether the gateway allowed it. A model call also records the
model the provider accepted, the total tokens used, the input and output token
counts where the provider reports both, how the call was charged and how it
ended. A record never carries bodies, headers, credentials or query strings.
Collect the stream with your cluster's log pipeline.

`MODEL_DIAGNOSTICS=true` additionally writes bounded provider-error records to
standard error, apart from the audit stream
([gateway environment](../../reference/required-secrets-and-config.md#gateway-environment)).
Its text is the one place near content, so it must not feed telemetry.
The `diagnose` tool answers an execution's questions from a bounded in-memory
journal of recent refusals
([Sproozi integration and diagnose](../../reference/sproozi-integration.md)).

The controller manager can serve Prometheus metrics. The endpoint is off by
default; `--metrics-bind-address` turns it on, and the metrics and Prometheus
overlays expose it to a scraper
([manual PR demo](../../guides/manual-pr-demo.md)). The gateway serves only a
readiness check.

## What is missing

- Request traces, and latency or duration metrics, for the gateway.
- Any export to an OpenTelemetry collector, from any binary.

## Where it would attach in the gateway

Two places cover every request without touching each endpoint:

- `Dispatcher.ServeHTTP` in `internal/gateway` is the request handler. A
  request span and a duration measurement could start and end there.
- `gateway.Record` in `internal/gateway` is the one function through which
  handlers write each audit decision, including a model call's single ending
  ([ADR 0070](../../adr/0070-gateway-routes-declare-admission-and-requests-record-one-decision.md)).
  The outcome, model and token counts could be read there.

Two paths sit outside them. A connection to a destination the gateway does not
enable is refused and audited directly in `proxytransport`, and MCP tool calls would need their own site if
tool latency mattered.

The other binaries (controller, local service, remote executor, task service
and supervisor) have no such shared seam. Each would need its own design.

## What must be decided first

1. Who consumes the telemetry, and for what. The audit stream and
   `diagnose` tool may already answer the questions asked.
2. Which attributes may leave the cluster. The audit record excludes content
   such as prompts, credentials and header values
   ([audit records](../../reference/gateway-certificates.md#audit-records);
   [ADR 0070](../../adr/0070-gateway-routes-declare-admission-and-requests-record-one-decision.md)
   keeps request URLs, headers and credentials out of a leak-guard record).
   No ADR says whether identifiers such as session, execution, policy and
   model names, or free text such as a denial reason, may be exported. An
   allow-list would settle it.
3. The exporter's outbound connection. The gateway's upstream clients ignore
   ambient proxy settings
   ([ADR 0001](../../adr/0001-mcp-capabilities-through-shared-gateway.md)), so
   an exporter would need the same, and network policy would need egress to the
   collector.
4. Environment variables. Every variable the gateway reads is a listed
   constant with a row in the
   [gateway environment](../../reference/required-secrets-and-config.md#gateway-environment)
   table, and a test keeps the two in step. The OpenTelemetry SDK reads
   `OTEL_*` variables itself, one of which can carry credentials. The design
   must say whether those are honoured, listed or suppressed.
5. Dependencies. The module has the OpenTelemetry API and SDK core as indirect
   dependencies, but no OTLP/HTTP exporter. Adding one changes `go.mod` and
   goes through `make verify-deps`.

## A smallest first step

This is a suggestion. Write a decision record with an attribute allow-list,
then add traces and metrics for the gateway only, at the two places above.
Logs, harness telemetry relayed from the sandbox, and one trace per execution
across binaries are later steps, each with its own questions.
