# WebSocket model traffic

Status: future architecture, not implemented.

## Why it matters

Codex prefers OpenAI's Responses API over a WebSocket when its model metadata
says so, which is the case for a Codex signed in to ChatGPT. The gateway
inspects HTTP requests and refuses an `Upgrade`, because it can't yet inspect,
count or cut off the frames inside a WebSocket. Codex then falls back to HTTPS
after each refused upgrade, so a session works but starts with several
refused attempts, and a client that can't fall back wouldn't work at all.

## What support would need

- Accept the upgrade only on a model route, after the same capability,
  budget and credential checks as an HTTP call.
- Parse the provider's frames: a `response.create` request starts a model
  call, and the response events carry usage, exactly as the SSE stream does.
  Each call reserves and settles as an HTTP call does.
- Apply refusal of hidden background work, monetary caps and the model call
  deadline per call, not per connection.
- Close the connection on revocation, cancellation, budget exhaustion and
  the run's deadline, and charge an estimate for a call cut off mid-response
  ([ADR 0028](../../adr/0028-streamed-model-responses-and-estimated-interrupts.md)).
- Scan outbound frames for a forwarded credential, as the leak guard scans
  request bodies ([ADR 0047](../../adr/0047-harnesses-sign-in-to-their-own-model-providers.md)).

## Open questions

- Whether providers document their WebSocket frames well enough to verify
  usage, or whether support follows each stock client release.
- Whether one connection may carry calls from more than one turn, and how a
  reservation spans them.
