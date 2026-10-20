# PRD: Managed Harness Configuration, Skills & Dynamic Trust

**Project:** Sproozi  
**Status:** In progress. M1 for managed Codex and Claude Code sessions and ordinary runs, and the `sproozi.diagnose` part of M3, are implemented; see [ADR 0025](../../adr/0025-managed-harness-integration-and-diagnose.md) and the [Sproozi integration reference](../../reference/sproozi-integration.md).  
**Priority:** Near-term foundation, followed by incremental trust capabilities  
**Target runtimes:** Docker and Kubernetes  
**Target harnesses:** Claude Code, Codex, OpenCode and Hermes

---

## 1. Executive Summary

Sproozi runs AI agents inside isolated execution environments, controlling their access to external services through a gateway and explicit capability policies.

This provides strong separation between an agent and the user's credentials, infrastructure and local environment. However, it introduces two usability problems:

1. Agents do not necessarily understand the Sproozi environment or how to recover from restricted external access.
2. Users cannot easily carry their preferred global skills, instructions and configuration into managed agent sessions.

Sproozi will introduce a **managed harness configuration system** that assembles a consistent, secure execution environment for every supported harness.

It will provide:

- Mandatory Sproozi system instructions and integration stubs.
- A built-in Sproozi access diagnostics skill.
- A protected Sproozi MCP control interface.
- User-global and project-specific skills and instructions.
- Harness-specific rendering of the assembled configuration.
- Reproducible configuration across local and Kubernetes execution.
- Configuration provenance and compatibility with portable sessions.

This provides the foundation for **dynamic capability negotiation**, allowing agents to identify access restrictions, request additional authority and recover from failures without leaving their native harness interface.

Sproozi remains responsible for enforcing policy. Harness instructions and skills improve the agent's ability to use the system but never establish authorization.

## 2. Problem Statement

### 2.1 Sandboxed agents encounter opaque failures

An agent running inside Sproozi may attempt to:

- Access an unfamiliar website.
- Use an MCP tool outside its approved scope.
- Read a Kubernetes resource beyond its permissions.
- Install a package from an unapproved registry.
- Connect to an internal service.

These operations may fail with HTTP errors, connection failures or application-level errors.

The agent cannot reliably distinguish policy restrictions from ordinary service failures.

This produces poor user experiences: agents may retry repeatedly, attempt inappropriate workarounds, abandon tasks or report misleading diagnoses.

### 2.2 Permission requests should not require a new user interface

Sproozi supports native harness experiences, including interactive terminal interfaces.

Replacing these with a custom terminal client solely to handle permissions would introduce significant complexity and reduce compatibility with native harness features.

Sproozi should preserve those interfaces and use established harness mechanisms, including MCP tools, skills, instruction files and MCP elicitation where supported.

### 2.3 Configuration is difficult to carry between environments

Users commonly maintain global instructions, agent skills and configuration for their preferred harnesses.

Sproozi deliberately isolates the execution environment from the user's personal home directory. This prevents accidental exposure of credentials, settings and host resources, but also means useful configuration is unavailable inside the sandbox.

Users need a safe way to import approved configuration and have it work consistently across local and remote executions.

### 2.4 Users cannot predict all required capabilities

A predefined capability policy works well when resource requirements are known.

For investigative or autonomous tasks, the agent may discover previously unknown requirements during execution.

Users need the ability to approve additional, narrowly scoped access without manually changing infrastructure configuration or restarting the entire workflow unnecessarily.

## 3. Goals

1. Every supported Sproozi-managed execution receives mandatory Sproozi instructions and integration configuration.
2. Users can add global and project-scoped instructions and skills without replacing mandatory Sproozi components.
3. Agents can diagnose external-access failures through a standard Sproozi MCP interface.
4. Gateway denials are discoverable and explainable using authenticated execution identity.
5. Configuration can be recreated consistently across supported execution environments.
6. Native harness interfaces remain usable without a Sproozi-specific TUI.
7. Future dynamic approvals reuse the diagnostic and MCP interfaces.
8. Explicit, independently enforced security boundaries remain authoritative.

