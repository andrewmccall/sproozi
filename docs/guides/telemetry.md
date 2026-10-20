# Collect traces, metrics and logs

Telemetry is an optional add-on to a working installation. Nothing in the
getting-started or installation guides needs it, and without it Sproozi runs
unchanged. Use this guide to send every Sproozi component's telemetry, and
optionally each managed harness's own telemetry, to one OpenTelemetry Collector. You then see
one trace per execution, covering its visits, moves and every gateway call:
model chats, MCP tool calls and the rest.

The [telemetry reference](../reference/telemetry.md) lists every span, metric,
log event, setting and limit.

## Before you start

You need an installation that already works without telemetry: a cluster set up
with [Install on Kubernetes and run your first Pod](install-kubernetes.md), a
home daemon set up with
[Move a live session between Docker and Kubernetes](session-mobility.md),
or both. The home daemon is `sproozi serve`; see [Install the CLI](install.md). You also need somewhere to send the
data. The examples use the Collector's `debug` exporter, which prints a summary
to the Collector's own log; replace it with your backend's exporter.

Sproozi sends only OTLP over HTTP with protobuf (`http/protobuf`), normally on
port 4318. Every OpenTelemetry Collector accepts it.

## Kubernetes

The `config/overlays/otel` overlay adds a Collector to `sproozi-system`, points
the controller, webhook and shared gateway at it, allows ingress to it only from
Sproozi's own pods, and turns on the gateway's harness relay.

1. Edit `config/overlays/otel/collector.yaml` and replace the `debug` exporter
   with your backend. Keep credentials in a Secret and reference them with
   `${env:...}`.
2. In the local overlay from the installation guide, change the base from
   `../../config/install` to `../../config/overlays/otel`. Your other patches
   still apply.
3. Build and apply the overlay as the installation guide describes, then wait
   for the Collector:

   ```sh
   kubectl rollout status -n sproozi-system deployment/sproozi-otel-collector --timeout=120s
   ```

4. To receive each managed harness's own telemetry, set the relay on its
   AgentRuntime. The gateway relays only for runtimes that ask:

   ```yaml
   spec:
     clientConfig:
       harness: codex
       nativeSession:
         telemetry: Relay
   ```

   To keep harness telemetry out entirely, remove `SPROOZI_HARNESS_TELEMETRY`
   from the overlay's `telemetry-env.yaml`.

5. If you run the profile service or task service in the cluster, add
   `OTEL_EXPORTER_OTLP_ENDPOINT=http://sproozi-otel-collector.sproozi-system.svc:4318`
   to their Deployments, and allow their pods in `allow-otlp-traffic.yaml`.

Check it: run any AgentRun, then read the Collector's log.

```sh
kubectl logs -n sproozi-system deployment/sproozi-otel-collector | grep -E 'Name +: (invoke_agent|transition AgentRun|chat )'
```

You should see the AgentRun's `invoke_agent` root span, its `transition AgentRun`
spans and a `chat {model}` span for each model call.

## Home daemon

Run a Collector next to the daemon. The example configuration listens on
`localhost:4318`:

```sh
docker run -d --name sproozi-otel -p 127.0.0.1:4318:4318 \
  -v "$PWD/examples/telemetry/collector.yaml:/etc/otelcol/config.yaml:ro" \
  otel/opentelemetry-collector:0.162.0@sha256:310a800ad69ee430e7c541796852a242c9c7db97aaad4daa5ccf843c525fbdb2 \
  --config=/etc/otelcol/config.yaml
```

Start the daemon with the endpoint in its environment:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
  sproozi serve --config /absolute/private/session.json
```

The daemon includes the Docker gateway and owns every visit, so this one
setting covers local visits, session operations and every local gateway
call. Start the profile service the same way to add its spans to the same
traces.

To relay the local harness's own telemetry, set `gateway.harnessTelemetry` in
the home configuration. The relay shares `gateway.mcpAuthority`, so set that
too, and make sure the inspection certificate covers it:

```json
{
  "gateway": {
    "mcpAuthority": "sproozi-mcp.internal:443",
    "harnessTelemetry": true
  }
}
```

Check it: start a session, then read the Collector's log.

```sh
docker logs sproozi-otel | grep -E 'Name +: (invoke_agent|start execution|chat )'
```

## What you can and cannot see

- Traces, metrics and logs never contain prompts, responses, tool arguments,
  tool results or credentials, and the harness relay strips content attributes
  even if a harness sends them.
- Sproozi never sends trace context to model providers, GitHub, MCP servers or
  other destinations.
- Interactive Claude Code does not join the execution trace. Its relayed spans
  form their own traces, which carry `sproozi.execution.id`.
- The relay works only if the harness's own exporter uses the sandbox's proxy
  and trust settings, as its model calls do.
