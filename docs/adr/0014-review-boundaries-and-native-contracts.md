# 0014: Bound shared gateways and unify trusted native contracts

Status: accepted, 15 October 2026.

## Context and constraints

The October source review found authorization-document drift in GitHub reads,
ambiguous absent/unknown sandbox state, unbounded gateway admission and authority
lookups, and divergent native launch/reply handling. CLI discovery and errors
also obscured the distinction between a durable session, a bounded execution and
its current allocation. These are execution contracts shared across current
implementations, rather than differences in installation convenience.

Existing [runtime ownership](0009-runtime-backends-and-sealed-accounting.md),
[managed authority](0010-managed-session-authority-and-admission.md) and
[native completion](0011-native-session-compatibility-and-completion.md) decisions
remain constraints. Neither a timeout nor a native acknowledgement establishes
physical writer termination or known accounting. Preserve the stock Codex and
Claude terminal paths and their selected native UUID/checkpoint semantics.

## Decision and reasons

Authorize the document actually forwarded: reconstruct the bounded supported
GitHub GraphQL read selections with fixed argument/variable bindings. Refuse
unsupported documents before obtaining provider credentials. Kubernetes reads
use exact supported group/version/resource tuples, remove the full impersonation
header family and refuse upstream redirects. Redirect refusal belongs in the
shared handler so Kubernetes and Docker use the same execution rule.

Represent successful absence separately from an uncertain existing Pod. During
release recover the deterministic sandbox only with the immutable Run identity;
retain network containment until the sandbox is absent or terminal. Trusted
runtime templates must drop ALL capabilities without additions, cannot share the
process namespace, and can supply only bounded scratch space or explicit public
configuration. The controller retains exclusive responsibility for projecting
workload credentials. Administrators remain trusted for template contents and
must not place secrets in literal environment values or public ConfigMaps.

Use one outer gateway server constructor for TLS, HTTP/1 and parsing limits in
both backends. Bound raw gateway sockets before TLS, authenticated sessions per execution, and
authority lookup workers. A lookup times out independently and fails closed; its
worker retains admission until it really exits. Existing sessions retain their
absolute deadline and live revocation checks. MCP negotiates and executes within
one bounded setup/work context, with independently bounded SDK cleanup. Setup
remains distinct from charged tool attempts. These controls bound admitted work;
they do not make uncooperative dependencies stop on cancellation.

Keep native launch preparation and reply validation in `internal/harness`, used
by both placements and the native supervisor. Fixed environment, approved client
bindings, task intent and rollout limits must have one implementation. Aggregate
checkpoint size is a separate limit. Validate operation, state and selected
thread together, with an explicit first-launch variant before the genuine UUID
is bound. Backend provisioning, token acquisition, mounts and physical fences
remain concrete backend responsibilities. Shared replies are untrusted native
diagnostics, never receipts or authority.

Public `id` consistently identifies the durable session. `executionId` names a
bounded execution, `allocationId` names an allocation, and `incarnation` carries
the backend resource binding. List discovery returns one labelled bounded
catalogue with per-executor availability. Work HTTP, MCP and clients preserve the
existing small rejection vocabulary and safe recovery guidance. They do not
expose raw provider errors or collapse unavailability into absence.

## Protocol choice

Reuse MCP's published SDK decoding at the MCP boundary. Do not add a parallel
JSON-RPC validator. For native control, use one private typed contract rather
than independent producer and consumer descriptions.

ACP session/prompt/cancel methods describe structured conversations. Its terminal
methods ask the client to execute a command; they do not carry the stock TUI's
keyboard/resize attachment. Adopting ACP here would replace the interaction and
native completion observer and require explicit adapter/native UUID mapping.
AHP is a viable host-facing terminal/catalogue design. Its published terminal
actions use string data and dispatch sequence acknowledgements, however, rather
than arbitrary byte segments, actual partial-written counts and native replay
gaps. Its standard lifecycle also does not replace Sproozi's admission, gate,
quiescence or physical stop/accounting facts.