## 4. Non-goals

- Building a new agent harness.
- Replacing Claude Code, Codex, OpenCode or Hermes terminal interfaces.
- Automatically granting access based solely on model requests.
- Blindly importing the user's complete home directory or harness configuration.
- Supporting arbitrary executable hooks in the initial release.
- Developing a generic messaging platform.
- Guaranteeing that all external network failures are observable by the gateway.
- Automatically installing arbitrary third-party MCP servers.
- Creating an AI-based trust or reputation scoring system.
- Guaranteeing behavioral compliance with instructions or skills.

## 5. Product Experience

### 5.1 First-time setup

A user installs Sproozi and configures an execution environment.

Sproozi automatically installs the appropriate managed instructions, diagnostics skill and control MCP connection.

No manual skill installation or instruction-file editing is required.

Existing launch workflows should continue to work, with new configuration assembled during environment preparation.

### 5.2 Adding global skills

Proposed commands:

```bash
sproozi config add-skill ~/skills/code-review
sproozi config add-skill ~/skills/home-ops
sproozi config list
```

Imported skills become available to supported harnesses according to their native skill-discovery conventions.

Sproozi checks for collisions with reserved components and applies the configured import policy.

Users should be able to inspect what will be available to a managed execution.

### 5.3 Project configuration

Projects may define additional instructions and skills.

For example:

```yaml
projects:
  home-ops:
    skills:
      - kubernetes-troubleshooting
      - home-network
    instructions:
      - ./instructions/home-ops.md
```

This is a proposed declarative representation, not an existing schema.

Project configuration extends the user-global configuration and is constrained by installation-level policy.

Selecting a project must not implicitly import arbitrary files or executable configuration from its working repository.

### 5.4 Access diagnosis

A user asks Claude Code to investigate an issue.

Claude attempts an external operation, but the operation fails.

Its managed instructions and Sproozi skill explain how to diagnose potential access restrictions.

Claude calls:

```text
sproozi.diagnose
```

Sproozi returns recent relevant authorization decisions for the authenticated execution.

For example:

```json
{
  "status": "denied",
  "source": "gateway",
  "capability": "network.egress",
  "destination": "docs.example.com:443",
  "reason": "destination_not_allowed",
  "requestable": true,
  "decision_id": "decision-123"
}
```

Claude can explain the restriction accurately and, once dynamic grants are implemented, request additional access.

### 5.5 Recovering from a denied operation

In the future approval workflow:

1. An agent encounters a failed external operation.
2. It invokes Sproozi diagnostics.
3. Sproozi identifies a matching, approvable denial.
4. The agent invokes the access-request MCP tool.
5. Sproozi checks delegated trust policy.
6. If necessary, Sproozi requests authorization from an appropriate user.
7. Sproozi issues a narrowly scoped grant.
8. The agent retries the operation when safe.

The user should not have to edit Kubernetes policies, restart Sproozi or switch to a second terminal just to authorize a routine request.

Approval presentation should use supported harness-native mechanisms wherever practical, with documented fallbacks.

## 6. Configuration Architecture

### 6.1 Configuration layers

Sproozi will assemble configuration from three layers.

| Layer | Ownership | Behaviour |
|---|---|---|
| Sproozi system | Sproozi installation | Mandatory, protected |
| User global | Authenticated installation principal | Extends approved settings |
| Project | Project configuration | Adds project-scoped settings |

Precedence is not a general unrestricted overwrite system.

For user-editable settings, project values may take precedence over user-global values where the schema explicitly permits it.

Sproozi system settings are non-overridable.

### 6.2 Mandatory system components

Every supported managed execution must receive:

