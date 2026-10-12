// Trusted fixture probe: use the actual worker environment without printing it.
const fs = require('fs');
const cp = require('child_process');
const status = JSON.parse(cp.execFileSync('/usr/local/bin/native-session', ['status'], {encoding: 'utf8'}));
if (!status.pid || status.state !== 'active') throw new Error('Expected the exact active native writer');
const env = {};
for (const item of fs.readFileSync('/proc/' + status.pid + '/environ', 'utf8').split('\0')) {
  const k = item.indexOf('=');
  if (k > 0) env[item.slice(0, k)] = item.slice(k + 1);
}
function call(repo, name = 'read_wiki_structure', target = 'https://sproozi-gateway.sproozi-system.svc/mcp/docs') {
  const body = JSON.stringify({jsonrpc: '2.0', id: 1, method: 'tools/call', params: {name, arguments: {repoName: repo}}});
  const r = cp.spawnSync('/usr/bin/curl', ['--silent', '--show-error', '--max-time', '45',
    '--proxy-cacert', env.SSL_CERT_FILE, '-H', 'Content-Type: application/json',
    '-H', 'Accept: application/json, text/event-stream', '--data-binary', body,
    '--write-out', '\n%{http_code} %{http_connect}', target], {env, encoding: 'utf8', maxBuffer: 1024 * 1024});
  if (r.error) throw new Error('Worker gateway transport failed');
  const split = r.stdout.lastIndexOf('\n');
  const codes = r.stdout.slice(split + 1).split(' ').map(Number);
  const code = codes[0] || codes[1];
  if (r.status !== 0 && code < 400) throw new Error('Worker transport failed without gateway denial');
  const text = r.stdout.slice(0, split);
  let reply;
  try { reply = JSON.parse(text); } catch (_) { reply = null; }
  return {code, reply};
}
const allowed = call('modelcontextprotocol/go-sdk');
if (allowed.code !== 200 || !allowed.reply?.result || allowed.reply.result.isError ||
    !JSON.stringify(allowed.reply.result).includes('Overview')) throw new Error('Real DeepWiki result missing');
const wrongRepo = call('andrewmccall/sproozi');
const wrongTool = call('modelcontextprotocol/go-sdk', 'ask_question');
const direct = call('modelcontextprotocol/go-sdk', 'read_wiki_structure', 'https://mcp.deepwiki.com/mcp');
if ([wrongRepo, wrongTool, direct].some(r => r.code < 400 && !r.reply?.error)) throw new Error('Scope/direct-upstream denial missing');
process.stdout.write(JSON.stringify({passed: true, threadId: status.threadId,
  provider: 'https://mcp.deepwiki.com/mcp', tool: 'read_wiki_structure', repository: 'modelcontextprotocol/go-sdk',
  allowed: allowed.code, wrongRepository: wrongRepo.code, wrongTool: wrongTool.code, directUpstream: direct.code}));
