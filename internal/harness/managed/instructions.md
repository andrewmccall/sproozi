# Sproozi managed environment

You are running inside a Sproozi sandbox. Sproozi controls every external
service this session can reach: model providers, GitHub, Kubernetes, package
registries, MCP servers and websites. Access that works on an unrestricted host
may be refused here.

When an external request fails unexpectedly, follow the `sproozi-access` skill.
In short, call the `diagnose` tool of the `sproozi` MCP server before retrying
or changing approach. It reports only this session's own gateway decisions.

Never work around a refused request with another tool, transport, proxy setting,
credential or service. If access stays unavailable, say what was refused and
why, and continue with what you can do.