- Sproozi environment instructions.
- Sproozi access-recovery skill.
- Sproozi MCP control connection.
- Execution-specific configuration metadata.
- Compatible gateway and credential-free connection settings.

These components are provisioned by the trusted environment preparation process.

The agent cannot alter its effective authorization by modifying generated configuration.

Reserved names and paths must not be replaced by imported user or project configuration.

### 6.3 Configuration assembly

Introduce a configuration assembly abstraction:

```text
Installation configuration
           +
User-global configuration
           +
Project configuration
           +
Sproozi-managed components
           |
           v
     Validation
           |
           v
 Immutable execution config
           |
           v
 Harness-specific renderer
           |
           v
    Managed agent runtime
```

Validation occurs before the harness is launched.

The assembled configuration must be reproducible from its inputs and recorded metadata.

Configuration assembly must not expose sensitive installation credentials to the sandbox.

### 6.4 Harness adapters

Each harness adapter is responsible for:

- Rendering supported instruction files.
- Installing approved skill definitions in native locations.
- Registering the protected Sproozi MCP connection.
- Translating approved user configuration into native formats.
- Disabling unsupported or unsafe options.
- Reporting unsupported configuration rather than silently ignoring important settings.

Adapters should reuse existing harness preparation logic rather than introduce parallel configuration pipelines.

The supported configuration subset must be documented and tested per harness.

### 6.5 Global configuration

Global configuration should be scoped to an authenticated user or installation principal, rather than automatically shared among all users of a Sproozi deployment.

The first implementation may use a locally managed configuration store.

Future installations may support centralized synchronization.

Imported skills should have explicit provenance, including source identity, content digest and version.

Sproozi must not treat the location of a skill as evidence of trustworthiness.

### 6.6 Configuration integrity

Sproozi should record an assembled configuration digest alongside each execution.

The digest should cover security-relevant, identity-defining and reproducibility-relevant configuration, with credentials excluded from stored artifacts.

Protected component changes require compatibility checks.

A changed system skill or instruction must not silently reinterpret a retained execution's approved authority.

## 7. Sproozi Access Diagnostics

### 7.1 Decision journal

Introduce a bounded, execution-scoped decision journal.

The journal records relevant authorization outcomes from the existing gateway:

- Execution identity.
- Timestamp.
- Capability.
- Enforcement tier.
- Requested destination or resource, appropriately redacted.
- Decision outcome.
- Denial reason.
- Correlation identifier.
- Policy version or identity where relevant.

Initially, the journal should focus on authorization denials. Full traffic logging is not required.

The system should avoid retaining sensitive request bodies, authorization headers or URL query parameters.

Storage must be bounded by retention, size and per-execution limits.

Kubernetes deployments should support a durable shared journal suitable for multiple gateway replicas. Local execution may use an appropriate local backend.

### 7.2 Diagnostic MCP tool

Expose:

```text
sproozi.diagnose
```

Optional arguments:

```json
{
  "destination": "docs.example.com",
  "lookback_seconds": 60,
  "decision_id": "decision-123"
}
```

The tool authenticates through Sproozi's trusted execution identity.

An agent cannot select another execution's journal by supplying an arbitrary run identifier.

Responses should distinguish:

- `denied`: a matching Sproozi authorization denial was observed.
- `allowed`: a relevant operation was permitted, where sufficient evidence exists.
- `not_observed`: no matching gateway decision was found.
- `unknown`: available evidence cannot establish the cause.

A gateway decision must not be represented as the cause of a failure unless the evidence supports that conclusion.

### 7.3 Network-level failures

Some network requests are blocked before reaching the gateway.

Sproozi's Kubernetes network policies and local network restrictions may prevent traffic without creating a gateway decision.

The initial diagnostics implementation must acknowledge this limitation.

Future extensions may use trusted CNI flow telemetry, eBPF-based observations or runtime network diagnostics.

Such integrations are optional and must not weaken isolation.

