#!/usr/bin/env python3
"""Extend the owned Codex mobility fixture with retained tasks and optional DeepWiki."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import importlib.util
import subprocess

import yaml

ROOT = Path(__file__).resolve().parents[3]
_spec = importlib.util.spec_from_file_location('mobility_kubernetes', ROOT / 'hack/verify/session-mobility/kubernetes.py')
KUBE = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(KUBE)


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')
    path.chmod(0o600)


def configure_home_ops(home, profile, documents, fixture, kubeconfig):
    kube = ['kubectl', '--kubeconfig', str(kubeconfig.resolve())]
    def output(*args):
        return subprocess.run([*kube, *args], capture_output=True, text=True, check=True).stdout
    context = output('config', 'current-context').strip()
    if not context.startswith('kind-sproozi-mobility-'):
        raise ValueError('Home-ops verification requires an explicit dedicated mobility Kind cluster')
    config = json.loads(output('config', 'view', '--minify', '--raw', '-o', 'json'))
    upstream = config['clusters'][0]['cluster']
    resources = [
        {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'sproozi-demo'}},
        {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': 'home-ops-reader', 'namespace': 'sproozi-demo'}},
        {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
         'metadata': {'name': 'home-ops-reader', 'namespace': 'sproozi-demo'},
         'rules': [{'apiGroups': [''], 'resources': ['pods', 'pods/log', 'events'], 'verbs': ['get', 'list']}]},
        {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding',
         'metadata': {'name': 'home-ops-reader', 'namespace': 'sproozi-demo'},
         'subjects': [{'kind': 'ServiceAccount', 'name': 'home-ops-reader', 'namespace': 'sproozi-demo'}],
         'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Role', 'name': 'home-ops-reader'}},
        {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'home-ops-target', 'namespace': 'sproozi-demo'},
         'spec': {'automountServiceAccountToken': False,
                  'securityContext': {'runAsNonRoot': True, 'runAsUser': 1000, 'seccompProfile': {'type': 'RuntimeDefault'}},
                  'containers': [{'name': 'target', 'image': next(d['spec']['podTemplate']['spec']['containers'][0]['image']
                       for d in documents if d and d['kind'] == 'AgentRuntime' and d['metadata']['name'] == profile['templateName']),
                     'command': ['/bin/sleep', '3600'],
                     'resources': {'requests': {'cpu': '10m', 'memory': '32Mi'}, 'limits': {'cpu': '100m', 'memory': '64Mi'}},
                     'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                                         'capabilities': {'drop': ['ALL']}}}]}}]
    subprocess.run([*kube, 'apply', '-f', '-'], input=yaml.safe_dump_all(resources), text=True, check=True,
                   stdout=subprocess.DEVNULL)
    token, ca = fixture / 'kubernetes-read-token', fixture / 'kubernetes-ca.pem'
    token.write_text(output('create', 'token', 'home-ops-reader', '-n', 'sproozi-demo', '--duration=1h'))
    ca.write_bytes(base64.b64decode(upstream['certificate-authority-data']))
    token.chmod(0o600)
    ca.chmod(0o600)
    scope = {'namespaces': ['sproozi-demo'], 'resources': ['pods', 'pods/log', 'events']}
    home['gateway']['kubernetes'] = dict(upstreamURL=upstream['server'], tokenFile=str(token), caFile=str(ca), scope=scope)
    allowance = {'units': {'bounded': True, 'value': 50}, 'costMicros': {'bounded': False, 'value': 0}}
    home['allowances']['kubernetes.read'] = allowance
    profile['allowances']['kubernetes.read'] = allowance
    for d in documents:
        if not d:
            continue
        if d['kind'] == 'AgentPolicy' and d['metadata']['name'] == profile['templateName']:
            d['spec']['allowedCapabilities'].append('kubernetes.read')
            d['spec']['budgets']['kubernetes.read'] = {'maxUnits': 50, 'maxCostMicros': 0}
            d['spec']['kubernetesRead'] = scope
        if d['kind'] == 'Deployment' and d['metadata']['name'] == 'sproozi-gateway':
            for env in d['spec']['template']['spec']['containers'][0]['env']:
                if env['name'] == 'SPROOZI_ENABLED_CAPABILITIES':
                    env['value'] += ',kubernetes.read'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fixture', type=Path, required=True)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--deepwiki', action='store_true')
    parser.add_argument('--projects', action='store_true', help='Configure distinct dev/home-ops projects and independent home approvals')
    parser.add_argument('--build-sproozi', action='store_true', help='Increase bounded compiler scratch/memory for offline Sproozi tests')
    parser.add_argument('--home-ops-kubeconfig', type=Path,
                        help='Configure isolated Pod-read/Secrets-denial proof, including a restricted local gateway token')
    args = parser.parse_args()
    os.umask(0o077)
    fixture = args.fixture.resolve()
    home_path, profile_path = fixture / 'home-config.json', fixture / 'profile-config.json'
    home, profile = json.loads(home_path.read_text()), json.loads(profile_path.read_text())
    if Path(home['gateway']['providerTokenFile']).read_text().strip() != 'tui-placeholder':
        raise RuntimeError('Only the deterministic credential-free fixture is supported')
    if (Path(home['directory']) / 'control.sock').exists():
        raise RuntimeError('Stop the fixture home before changing approval')
    # The Hermes work token stays a static token; the executor reads it from
    # its credentials Secret, which the administrator creates from this file.
    token = fixture / 'work-token'
    if not token.exists():
        token.write_text(secrets.token_hex(32) + '\n')
        token.chmod(0o600)
    profile['workTokenFile'] = KUBE.CREDENTIALS + '/task-token'
    # The executor's init container seeds this administrator-owned snapshot.
    profile['repositories'] = {'sproozi': {'snapshot': '/state/source'}}
    documents = list(yaml.safe_load_all(args.manifest.read_text()))
    if args.deepwiki:
        registry = ROOT / 'examples/portable-workflows/deepwiki-servers.json'
        local_registry = fixture / 'mcp-servers.json'
        local_registry.write_bytes(registry.read_bytes())
        local_registry.chmod(0o600)
        policy = yaml.safe_load((ROOT / 'examples/portable-workflows/deepwiki-policy.yaml').read_text())['spec']
        allowance = {'units': {'bounded': True, 'value': 10}, 'costMicros': {'bounded': False, 'value': 0}}
        home['allowances']['mcp.docs'] = allowance
        profile['allowances']['mcp.docs'] = allowance
        home['gateway'].update(mcpRegistryFile=str(local_registry), mcpAuthority='sproozi-gateway.sproozi-system.svc:443',
                               mcpPolicy=policy['mcpServers'])
        found_registry = found_policy = False
        for document in documents:
            if not document:
                continue
            if document['kind'] == 'ConfigMap' and document['metadata']['name'] == 'sproozi-mcp-servers':
                document['data']['servers.json'] = registry.read_text()
                found_registry = True
            if document['kind'] == 'AgentPolicy' and document['metadata']['name'] == profile['templateName']:
                document['spec']['allowedCapabilities'] = ['model.inference', 'mcp.docs']
                document['spec']['budgets']['mcp.docs'] = {'maxUnits': 10, 'maxCostMicros': 0}
                document['spec']['mcpServers'] = policy['mcpServers']
                found_policy = True
        if not found_registry or not found_policy:
            raise RuntimeError('Expected the complete mobility registry and policy')
    if args.home_ops_kubeconfig:
        configure_home_ops(home, profile, documents, fixture, args.home_ops_kubeconfig)
    if args.projects:
        profile['repositories']['sproozi']['reference'] = 'https://github.com/andrewmccall/sproozi'
        capabilities = ['model.inference']
        if args.home_ops_kubeconfig:
            capabilities.append('kubernetes.read')
        profile['projects'] = {
            'sproozi-dev': dict(template=profile['templateName'], workspace='sproozi',
                                capabilities=['model.inference'], members=[profile['principal']]),
            'home-ops': dict(template=profile['templateName'], workspace='sproozi',
                            capabilities=capabilities, members=[profile['principal']])}
        home['projectApprovals'] = [dict(source=profile['placementName'], project=name,
                                        capabilities=definition['capabilities'])
                                   for name, definition in profile['projects'].items()]
    if args.build_sproozi:
        home['local']['AgentLimits']['MemoryBytes'] = 2 << 30
        home['local']['VolumeBytes'] = 2 << 30
        for d in documents:
            if not d:
                continue
            if d['kind'] == 'AgentRuntime' and d['metadata']['name'] == profile['templateName']:
                d['spec']['ephemeralWorkspace']['sizeLimit'] = '2Gi'
                for container in d['spec']['podTemplate']['spec']['containers']:
                    if container['name'] == 'agent':
                        for key in ('requests', 'limits'):
                            container['resources'][key]['memory'] = '2Gi'
            if d['kind'] == 'AgentPolicy' and d['metadata']['name'] == profile['templateName']:
                d['spec']['resourceBounds']['max']['memory'] = '2560Mi'
                d['spec']['resourceBounds']['max']['ephemeral-storage'] = '2Gi'
    executor = next(d for d in documents if d and d['kind'] == 'Deployment' and d['metadata']['name'] == KUBE.EXECUTOR)
    published = next(d for d in documents if d and d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'sproozi-remote-executor')
    documents = KUBE.replace_executor(documents, profile, executor['spec']['template']['spec']['containers'][0]['image'],
                                      published['data']['ca.crt'])
    write(home_path, home)
    write(profile_path, profile)
    args.output.write_text(yaml.safe_dump_all(documents, sort_keys=False))
    args.output.chmod(0o600)
    print(json.dumps({'configured': True, 'deepwiki': args.deepwiki, 'manifest': str(args.output)}))


if __name__ == '__main__':
    main()
