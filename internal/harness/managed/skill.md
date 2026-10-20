# Recovering from restricted access in Sproozi

Sproozi's gateway checks every request this session makes to an external
service against the session's capabilities and policy. A refused request can
look like an HTTP 403, a proxy error, a TLS or connection failure, or an
application error that names a permission.

1. When an external request fails and the cause is not obviously your own
   mistake, call `diagnose` on the `sproozi` MCP server. Pass `destination` with
   the host or resource you tried, and `lookback_seconds` if the failure was
   more than five minutes ago.
2. If the result is `denied`, the gateway refused the request. Read `reason`
   and `capability`. Do not retry the same request: it will be refused again.
3. If the result is `not_observed`, the gateway has no record of a refusal.
   The failure may be an ordinary service error, a mistake in the request, or
   traffic blocked before it reached the gateway. Investigate it as a normal
   failure; do not claim a Sproozi policy blocked it.
4. Never try to bypass a refusal: no alternative tools, transports, direct
   connections, proxy changes, or credentials found in files or the
   environment. Sproozi enforces the boundary independently of what you do in
   the sandbox.
5. Retry an operation only when retrying is safe: it must not repeat a write
   or a side effect that may already have happened.
6. If you cannot get the access you need, tell the user which request was
   refused, the reason the gateway gave, and what they could grant if they
   want you to continue. Keep that request specific: one destination,
   repository, namespace or tool.