A network timeout or missing journal event is not automatically an approvable permission denial.

### 7.4 Structured gateway errors

Where possible, gateway denials should return machine-readable information including:

- Stable denial code.
- Correlation identifier.
- Missing capability or denied scope.
- Whether further access may be requested.
- An actionable Sproozi diagnostic reference.

These errors must preserve appropriate semantics for the original client and protocol.

Structured responses improve recovery but are not required for the diagnostic tool to function.

## 8. Built-in Sproozi Skill

### 8.1 Purpose

The skill teaches agents how to operate effectively within Sproozi's security boundary.

It should be concise, harness-neutral and maintained with the Sproozi distribution.

### 8.2 Required guidance

The skill explains:

- External capabilities are explicitly controlled.
- Sproozi may reject access that works on an unrestricted host.
- Unexpected external-operation failures can be investigated with `sproozi.diagnose`.
- Confirmed denials may be requestable when supported.
- Denied access must not be bypassed through alternative tools, transports or credentials.
- Access requests should be specific and justified.
- Failed operations should only be retried when retrying is safe.
- If access remains unavailable, the agent should report the limitation clearly.

### 8.3 Installation contract

The skill must be present in each supported execution.

Harness-specific loaders must be tested to ensure it is discoverable under the generated configuration.

Always-loaded Sproozi instructions should reference the skill and relevant MCP tools.

The system must not depend on the model consistently following these instructions for security enforcement.

### 8.4 User customization

Users may add additional skills but cannot replace or shadow the protected Sproozi skill.

Conflicting names or reserved namespace usage should fail configuration validation with an actionable error.

## 9. Sproozi Control MCP

### 9.1 Initial tool

The first milestone introduces:

```text
sproozi.diagnose
```

This is read-only and available through the protected Sproozi control connection.

It should not require users to configure a third-party MCP provider.

### 9.2 Future tools

```text
sproozi.request_access
sproozi.access_status
sproozi.list_capabilities
```

`request_access` submits a request; it does not itself grant authority.

`access_status` returns the status of a request visible to the current execution.

`list_capabilities` describes available capabilities and relevant restrictions without leaking confidential policy details belonging to other users or projects.

### 9.3 MCP compatibility

The MCP adapter should use standard protocol operations where possible.

Future interactive approvals may use MCP elicitation when supported by the harness and MCP client.

Support must be verified against specific harness versions.

The system must provide a fallback when elicitation is unavailable.

MCP compatibility must not make the client, model or agent the authority for granting access.

## 10. Future: Dynamic Capability Negotiation

### 10.1 Capability requests

Agents may request additional authority for a specific operation or resource.

A request should include:

- Authenticated execution identity.
- Capability and requested scope.
- Resource or destination.
- Reason for access.
- Requested duration.
- Correlation with an observed denial, where applicable.

Requests may also be initiated proactively when the agent recognizes that additional capability is required.

### 10.2 Policy outcomes

A trust broker evaluates the request and returns:

- **Allow:** Already within delegated authority.
- **Ask:** Requires an authenticated human approval.
- **Deny:** Outside permissible security boundaries.

Automatic approval must be governed by deterministic, administrator-controlled policy.

An agent cannot authorize its own request.

### 10.3 Temporary capability leases

Approved additional authority is represented by a temporary capability lease.

Leases should be:

- Bound to an execution and authenticated principal.
- Restricted to a specified resource and operation scope.
- Time-limited.
- Independently revocable.
- Auditable.
- Subject to installation-level hard limits.
- Revalidated before enforcement.

Leases must never silently replace or widen immutable execution grants without an explicit authorization mechanism.

The design must define whether a new execution is required when a lease cannot be applied safely to the existing one.

### 10.4 Approval delivery

Sproozi owns the approval lifecycle.

Approval presentation is extensible through delivery adapters.

Candidate surfaces include:

