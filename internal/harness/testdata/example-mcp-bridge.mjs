// Example MCP bridge for a harness whose agent has no MCP client of its own.
// A real bridge is a launch file the harness loads (for example as a plugin or
// extension); this one shows the parts Sproozi relies on and supports the
// `--sproozi-check CONFIG` mode the registry test hook runs.
//
// - Read the rendered MCP file from the run contract.
// - Refuse to start without the `sproozi` control connection.
// - Open one MCP session per call over Streamable HTTP, through the proxy and
//   trust bundle the sandbox environment already provides (Node: set
//   NODE_USE_ENV_PROXY=1; NODE_EXTRA_CA_CERTS is injected).
// - Name each tool `<connection>__<tool>`.
import { readFileSync } from "node:fs";

const PROTOCOL = "2025-06-18";

function connections(path) {
  const servers = JSON.parse(readFileSync(path, "utf8")).mcpServers ?? {};
  if (!servers.sproozi?.url) {
    throw new Error("refusing to start: the MCP configuration has no sproozi control connection");
  }
  return servers;
}

async function rpc(url, session, id, method, params) {
  const headers = { "content-type": "application/json", accept: "application/json, text/event-stream",
    "mcp-protocol-version": PROTOCOL };
  if (session) headers["mcp-session-id"] = session;
  const body = { jsonrpc: "2.0", method, params };
  if (id !== undefined) body.id = id;
  const response = await fetch(url, { method: "POST", headers, body: JSON.stringify(body) });
  if (!response.ok) throw new Error(`${method}: HTTP ${response.status}`);
  if (id === undefined) return { session };
  const text = await response.text();
  const json = response.headers.get("content-type")?.includes("text/event-stream")
    ? JSON.parse(text.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5)).join(""))
    : JSON.parse(text);
  if (json.error) throw new Error(`${method}: ${json.error.message}`);
  return { session: session ?? response.headers.get("mcp-session-id"), result: json.result };
}

// One MCP session per call: initialize, then the request.
async function call(url, method, params) {
  const init = await rpc(url, undefined, 1, "initialize",
    { protocolVersion: PROTOCOL, capabilities: {}, clientInfo: { name: "example-mcp-bridge", version: "1" } });
  await rpc(url, init.session, undefined, "notifications/initialized", {});
  return (await rpc(url, init.session, 2, method, params)).result;
}

export async function tools(path) {
  const names = [];
  for (const [connection, server] of Object.entries(connections(path))) {
    const listed = await call(server.url, "tools/list", {});
    for (const tool of listed.tools ?? []) names.push(`${connection}__${tool.name}`);
  }
  return names.sort();
}

const check = process.argv.indexOf("--sproozi-check");
if (check >= 0) {
  try {
    console.log(JSON.stringify({ tools: await tools(process.argv[check + 1]) }));
  } catch (error) {
    console.error(error.message);
    process.exit(2);
  }
}
