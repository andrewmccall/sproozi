# 0039: One harness registry, and every harness needs an MCP client

Status: accepted
Date: 2026-10-20

## Problem and constraints

Five contributors tried to add the `pi` agent as a harness from the docs. Each
built only part of it. Harness names were string literals in the CRD enum,
the MCP renderer, the contract renderer, the managed-session code, tests,
verifiers and four or five pages. Adding `pi` to the MCP renderer's allow-list
alone made it render `claude-mcp.json` with no servers, dropping the requested
servers and the `sproozi` control connection, and every test still passed.
Adding it to the CRD enum alone also passed every test, although the renderer
then refused it.

Stock `pi` has no MCP client. Every harness receives the `sproozi` control
connection and its `diagnose` tool over MCP
([ADR 0025](0025-managed-harness-integration-and-diagnose.md)), and nothing said
what a harness without MCP meant.

## Decision

- **One registry.** `internal/harness/registry.go` lists every harness with
  the facts Go code needs: its selector, its MCP configuration format, its
  contract launch files, whether its launch reads the request files, and for
  managed harnesses its native kind, native directory and model protocol. The
  MCP renderer, the AgentRun contract renderer, native-kind validation,
  provider selection, inspection hosts and the profile's harness files read
  it. An unregistered selector is an error everywhere.
- **Two tiers.** Every registered harness is an AgentRun harness; one with a
  native kind is also a managed harness. The glossary and the harness
  reference name both.
- **Guards instead of prose.** Tests compare the registry with the CRD enum
  marker and the generated CRD, render every harness's configuration and
  require the `sproozi` connection, and check that each managed kind is
  accepted by `nativeconfig` and has a supervisor adapter and a checkpoint
  format. Each failure names the next file to change.
- **An MCP client is required.** A harness is registered only with an MCP
  format that renders every connection. A program without an MCP client
  can't be added until it gains one, upstream or through an MCP bridge
  shipped as a launch file.

## Alternatives

- **Allow a harness without MCP, marked "no diagnose".** It would add a
  capability flag, a second contract shape, and a supported path with no
  `diagnose` and no configured MCP servers, which the acceptance rules treat
  as part of every run. Requiring a bridge keeps one contract.
- **Generate the CRD enum from the registry.** Kubebuilder markers are static,
  and `api` cannot import `internal/harness`, which depends on it. A guard
  test gives the same assurance with less machinery.
- **One table for every per-harness behaviour.** Checkpoint formats,
  completion observers and supervisor adapters are code, not data; the
  registry records facts and the guards check the code exists.

## Consequences and limits

- Verifiers, client lockfiles, image builds, examples and docs still list
  harnesses; the contributor checklist names them.
- `nativeconfig` keeps its own switch because `api` imports it; a guard keeps
  it equal to the managed tier.

Revisit if a harness needs a contract that is not MCP configuration, or if
Kubebuilder can take enum values from Go code.

Implementation: [registry](../../internal/harness/registry.go),
[guards](../../internal/harness/registry_test.go) and the
[contributor checklist](../contributing/add-a-harness.md).