- MCP elicitation inside supported native harness interfaces.
- ACP interactions where Sproozi operates an ACP client.
- CLI approval commands.
- Webhooks and external messaging integrations.
- Authenticated browser-based approval pages.

No single messaging provider or harness is required for the core approval model.

Approval identity must be independently verified.

An approval message, tool result or agent-generated text must not be treated as authorization.

### 10.5 Attended execution

For attended sessions, the preferred experience is approval within the existing harness interface.

The agent may use its Sproozi skill and MCP connection to initiate approval.

Where the protocol supports it, the client presents an interactive approval request.

Where no native interaction is available, the request may become pending and be delivered through another configured surface.

The design must not assume that gateway denials automatically become native permission dialogs.

### 10.6 Unattended execution

For unattended executions, approval requests may remain pending while Sproozi notifies an authorized user.

Pending requests should survive temporary disconnection and process interruption, subject to expiration.

Agents should be able to check status or resume work after authorization.

Long waits should not require an unlimited active gateway connection or an indefinitely running agent.

### 10.7 Dynamic provider registration

Introducing a completely new MCP server or external provider is distinct from widening access to an already configured service.

Provider registration may require:

- Trusted endpoint validation.
- Credential provisioning.
- Tool-schema inspection.
- Provider identity and transport verification.
- Administrator approval.
- Explicit capability registration.

This belongs in a later milestone.

## 11. Session Mobility and Configuration

Sproozi supports retained conversations and workspace transfer between execution environments.

Managed configuration must work with this model.

### 11.1 Configuration identity

Each execution should record:

- Harness type.
- Relevant harness version or compatibility metadata.
- Assembled configuration digest.
- Sproozi integration version.
- Approved skill identities and digests.
- Project association.
- Relevant configuration provenance.

### 11.2 Resume behaviour

Resuming a session reconstructs the approved execution environment independently of the source runtime.

The destination validates current project approval and capability policy.

The latest trusted Sproozi security integration may be installed only when compatible with the retained session contract.

Changes to user skills or instructions require an explicit compatibility policy.

User-global configuration must not silently expand a resumed session's approved external authority.

### 11.3 Cross-harness portability

This feature does not itself provide cross-harness conversation compatibility.

Harness-native session formats and existing mobility limitations remain separate concerns.

Configuration portability should not be presented as conversation portability.

## 12. Security Requirements

The following are mandatory:

1. **Fail closed:** Missing or invalid protected configuration must not result in unrestricted execution.
2. **Immutable authority:** User files, skills and instructions cannot expand external permissions.
3. **Reserved integration:** The protected Sproozi namespace and MCP control interface cannot be shadowed by user configuration.
4. **No credential imports:** Personal token stores, SSH keys, cloud credentials and unreviewed environment variables must not be copied automatically.
5. **No arbitrary hooks:** Executable hooks and plugins require a separate, explicit trust policy.
6. **Scoped diagnostics:** Agents may inspect only their own permitted execution context.
7. **Bounded telemetry:** Journals have explicit retention, capacity and redaction.
8. **Authenticated approvals:** Future grants require trusted authorization outside the agent.
9. **Revocation:** Access changes must be enforced at the actual service/network boundary, with documented timing guarantees.
10. **Config provenance:** Configuration sources and integrity digests must be recorded.
11. **Untrusted project content:** Repository files cannot overwrite trusted system configuration simply by containing harness-recognized filenames.
12. **No bypass via alternative transport:** Generic destination access cannot circumvent semantic or protocol-specific restrictions.

Protected stubs should be delivered through trusted preparation and filesystem controls. Their presence alone does not guarantee that an agent will follow them.

## 13. Proposed Milestones

### M1 — Managed Harness Bootstrap

**Goal:** Every execution understands that it is managed by Sproozi.

Deliverables:

- Configuration assembly abstraction.
- Mandatory system instructions.
- Protected Sproozi access-recovery skill.
- Protected Sproozi MCP configuration.
- Harness-specific renderers.
- Configuration validation.
- Integration tests for supported harnesses.

