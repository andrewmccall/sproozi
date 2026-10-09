# 0002: Native CLI harnesses and model protocols

Status: accepted
Date: 2026-10-07

## Problem and constraints

The runtime already creates disposable Pods and observes process completion,
while Codex MCP configuration is rendered at admission. Supporting another CLI
must preserve those responsibilities and the existing run identity, capability
policy and gateway-owned credentials. A native configuration file alone does
not establish model protocol or completion compatibility.

Claude Code uses Anthropic Messages rather than OpenAI Responses. Anthropic's
normal input, cache read and cache creation counts are disjoint, and its SSE
completion and usage differ from OpenAI. Two provider routes must share one
`model.inference` account. OpenCode can use the existing OpenAI protocol, but
its installed one-shot client's terminal behavior needs direct verification.

## Decision

Extend `AgentRuntime.spec.clientConfig.harness` with `claude-code` and `opencode`
and render each CLI's native named MCP connections in `internal/harness`.
Keep launch configuration administrator-owned. Use stock one-shot CLIs in the
existing Kubernetes Pod, fresh client state and separate trusted instruction
and untrusted request files. Native configuration supplies connectivity; the
requested grant and live policy remain authority.

OpenCode 1.15.1 demonstrably returns zero after a denied inference and can omit
terminal stdout events. Supply an immutable Python launch adapter that invokes
the stock loop, forwards termination, and verifies its native session export.
Fresh state, one root session and a completed final response determine the
adapter's exit status. The controller continues to observe only container exit;
the adapter does not assess task quality or implement agent execution.

Add a concrete Anthropic constructor to the existing model Handler. A private
fixed protocol selector chooses native request and usage parsing. The shared
handler continues to own capability checks, reservation, forwarding,
settlement, redacted audit and denial. It never translates the model protocols
or exposes a general provider plugin interface.

Reserve `api.anthropic.com:443` as Semantic `model.inference` even when disabled.
An optional gateway API key activates the handler and requires the matching
inspection certificate SAN. The gateway installs the trusted API key after
filtering client headers; upstream redirects are refused. OpenAI and Anthropic
routes consume the existing account keyed by run UID and capability.

Validate complete Messages JSON or bounded buffered SSE before returning output.
Account for normal input, cache reads, distinct cache-write TTLs and output.
Extend administrator pricing with optional TTL rates, preserving old OpenAI
rows. Deny known unsupported billable options under monetary caps. Unknown
model pricing denies Anthropic requests before inference; missing consumed
category pricing after inference forfeits the reservation. Monetary-capped
requests explicitly select `standard_only` instead of the provider's default
automatic Priority Tier selection. Cache checks inspect protocol metadata,
leaving arbitrary tool data untouched; nullable optional usage fields preserve
known cumulative counts. Required input/output and terminal evidence stay strict.

For Anthropic, reserve the run's remaining capped capacity because an output
limit cannot bound total token use. Add an atomic budget forfeiture operation
for ambiguous consumption, retaining both token and dollar capacity. The shared
handler uses it for malformed successful responses and uncertain transport
failures. Keep OpenAI's existing output-bound admission in this change.

## Alternatives and reasons

A separate Anthropic proxy handler has a small public interface and would avoid
branches in the existing handler. It duplicates security and accounting policy,
so a later fix could reach only one provider. The selected design keeps that
policy in one place while native parsers own protocol details.

A generic provider interface could hide protocol selection, but would expose
request inspection, headers, usage, pricing and error stages to each adapter.
Two concrete protocols do not justify those callbacks. A protocol translation
layer would alter tools, thinking and stop semantics and still need provider
billing knowledge. A second service would duplicate the transport trust boundary.

One generic MCP file would be simpler to generate, but stock clients use
different native formats. A repository-owned agent loop would allow uniform
session semantics but would replace each CLI's model and tool behavior. Native
formats and narrow launch adapters keep each stock loop intact.

Using OpenCode's raw exit code would admit the observed false success. Parsing
stdout alone would reject completed invocations whose terminal event was lost.
Native session export supplies the completed response and error state that
neither signal reliably exposes in the tested version.

## Accepted costs and consequences

Capped Anthropic requests serialize within a run. Buffered SSE adds latency and
retains the existing body and timeout bounds. A disconnected or malformed
provider response can consume the remaining reservation even if actual usage
was smaller. These choices prefer withheld output and conservative accounting
to reusing capacity that may already have been consumed.

Token/cost reservations do not prove a hard provider-spending ceiling. OpenAI's
existing output-only reservation can underestimate input-plus-output usage;
Anthropic cannot undo upstream consumption that exceeds the admitted capacity.
Explicit one-hour cache requests and hosted billable extras remain outside
monetary-capped acceptance. Supporting CLI configuration does not establish
subscription authentication, managed agents or external execution servers.

Native CLI failure semantics vary. Each supported launch path must produce a
reliable container status after a bounded invocation, using a narrow adapter
where the stock CLI's process status is insufficient. The adapter may normalize
completion but must not grant authority, interpret task quality as lifecycle
success, or implement the agent loop.
OpenCode's adapter adds Python 3 to the runtime image and requires disposable
session state. Export inspection is bounded; persistent or ambiguous sessions
fail instead of silently resuming work.

## Evidence and reconciliation

The retained task grounding traced the authenticated proxy, dispatcher meter,
model handler and pricing. Two structurally distinct design candidates were
compared against authority preservation, policy ownership, protocol correctness,
regression cost and verifiability. Independent cross-judging and root synthesis
selected the shared handler and incorporated atomic forfeiture and strict
provider header filtering from the separate-handler alternative. All seats used
the available parent model, so this review has no cross-family model diversity.

Native configuration checks exercised installed Codex, Claude Code and OpenCode
with disposable state. Provider and ledger tests cover terminal usage, cache
counts and pricing, overflow, shared caps, credential replacement, reserved
routes and malformed outcomes. Stock-process fixtures use local mocked providers
through the production inspected-TLS transport. They do not establish paid
Anthropic billing or deployed CNI containment. The existing Kind proof remains
Codex-specific.

Implementation: `internal/harness`, `internal/kubernetes/contract.go`,
`internal/endpoints/model`, `internal/modelauth`, `internal/budget`,
`cmd/gateway/main.go` and the [stock runtime examples](../../examples/harnesses/README.md).
Current behavior and verification limits belong in the [harness reference](../reference/harnesses.md),
[agent contract](../reference/agent-contract.md),
[runtime reference](../reference/agentruntime.md) and
[system architecture](../explanations/system-architecture.md).

## Revisit conditions

Revisit admission when measured concurrency needs justify a trusted total-token
bound. Revisit buffering if real clients cannot tolerate its latency or bounded
response sizes. Add token-count/discovery endpoints only when a stock-client
failure establishes their necessity and separate accounting semantics.

Revisit the two-protocol design when another provider demonstrates stable common
protocol behavior. Extend priced options only with complete trusted usage and
pricing evidence. Managed loops, persistent sessions, remote executors and
additional execution runtimes require separate lifecycle and authority decisions.
Remove or simplify the OpenCode adapter when a pinned stock release demonstrates
reliable success and denial exit statuses with retained acceptance evidence.


## Subsequent decision

[ADR 0003](0003-persistent-assistants-and-retained-results.md) adds the stock
Hermes worker and a separate persistent coordinator, fixed-workflow task MCP
interface, retained answers and full deterministic-provider Kind acceptance.
The original evidence above remains the record for this decision's first change.
