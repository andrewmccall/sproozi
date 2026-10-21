# Contributing

Start with `AGENTS.md`: it maps the code and lists the checks each kind of
change needs. You need Go 1.26 or later, Make and Python 3; some checks also
need Docker. Lint runs with Go 1.26.6 whatever Go you have installed
(`LINT_TOOLCHAIN` in the Makefile); the go command downloads it once.

Run the narrowest check that covers your change before opening it. The
[testing requirements](test/acceptance/README.md) describe the behaviour tests
must keep, and [test commands](test/acceptance/running.md) say what each
command proves and what it needs.

Regenerate CRDs, RBAC and deepcopy files with the commands in `AGENTS.md`;
never edit generated output by hand. A security-sensitive change needs both an
allow and a denial test, including the expected reason and proof that nothing
reached the upstream service.

## Common changes

- **Add an MCP server.** Put `servers.json`, `agentpolicy.yaml`,
  `agenttemplate.yaml`, `agentrun.yaml` and `agentruntime.yaml` in a directory
  under `examples/`, list the server's real tools with
  `python3 hack/mcp-tools.py URL`, and run `make verify-mcp`.
  [Configured MCP](docs/reference/configured-mcp.md) explains each file.
- **Add a harness.** Follow [Add a harness](docs/contributing/add-a-harness.md):
  one registry entry, guard tests that name each missing piece, and the MCP
  requirement.

Don't commit credentials, provider responses, patches or local agent
configuration.

## Maintainer skill

`.agents/skills/verify-sproozi` drives the
[investigation-to-PR demo on Kind](docs/guides/pr-demo.md) through
`hack/verify/quickstart.py` and keeps local proof. You don't need it for code
changes.
