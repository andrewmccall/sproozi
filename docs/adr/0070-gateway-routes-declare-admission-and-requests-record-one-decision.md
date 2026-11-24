# 0070: Every gateway route declares how it is admitted, and every request records one decision

Status: accepted
Date: 2026-11-20

## Problem and constraints

[ADR 0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md)
made the route the single source of its capability and moved the capability
check into the dispatcher. Three things stayed with each handler: spending the
budget, writing the refusal and writing the audit record. The gateway serves
seven handler families (Kubernetes, GitHub, packages, egress, named MCP, the
sign-in and account relays, model inference) behind two compositions
(`cmd/gateway` and `internal/gateway/docker`). Reading them showed what a route
got when its author left a step out:

- **A route with no capability was neither checked nor metered.**
  `Dispatcher.ServeHTTP` checked `Permits` and selected a meter only when the
  route named a capability. A route written as `gateway.Route{Handler: h}`
  served any authenticated execution. Only the gateway's own authority (named
  MCP and the control connection) relied on this, and the MCP handler repeats
  `Permits`, `BudgetLimits` and `Meter` itself, with its own statuses.
- **A capability route was metered only if its handler spent.** The meter in
  the request context is optional, and five handlers each call
  `SpendRequest` in their own place. The relays charge nothing by decision
  ([ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md)).
  Nothing recorded which routes are meant to be free.
- **A request was audited only if its handler logged.** The dispatcher's
  `budget_unavailable`, `allocation_denied` and handler-less
  `destination_denied` refusals and `LeakGuard`'s refusal left no record, so
  `diagnose` could not explain them. Kubernetes and named MCP discovery and
  the control connection are unrecorded by their handlers' design.
- **A new model authority would be served by the OpenAI handler.** The
  Kubernetes composition gives every `model.inference` semantic authority the
  OpenAI handler and then closes the provider table's own signed-in hosts. An
  authority added to `endpoints.SemanticAuthorities` without a provider-table
  entry would be inspected and forwarded with the OpenAI credential. The Docker
  composition builds only served routes and has no such default.

The same layering let the handlers drift. Budget exhaustion is answered as
403, 429 or 502, as text or JSON, and on a GraphQL pull request as `repository
not permitted by policy`; a ledger outage is reported as exhaustion by the
model, packages and egress handlers. `audit.Event` recorded `allowed` only,
and `allowed` itself is not consistent: a gateway stop is `true` while a model
call waits for its budget and `false` while it waits for headers, and a
provider's error is `false` on Anthropic and `true` on OpenAI.

Constraints:

- Refusal bodies stay per protocol. Model clients parse the OpenAI or
  Anthropic error shape; git, pip and kubectl print a text body. The dispatcher
  body of ADR 0016 is already parsed by both SDK families.
- The audit format is read outside the gateway: the denial journal,
  `test/acceptance`, `hack/verify/orchestration-kind`, `governance.py` and
  `mcp-live.py` all read `allowed`.
- Model calls stream: nothing may buffer or delay a relayed response.
- Per-request charges happen before the upstream call and are never refunded,
  and a model reservation always settles
  ([ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md)).
  Neither changes here.

## Decision

1. **A route declares how it is admitted.** `Route.Capability` is an
   `api.CapabilityKind`, and `Route.Admission` says who admits a request:
   - `AdmitCapability`, the zero value: the dispatcher checks and meters
     `Capability`, which must be set.
   - `AdmitSelected`: the handler selects a capability from the request and
     checks it itself; the route has no `Capability`. The gateway's own
     authority is the one such route: named MCP selects `mcp.<name>` from the
     path, and the control connection beside it needs none (ADR 0025).

   `Dispatcher.Validate` refuses a route or path route that has a handler and
   no valid declaration. Both compositions call it before serving, so a
   zero-valued `Route` with a handler stops the gateway at start-up instead of
   serving unchecked. The dispatcher also checks the declaration of every
   request's route, and refuses (403 `route_undeclared`) and records an
   undeclared one, so a composition that never called `Validate` still cannot
   serve it. A route without a handler is a reserved, closed authority and
   needs no declaration.

