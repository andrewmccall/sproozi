#!/usr/bin/env python3
"""List the tools a remote MCP server offers, to write an AgentPolicy tool scope.

    python3 hack/mcp-tools.py https://mcp.deepwiki.com/mcp
    python3 hack/mcp-tools.py https://mcp.example.com/mcp --bearer-token-file token --json
    python3 hack/mcp-tools.py https://mcp.deepwiki.com/mcp --policy examples/mcp/agentpolicy.yaml

It speaks the MCP Streamable HTTP transport directly (initialize, then
tools/list, following pagination) with the Python standard library, and prints
each tool's name and argument names. --json prints the tools as the server sent
them. --policy checks an AgentPolicy's tool scope for this server against the
live tools: unknown tools, unknown arguments and required arguments the policy
forbids are reported, and the command exits 1. A URL that serves only the legacy HTTP+SSE transport (often ending in
/sse) is reported as such; the gateway needs the Streamable HTTP endpoint. Run
it from a machine that may reach the server; the agent never does.
The listing grants nothing: copy only the tools you approve into
AgentPolicy.spec.mcpServers.<name>.tools.
"""
import argparse
import json
from pathlib import Path
import sys
import urllib.error
import urllib.request

PROTOCOL = "2025-03-26"


def post(url, token, session, message):
    # Some hosted servers sit behind bot filters that refuse Python's default
    # User-Agent with 403, so name the tool instead.
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
               "MCP-Protocol-Version": PROTOCOL, "User-Agent": "sproozi-mcp-tools/1"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if session:
        headers["Mcp-Session-Id"] = session
    request = urllib.request.Request(url, data=json.dumps(message).encode(), headers=headers, method="POST")
    try:
        response = urllib.request.urlopen(request, timeout=30)
    except urllib.error.HTTPError as error:
        if message.get("method") == "initialize" and error.code in (400, 404, 405) and legacy_sse(url, headers):
            raise RuntimeError(LEGACY) from None
        raise
    with response:
        session = response.headers.get("Mcp-Session-Id") or session
        if "id" not in message:
            return None, session
        if not response.headers.get("Content-Type", "").startswith("text/event-stream"):
            return json.loads(response.read(4 << 20).decode()), session
        # Read events as they arrive: a Streamable HTTP reply ends the
        # stream, but a legacy SSE endpoint keeps it open after "endpoint".
        event, data = "", []
        for raw in response:
            line = raw.decode().rstrip("\r\n")
            if line.startswith("event:"):
                event = line[6:].strip()
            elif line.startswith("data:"):
                data.append(line[5:].strip())
            elif not line:
                if event == "endpoint":
                    raise RuntimeError(LEGACY)
                if data:
                    reply = json.loads("\n".join(data))
                    if reply.get("id") == message["id"]:
                        return reply, session
                event, data = "", []
        raise RuntimeError("the server sent no reply to " + message["method"])


LEGACY = ("this URL serves only the legacy HTTP+SSE transport; Sproozi needs the server's Streamable HTTP "
          "endpoint, which often ends in /mcp instead of /sse")


def legacy_sse(url, headers):
    """Report whether a GET opens a legacy SSE stream that announces an endpoint."""
    request = urllib.request.Request(url, headers={"Accept": "text/event-stream", "User-Agent": headers["User-Agent"]})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            if not response.headers.get("Content-Type", "").startswith("text/event-stream"):
                return False
            for _, raw in zip(range(20), response):
                if raw.decode().strip() == "event: endpoint":
                    return True
    except OSError:
        return False
    return False


def result(reply, method):
    if "error" in reply:
        raise RuntimeError("%s failed: %s" % (method, reply["error"].get("message", reply["error"])))
    return reply["result"]


def load_policy(path):
    """Read an AgentPolicy from YAML (with PyYAML) or JSON."""
    text = path.read_text()
    try:
        return json.loads(text)
    except ValueError:
        pass
    try:
        import yaml
    except ImportError:
        raise RuntimeError("--policy needs PyYAML for YAML files: python3 -m pip install pyyaml") from None
    return yaml.safe_load(text)