Two complete alternatives were compared: an AHP host around the existing PTY,
with negotiated byte-specific extensions, and an ACP conversation backend with
a renderer and native identity mapping. The former preserves today's terminal
experience and is the preferred direction when adding external host clients.
For the current review fixes, adding host initialization, subscription, claim and
replay state only to wrap the existing private CLI transport would enlarge the
surface without eliminating its necessary byte/control extensions. Keep the
private contract and fix its duplication now. No AHP or ACP interoperability is
claimed, and no unused SDK dependency or adapter is shipped. This is a concrete
semantic fit decision, not a claim that the standards are impossible to adopt.

Primary contracts: [ACP terminals](https://agentclientprotocol.com/protocol/v1/terminals),
[ACP prompt turns](https://agentclientprotocol.com/protocol/v1/prompt-turn),
[AHP terminals](https://microsoft.github.io/agent-host-protocol/guide/terminals.html)
and [AHP/ACP relationship](https://microsoft.github.io/agent-host-protocol/guide/ahp-and-acp).
The review inspected the AHP Go schemas at revision
`cb6ba6189ed861e7e5ae61fab92d6e68e8505182` as well as the actual native attachment.

## Alternatives and accepted costs

- Retain separate backend launch and response rules. This minimizes immediate
  edits but perpetuates different task, size and acknowledgement behavior. Reject
  it because both backends already run the same supervisor.
- Introduce a universal runtime/phase framework. It could centralize more code
  but would hide different physical stop evidence and authority owners. Preserve
  the existing `session.Placement` seam instead.
- Depend on HTTP request cancellation alone. It uses fewer workers but cannot
  bound a callback that ignores context. Retain bounded lookup admission even
  after callers time out; exhausted capacity fails closed and needs recovery.
- Allow arbitrary runtime volume types and rely solely on namespace Pod Security.
  This permits more administrator customization, but does not express Sproozi's
  public-configuration/credential boundary. Accept the tighter template contract
  and migration cost for custom runtimes.
- Return the first discovery error or stream independent arrays. Both simplify
  fan-out but hide usable sources or require a special parser. Accept a compact
  catalogue and explicit partial availability instead. Public JSON identity and
  list changes require consumers to migrate with this change.

Fixed default admission limits may need tuning after measured production load.
Limits protect capacity rather than guarantee fairness between unauthenticated
clients. Bounded cleanup can outlive a cancelled request. Do not claim hard
provider spending ceilings: input/output accounting, unknown consumption and
non-token provider charges retain their documented limits.

## Deferred direction and revisit conditions

DNS is explicitly deferred by the user. The current Kubernetes kube-dns exception
is unchanged, including its recursive external-name egress limitation. See
[DNS egress containment](../explanations/future-architecture/dns-egress.md).

Revisit AHP when a real external host/GUI client is in scope; require an actual
SDK client round trip, negotiated byte extensions, uncertain-delivery handling,
exclusive input, bounded replay, incarnation revalidation and detach/move/cancel
acceptance. Revisit ACP with a requested structured interaction and proof of
native UUID/checkpoint continuity for each pinned adapter. Revisit template and
gateway limits with a concrete supported workload and measured isolated tests,
without weakening unknown-state containment or physical fencing.

## Implementation and evidence reconciliation

The retained source reviews, security/gateway/CLI implementation reports, two
native design candidates and synthesis are under the local task evidence
`.engineering/tasks/review-fixes-2026-10-14/` and the preceding review task.
They are local working evidence rather than distributed operational instructions.
This decision covers their consequential ownership, protocol and identity choices;
ordinary parser fixes and regression tests do not introduce separate architecture.
The [current architecture](../explanations/system-architecture.md),
[security reference](../reference/security-hardening.md),
[native control reference](../reference/native-control.md) and
[session command reference](../reference/session-commands.md) describe the
implemented behavior and limits. Regression and integration results are recorded
in the task evidence; this ADR makes no new live provider, CNI, throughput or
full native failure-matrix claim.
