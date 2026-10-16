# Verification reports

These reports describe specific runs and the limits of their evidence. Some used
real model accounts; others used deterministic model responses while running the
stock clients, gateways and execution environments. A deterministic provider
tests the integration and access controls, not model reasoning or real billing.

| Report | What was exercised | Model and scope |
| --- | --- | --- |
| [Infrastructure investigation](verified-sre-demo.md) | Codex investigated a failing workload and proposed a GitHub PR. | Real model and GitHub in isolated Kind; no merge or deployment. |
| [Kubernetes and configured MCP](verified-mcp-demo.md) | Native Kubernetes reads, two HTTPS MCP fixture providers, scope and budget denials, cancellation and cleanup. | Real model in isolated Kind; the configured providers were fixtures. |
| [Interactive session movement](verified-session-mobility.md) | Codex 0.161.0 and Claude Code 2.1.27 moved between Docker and Kubernetes with the same conversation and unfinished Git work. | Deterministic providers; the report distinguishes each client's failure-path coverage. |
| [Retained remote work](verified-portable-workflows.md) | Remote Codex work, stock Hermes dispatch, pull and fresh local continuation, Project filtering and exact Git revision replay. | Deterministic models, actual package checks and isolated Pod reads/Secrets denials; personal infrastructure was not accessed. |

The [Project and revision evidence](execution-roadmap-evidence.json) records the
native development and isolated home-ops scenarios described in the retained-work
report. The [session mobility evidence](session-mobility-evidence.json) and
[stock Hermes evidence](retained-work-hermes-evidence.json) retain selected
identities and outcomes without raw transcripts or credentials.

Read each report's scope and source or image provenance before interpreting its
results. These historical observations do not establish that
every current configuration works, that a release is reproducible, or that
Sproozi is production ready. They also do not establish correctness of agent
answers or hard provider-spending limits. Read the
[security limitations](../reference/security-hardening.md) before interpreting
the results as deployment guarantees.

See the [documentation index](../README.md) for setup guides and references.
