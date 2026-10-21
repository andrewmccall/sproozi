#!/usr/bin/env python3
"""List the tools a remote MCP server offers, to write an AgentPolicy tool scope.

    python3 hack/mcp-tools.py https://mcp.deepwiki.com/mcp
    python3 hack/mcp-tools.py https://mcp.example.com/mcp --bearer-token-file token --json

It speaks the MCP Streamable HTTP transport directly (initialize, then
tools/list, following pagination) with the Python standard library, and prints
each tool's name and argument names. --json prints the tools as the server sent
them. A URL that serves only the legacy HTTP+SSE transport (often ending in
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