2. **One admission function.** `gateway.Admit(ctx, capability)` runs
   `Permits` and selects the meter, returning the metered context or a
   `Refusal`; `gateway.Refuse` records the refusal and writes the dispatcher's
   JSON body. The dispatcher uses them for `AdmitCapability` routes. The named
   MCP handler keeps its own copy for now: it checks the capability before the
   method and body and the meter after them, answers in text, and refuses a
   cost cap and a ledger error with 503 where the dispatcher answers 403.
   Adopting `Admit` would change those replies, which is a separate decision.

3. **Each dispatched request records one decision.** The dispatcher puts a
   decision in the request context. A handler records its audit event with
   `gateway.Record(ctx, writer, event)`, which writes through the handler's
   writer and marks the request decided. A nil writer writes nothing and
   leaves the request undecided; a handler built without a writer, which used
   to panic on `Log` in four handler families, now replies and gets the
   dispatcher's record. A request that is unrecorded by design says so with
   `gateway.Unrecorded(ctx, reason)`, without writing anything. The reasons
   are a closed list: a handler outside the gateway package can neither
   construct nor alter a reason, only take one of the three the package's
   functions return, and the decision keeps the reason so a test can see which
   site declared what. Any handler can still declare a listed reason on the
   wrong path; the `Edges` requests and the per-site record rows are the check
   for that. When a handler returns without either, the dispatcher writes the
   route's event with outcome `unrecorded`, naming the route and nothing from
   the request. A sandbox cannot cause one: a named MCP tool call the protocol
   handler refuses before the tool runs, and every path on a Docker MCP
   authority without a registry, keep their replies and are recorded. Handlers keep building
   their own events with `gateway.AuditEvent` and keep writing their own
   replies. The dispatcher and `LeakGuard` record their own refusals:
   `budget_unavailable`, `allocation_denied`, the handler-less
   `destination_denied` and `credential_leak_denied`. A `LeakGuard` record
   names the route, never the request's URL, headers or the credential.

   Unrecorded by design, pending an owner decision on whether to audit them:
   named MCP discovery (`initialize`, `tools/list`, `ping`, `server/discover`
   and notifications), Kubernetes `/mcp` discovery and its shape and argument
   refusals, and the control connection with its `diagnose` calls. Requests
   refused before identification stay unrecorded in `proxytransport`
   (ADR 0025).

4. **The audit record says what happened.** `audit.Event` gains `outcome`:

   | Outcome | Meaning |
   | --- | --- |
   | `served` | A response was relayed, whatever its status. `endReason` says how a model call ended. |
   | `refused_request` | The request was malformed or unsupported. |
   | `refused_policy` | The run, policy or a credential rule does not allow it. |
   | `refused_budget` | The allocation or budget is exhausted, or too many calls are pending. |
   | `unavailable` | The gateway could not account, build or authenticate the upstream request, or is stopping. |
   | `upstream_failed` | Nothing usable came back before anything was relayed. |
   | `cancelled` | The workload left before a response was relayed. |
   | `unrecorded` | The handler recorded nothing; only the dispatcher writes it. |

   The outcome follows the cause, not the status: an unsupported operation,
   option, method or query is `refused_request` whatever status answers it, a
   cost cap the endpoint cannot price is `refused_budget`, and a workload that
   leaves a tool call is `cancelled`, as it is for a model call. Every audit
   record carries one, including the proxy's refusal of a destination that is
   not enabled. Left as they are, and open: the named MCP "invalid tool
   scope" refusal (`refused_policy`, though it is the administrator's scope
   that is invalid), model row 17, a first flush that fails after the
   headers (`served`), and a provider's error, `served` and `allowed` on
   OpenAI but `upstream_failed` and not `allowed` on Anthropic.

   `allowed` keeps its value on every path. Which outcomes `allowed` and the
   denial journal follow is decided separately. The journal does not retain
   `unrecorded` records: they are not refusals, and `diagnose` would show them
   with no reason.

