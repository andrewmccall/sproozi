// Run inside the actual UID-1000 native container, after trusted admission.
// Only observed status/codes leave the container; effective credentials do not.
const fs = require('fs');
const child = require('child_process');
const statusCall = child.spawnSync('/usr/local/bin/native-session', ['status'], {encoding: 'utf8', timeout: 5000});
if (statusCall.status !== 0) throw new Error('Native status unavailable');
const status = JSON.parse(statusCall.stdout);
if (status.state !== 'active' || !Number.isInteger(status.pid) || status.pid <= 1) throw new Error('Actual native process is not active');
const entries = fs.readFileSync(`/proc/${status.pid}/environ`, 'utf8').split('\0').filter(Boolean);
const env = Object.fromEntries(entries.map(entry => {
  const at = entry.indexOf('=');
  return [entry.slice(0, at), entry.slice(at + 1)];
}));
if (!env.HTTPS_PROXY || !env.SSL_CERT_FILE || !env.PATH) throw new Error('Fresh native authority unavailable');
function curl(url, direct) {
  const args = ['--silent', '--show-error', '--max-time', '4', '--output', '/dev/null', '--write-out', '%{http_code}', '--proxy-cacert', env.SSL_CERT_FILE];
  if (direct) args.push('--noproxy', '*');
  args.push(url);
  const result = child.spawnSync('/usr/bin/curl', args, {env, encoding: 'utf8', timeout: 7000});
  return {exitCode: result.status, httpStatus: result.stdout, proxyDenied: /CONNECT tunnel failed, response 403/.test(result.stderr)};
}
const capabilities = {};
for (const [label, url] of Object.entries({kubernetesRead: 'https://kubernetes.default.svc/api/v1/pods', unregisteredMCP: 'https://sproozi-mcp.sproozi-system.svc/mcp/not-granted'})) {
  const value = curl(url, false);
  if (value.httpStatus !== '403' && !value.proxyDenied) throw new Error(`Missing explicit gateway denial for ${label}: ${JSON.stringify(value)}`);
  capabilities[label] = value;
}
const direct = {};
for (const [label, url] of Object.entries({ipv4: 'http://1.1.1.1:80/', ipv6: 'http://[2606:4700:4700::1111]:80/', dns: 'http://example.com/'})) {
  const value = curl(url, true);
  if (value.exitCode === 0 || value.httpStatus !== '000') throw new Error(`Direct ${label} access succeeded`);
  direct[label] = value;
}
const firewall = child.spawnSync('/usr/sbin/iptables', ['-F'], {env, encoding: 'utf8', timeout: 5000});
if (firewall.status === 0) throw new Error('Native UID changed firewall');
console.log(JSON.stringify({passed: true, nativeUID: process.getuid(), capabilityDenials: capabilities, directDenials: direct, firewallMutationDenied: true}));