def policy_scope(policy, server):
    servers = (policy.get("spec") or {}).get("mcpServers") or {}
    if server is None:
        if len(servers) != 1:
            raise RuntimeError("the policy scopes %d servers (%s); name one with --server" %
                               (len(servers), ", ".join(sorted(servers)) or "none"))
        server = next(iter(servers))
    if server not in servers:
        raise RuntimeError("the policy has no mcpServers.%s" % server)
    return server, servers[server].get("tools") or {}


def check_policy(tools, scope):
    """Compare a policy's tool scope with the server's tools; return problems."""
    offered = {tool["name"]: tool.get("inputSchema") or {} for tool in tools}
    problems = []
    for name in sorted(scope):
        if name not in offered:
            problems.append("tool %r is not offered; the server offers %s" % (name, ", ".join(sorted(offered)) or "none"))
            continue
        arguments = (scope[name] or {}).get("arguments") or {}
        known = set((offered[name].get("properties") or {}))
        named = set(arguments.get("properties") or {}) | set(arguments.get("required") or [])
        for argument in sorted(named - known):
            problems.append("tool %r has no argument %r; its arguments are %s" %
                            (name, argument, ", ".join(sorted(known)) or "none"))
        if arguments.get("additionalProperties") is False:
            allowed = set(arguments.get("properties") or {})
            for argument in sorted(set(offered[name].get("required") or []) - allowed):
                problems.append("tool %r requires argument %r, which the policy's additionalProperties: false "
                                "forbids, so every call would fail" % (name, argument))
    return problems


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("url", help="the server's MCP endpoint, as in servers.json")
    parser.add_argument("--bearer-token-file", type=Path, help="file holding the server's bearer token")
    parser.add_argument("--json", action="store_true", help="print the tools as the server sent them")
    parser.add_argument("--policy", type=Path, help="AgentPolicy YAML or JSON whose tool scope to check")
    parser.add_argument("--server", help="the policy's mcpServers entry to check, when it has several")
    args = parser.parse_args()
    token = args.bearer_token_file.read_text().strip() if args.bearer_token_file else ""
    reply, session = post(args.url, token, "", {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
        "protocolVersion": PROTOCOL, "capabilities": {}, "clientInfo": {"name": "sproozi-mcp-tools", "version": "1"}}})
    server = result(reply, "initialize").get("serverInfo", {})
    post(args.url, token, session, {"jsonrpc": "2.0", "method": "notifications/initialized"})
    tools, cursor, number = [], None, 2
    for _ in range(16):
        params = {"cursor": cursor} if cursor else {}
        reply, session = post(args.url, token, session, {"jsonrpc": "2.0", "id": number, "method": "tools/list", "params": params})
        page = result(reply, "tools/list")
        tools += page.get("tools", [])
        cursor, number = page.get("nextCursor"), number + 1
        if not cursor:
            break
    if args.policy:
        name, scope = policy_scope(load_policy(args.policy), args.server)
        problems = check_policy(tools, scope)
        for problem in problems:
            print("mcp-tools: %s: %s" % (name, problem), file=sys.stderr)
        if problems:
            sys.exit(1)
        print("The policy's %d %s tools match the server's tools and arguments." % (len(scope), name))
        return
    if args.json:
        json.dump(tools, sys.stdout, indent=2)
        print()
        return
    print("%s %s offers %d tools:" % (server.get("name", "The server"), server.get("version", ""), len(tools)))
    for tool in tools:
        schema = tool.get("inputSchema") or {}
        required = set(schema.get("required", []))
        arguments = ", ".join(name + ("" if name in required else "?") for name in schema.get("properties", {}))
        print("  %s(%s)" % (tool["name"], arguments))


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError, KeyError) as error:
        print("mcp-tools: " + str(error), file=sys.stderr)
        sys.exit(1)