**Exit criteria:** Claude Code and Codex can launch using their native interfaces, discover Sproozi's integration and maintain existing sandbox restrictions. OpenCode and Hermes receive equivalent configuration in their supported execution modes.

### M2 — Global and Project Configuration

**Goal:** Users can bring approved skills and instructions into managed sessions.

Deliverables:

- User-global configuration source.
- Project-scoped configuration.
- Skill import and listing.
- Protected namespace collision detection.
- Explicit import allowlists and validation.
- Configuration provenance and digests.
- Documentation and CLI integration.

**Exit criteria:** A globally configured skill appears in applicable local and remote sessions. Project configuration is scoped correctly. Reserved Sproozi components cannot be replaced.

### M3 — Gateway Diagnostics

**Goal:** Agents can understand why external access failed.

Deliverables:

- Execution-scoped authorization journal.
- Standardized diagnostic reason codes.
- `sproozi.diagnose` MCP tool.
- Diagnostic skill instructions.
- Exact and time-window-based correlation.
- Journal retention and redaction policies.
- Tests for denied, unobserved and ambiguous requests.

**Exit criteria:** An agent can identify a gateway-blocked request through MCP without direct access to gateway logs or administrative credentials.

### M4 — Approval Requests

**Goal:** Agents can request access beyond predefined capabilities.

Deliverables:

- Capability-request data model.
- Approval state machine.
- Authenticated decision API.
- Policy-based request eligibility.
- Approval event interface.
- MCP request and status tools.
- Initial attended approval experience.
- Explicit denial and expiration behaviour.

**Exit criteria:** A supported agent can diagnose a denied operation, request approval and receive a clear outcome, without changing the underlying policy automatically.

### M5 — Temporary Grants and Recovery

**Goal:** Approved access can be used safely.

Deliverables:

- Scoped capability leases.
- Gateway lease enforcement.
- Expiry and revocation.
- Revalidation against hard policy limits.
- Safe retry guidance.
- Audit trail.
- Local and Kubernetes integration tests.

**Exit criteria:** An approved destination or operation becomes temporarily accessible to the authorized execution, expires correctly and remains inaccessible to other executions.

### M6 — Delegated Trust and Provider Expansion

**Goal:** Reduce approval fatigue and support agents discovering new capabilities.

Deliverables:

- Delegated automatic-approval policies.
- Risk and scope-based escalation rules.
- Notification adapters for unattended agents.
- Capability discovery.
- Managed MCP provider registration workflow.
- Approval history and reusable policy recommendations.

**Exit criteria:** Routine requests within delegated authority succeed without manual intervention, while disallowed or high-risk requests remain subject to strict authorization.

## 14. Acceptance Tests

The implementation should include these end-to-end scenarios.

| Scenario | Expected result |
|---|---|
| Fresh managed Claude session | Sproozi integration and skill available |
| Fresh managed Codex session | Equivalent Sproozi integration available |
| User adds global skill | Skill becomes discoverable in supported executions |
| User adds skill named `sproozi-access` | Reserved-name validation rejects collision |
| Project contains hostile `CLAUDE.md` or `AGENTS.md` | Trusted system integration remains intact |
| Unauthorized website access | Gateway denies and records a diagnostic decision |
| Agent queries recent denials | Receives only its own relevant decisions |
| Direct connection blocked before gateway | Diagnostic tool does not invent a gateway denial |
| Gateway is unavailable | Diagnostic request fails safely |
| Session moves Docker → Kubernetes | Configuration is reconstructed and verified |
| Project permission narrows before resume | Resume cannot silently retain excessive authority |
| Approved temporary access | Only the approved execution receives the scoped lease |
| Lease expires | Further operations are denied |
| User denies approval | Agent cannot acquire the requested authority |
| Unsupported elicitation | Request remains accessible through documented fallback |
| Untrusted skill requests unrestricted access | Gateway continues enforcing policy |

