// The stock native client runs this through its actual exec_command tool.
const child = require('child_process');
const allowed = child.spawnSync('kubectl', ['get', '--raw=/api/v1/namespaces/sproozi-demo/pods/home-ops-target'],
  {encoding: 'utf8', timeout: 15000});
if (allowed.status !== 0) throw new Error('Approved Pod read failed: ' + allowed.stderr);
const pod = JSON.parse(allowed.stdout);
if (pod.metadata.name !== 'home-ops-target' || pod.status.phase !== 'Running') {
  throw new Error('Approved fixture state was not observed');
}
const denied = child.spawnSync('kubectl', ['get', '--raw=/api/v1/namespaces/sproozi-demo/secrets'],
  {encoding: 'utf8', timeout: 15000});
if (denied.status === 0 || !/Forbidden|403|denied/.test(denied.stderr)) {
  throw new Error('Prohibited Secrets request was not visibly denied');
}
console.log('HOMEOPS_CAPABILITIES_VERIFIED ' + JSON.stringify({pod: pod.metadata.name,
  phase: pod.status.phase, secretsDenied: true}));