5. **One classification of a per-request charge.** `gateway.Spend(ctx)`
   wraps `SpendRequest` and returns `nil`, an error matching
   `ErrRefusedBudget` (exhausted, or a monetary cap on an endpoint without
   pricing) or one matching `ErrBudgetUnavailable` (any other ledger error),
   with the budget's own error text. Handlers keep their statuses and
   messages; the error lets each record the right outcome, so a budget
   refusal is no longer recorded as an upstream failure.

6. **One test enumerates the composed routes.** It builds the routes with
   the compositions' own construction (`composeGateway` in `cmd/gateway`,
   which `main` calls, and `composeRoutes` in the Docker gateway) and serves:
   - the Kubernetes composition for eight credential configurations (an
     OpenAI key, both keys, an Anthropic key, gateway-held sign-in tokens, a
     Copilot token, the harness's own sign-in with and without a gateway key,
     and ChatGPT) against five module selections, always with an MCP
     registry;
   - the Docker composition for every managed harness with a key, a token,
     its own sign-in and, for the OpenAI harnesses, ChatGPT, with GitHub and
     Kubernetes; and, for the malformed-request check alone, every harness
     with its own sign-in and no registry, GitHub or Kubernetes.

   For each route it asserts: a valid declaration; a request without the
   capability is refused and recorded; one served request with a one-unit
   budget spends it, unless the handler is listed as free with its record
   (the relays, ADR 0047; the control connection, ADR 0025); malformed and
   unusual requests leave no `unrecorded` record; and every `model.inference`
   authority is served by its provider-table route or by none. It also
   causes a refusal through the composed gateway and reads it back through
   the composed control connection's `diagnose`, so the control connection
   and the handlers share one journal. A separate
   sweep composes and validates, without serving, every configuration the
   loader accepts across auth mode, keys, tokens, seven module selections and
   the registry (632 today). ChatGPT is served with a stored-sign-in
   fake; an Anthropic harness with ChatGPT is not a configuration.

   The contract proves that each route's handler spends for one request. It
   does not prove every operation spends: a GitHub pull request charges each
   upstream request, and a Kubernetes watch is charged before it opens. The
   per-operation outcome records are the guard for the rest.

Building the handlers in one function moves the GitHub App token
provider's construction ahead of the ChatGPT session check, so when both are
broken the gateway now reports the token provider's failure first at
start-up.

The model call's settlement (ADR 0028) may later be rewritten as one `end`
step that derives the charge, end reason and audit fields from the call's
phase. That rewrite changes no outcome and is not part of this decision.

## Alternatives

- **Keep the handlers and add only the enumeration test.** It catches a
  missing capability, a route that does not spend and a model authority
  without a provider route, for every route that exists when the test runs.
  It does not record the unrecorded refusals, and a new refusal path inside an
  existing handler could still skip its record. Kept, as the guard for
  spending, which no type can enforce while handlers spend.
- **One `Refuse` writing one body, one gateway-owned `Charge`, and five
  outcomes** (refused-policy, refused-budget, upstream-failed, served,
  interrupted). One body would change what git, pip, kubectl and the model
  SDKs show, for no gain in what is checked. A shared `Charge` cannot express
  the model's reserve and settle. The five outcomes have no place for a
  malformed request, a gateway that cannot account or is stopping, or a
  workload that left, and "interrupted" duplicates `endReason`.
- **The dispatcher charges one unit per request.** Metering would hold by
  construction, but a request refused by the handler's own policy check would
  be charged, and GitHub pull requests, which spend once per upstream request
  including read-back, would be charged differently.
- **A metering transport on the upstream clients.** Named MCP's discovery,
  which costs no units, uses the same client path, and a handler that builds a
  request without the context would escape it.
- **An open admission with a reason.** `AdmitOpen` with a free-text
  `Route.Reason` naming the record that decided a route needs no capability.
  Removed: no route needs it, any non-blank reason passed, and a sign-in
  relay declared open would have served runs without `model.inference`.
