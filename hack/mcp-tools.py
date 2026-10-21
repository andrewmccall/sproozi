#!/usr/bin/env python3
"""List the tools a remote MCP server offers, to write an AgentPolicy tool scope.

    python3 hack/mcp-tools.py https://mcp.deepwiki.com/mcp
    python3 hack/mcp-tools.py https://mcp.example.com/mcp --bearer-token-file token --json

It speaks the MCP Streamable HTTP transport directly (initialize, then
tools/list, following pagination) with the Python standard library, and prints
each tool's name and argument names. --json prints the tools as the server sent
them. Run it from a machine that may reach the server; the agent never does.
The listing grants nothing: copy only the tools you approve into
AgentPolicy.spec.mcpServers.<name>.tools.
"""
import argparse
import json
from pathlib import Path
import sys
import urllib.request

PROTOCOL = "2025-03-26"


def post(url, token, session, message):
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
               "MCP-Protocol-Version": PROTOCOL}
    if token:
        headers["Authorization"] = "Bearer " + token
    if session:
        headers["Mcp-Session-Id"] = session
    request = urllib.request.Request(url, data=json.dumps(message).encode(), headers=headers, method="POST")
    with urllib.request.urlopen(request, timeout=30) as response:
        session = response.headers.get("Mcp-Session-Id") or session
        body = response.read(4 << 20).decode()
        kind = response.headers.get("Content-Type", "")
    if "id" not in message:
        return None, session
    if kind.startswith("text/event-stream"):
        # Each event's data lines carry one JSON-RPC message; take the reply.
        for event in body.split("\n\n"):
            data = "\n".join(line[5:].strip() for line in event.splitlines() if line.startswith("data:"))
            if data:
                reply = json.loads(data)
                if reply.get("id") == message["id"]:
                    return reply, session
        raise RuntimeError("the server sent no reply to " + message["method"])
    return json.loads(body), session


def result(reply, method):
    if "error" in reply:
        raise RuntimeError("%s failed: %s" % (method, reply["error"].get("message", reply["error"])))
    return reply["result"]


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("url", help="the server's MCP endpoint, as in servers.json")
    parser.add_argument("--bearer-token-file", type=Path, help="file holding the server's bearer token")
    parser.add_argument("--json", action="store_true", help="print the tools as the server sent them")
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