## 15. Existing Architecture Alignment

The implementation should build on existing Sproozi components.

Relevant current concepts include:

- `AgentRun` and immutable execution capabilities.
- `AgentPolicy` and live policy checks.
- `AgentTemplate` and runtime configuration.
- Semantic, protocol and destination enforcement tiers.
- Shared capability gateway.
- Configured MCP connections.
- Native managed sessions.
- Docker and Kubernetes execution.
- Projects and retained session mobility.
- Gateway audit events and route enforcement.

The configuration system should extend the current trusted harness preparation mechanism.

The diagnostic journal should reuse gateway authorization decisions rather than introduce a second policy engine.

Existing portability and session-approval safeguards must remain authoritative.

Current harness support is not uniform: ordinary runs, managed interactive sessions and remote task workflows have different supported feature sets. Implementation and documentation must distinguish these rather than claim identical behaviour.

## 16. Success Criteria

The product succeeds when:

- Users can run their preferred supported harness without manually preparing Sproozi-specific instructions.
- Existing native terminal experiences remain intact.
- Approved global skills are consistently available across executions.
- Agents can correctly identify known gateway restrictions.
- Agents stop treating every restricted operation as an arbitrary network failure.
- Diagnostic queries do not expose other users' execution activity.
- Adding skills does not weaken existing sandbox isolation.
- Configuration can be reconstructed and inspected after session movement.
- Future approvals can be added without redesigning the harness configuration or gateway authorization model.

Future measures may include successful recovery from policy denials, approval turnaround time, repeated-denial frequency and the proportion of routine capability requests handled under delegated authority.

## 17. Open Design Questions

### Configuration ownership

Should global configuration be managed by each Sproozi installation initially, with synchronization introduced later?

**Initial recommendation:** Yes. Avoid a centralized configuration service for the first implementation.

### Configuration snapshots

Should every imported skill be snapshotted into immutable execution configuration?

**Initial recommendation:** Yes, or pinned to a content-addressed artifact. Do not rely on mutable source paths during remote execution.

### Configuration updates during resume

Should a session always use its original skills, or should newer versions be available?

**Initial recommendation:** Retain an immutable configuration identity, enforce new security policy independently and introduce explicit compatibility rules for upgrades.

### Diagnostic storage

Should the gateway journal share existing audit infrastructure or use its own persistence abstraction?

**Initial recommendation:** Reuse existing audit records where suitable, with a bounded execution-oriented query index rather than a duplicate event model.

### MCP elicitation

How consistently can the supported harnesses display native MCP elicitation, especially in unattended or permission-bypass modes?

**Initial recommendation:** Test this experimentally before making it the default attended approval transport.

### Automatic network diagnosis

Should agents ask for diagnostics only when they see failures, or should Sproozi proactively expose recent denials?

**Initial recommendation:** Provide agent-initiated diagnosis first. Event-driven suggestions may follow.

### Initial trust boundary

Should dynamic grants be applicable to active immutable executions?

**Initial recommendation:** Preserve existing immutable grants until an explicit, independently authorized lease mechanism is designed and verified.

## 18. Recommended Implementation Direction

Proceed with **M1–M3 as a cohesive near-term capability**.

These milestones deliver real value without requiring a complete dynamic-trust system:

- Automatic Sproozi integration in managed harnesses.
- User-global and project-scoped configuration.
- Safer configuration portability.
- Accessible gateway diagnostics.
- Better agent recovery from external-access failures.

Treat M4–M6 as the next stage of the same architecture, rather than introducing a separate approval platform.

The core design principles are:

**Sproozi owns the execution boundary.**

**The user owns approved configuration and delegated authority.**

**The harness retains its native interaction model.**

**The agent can diagnose restrictions and request more access, but cannot grant it to itself.**