- **`Record(ctx, event)` writing through the route's writer.** One writer per
  route, but handlers are built and tested with their own writer, and a route
  composed without one would silently drop every handler's records. The
  handler passes its writer instead; the route's writer serves the
  dispatcher's and `LeakGuard`'s own records and the fallback.
- **Wrap the response writer so the dispatcher records the status.** The
  `unrecorded` event would carry a status. It adds a writer on the streaming
  path that must keep `Flush` and `Unwrap`, for a record the enumeration test
  should never see.

## Tradeoffs and consequences

- No composed route's refusal body, status, charge, `allowed` value or `endReason` changes.
  The recorded outcomes of the unchanged code were committed as test data
  first (`testdata/outcomes.txt` beside each handler, `testdata/routes.txt`
  beside each composition), and each behaviour change shows its rows
  changing.
- Audit volume grows by the dispatcher's budget and handler-less refusals and
  `LeakGuard`'s refusals. These reach the denial journal, so `diagnose` can
  now explain them.
- Every audit line gains `outcome`. Readers of `allowed` are unaffected.
- Spending is proven per route by the contract and per operation only by the
  outcome records; a new operation needs its own record rows.
- Handlers still choose their statuses and bodies, so the budget answers
  stay as they are until each is changed deliberately. The outcome makes the
  remaining differences visible.
- The model authority default in `cmd/gateway` remains until that
  composition is built from the provider table; until then the enumeration
  test is the guard, and it records the default as a known exception.
- A handler can still record the wrong outcome. The recorded test data is
  the check.

## Revisit conditions

- A route needs to charge something other than one unit per upstream request
  or a model reservation.
- A route needs admission that neither the dispatcher nor a selected
  capability expresses, such as a capability chosen per JSON-RPC message in
  one batch.
- The owner decides whether discovery and control-connection requests are
  audited; each `gateway.Unrecorded` reason is then removed or kept.
- The audit stream is consumed by a system that needs a status on
  `unrecorded` events.
- `allowed` is retired in favour of `outcome`.

## Links

- Implementation: `internal/gateway/dispatch.go`, `internal/gateway/admit.go`,
  `internal/gateway/record.go`, `internal/gateway/spend.go`,
  `internal/gateway/outcome.go`,
  `internal/gateway/leak.go`, `internal/audit/event.go`,
  `internal/diagnose/journal.go`, `internal/diagnose/control.go`,
  `internal/endpoints/kubernetes`, `internal/endpoints/github`,
  `internal/endpoints/packages`, `internal/endpoints/destination`,
  `internal/endpoints/mcp`, `internal/endpoints/model`,
  `cmd/gateway/main.go`, `internal/gateway/docker/gateway.go`
- Tests: `internal/gateway/outcome_record_test.go`, the
  `outcome_record_test.go` of each endpoint package,
  `cmd/gateway/routes_record_test.go`,
  `internal/gateway/docker/routes_record_test.go`, the enumeration checks in
  `internal/gateway/routetest/routetest.go` run by
  `cmd/gateway/routes_contract_test.go` and
  `internal/gateway/docker/routes_contract_test.go`
- Explanation: [capabilities and gateways](../explanations/capabilities-and-gateways.md#endpoint-interface-and-spending)
- Reference: [Sproozi integration and diagnose](../reference/sproozi-integration.md),
  [gateway certificates and audit records](../reference/gateway-certificates.md#audit-records)
- Related: [ADR 0001](0001-mcp-capabilities-through-shared-gateway.md),
  [ADR 0016](0016-dispatcher-capability-enforcement-and-fresh-revalidation.md)
  (extended: admission covers selected and open routes, and the dispatcher's
  budget refusals are recorded),
  [ADR 0025](0025-managed-harness-integration-and-diagnose.md),
  [ADR 0028](0028-streamed-model-responses-and-estimated-interrupts.md),
  [ADR 0047](0047-harnesses-sign-in-to-their-own-model-providers.md),
  [ADR 0051](0051-interactive-sessions-may-run-without-a-model-budget.md)
  (a route with no configured budget is unbounded, not free)
