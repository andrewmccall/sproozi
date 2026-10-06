#!/usr/bin/env python3
"""Render non-secret deployment wiring for isolated configured MCP acceptance."""

import argparse
import json
from pathlib import Path
import subprocess
import yaml


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True)
    parser.add_argument('--provider-image', required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    rendered = subprocess.check_output([str(root / 'bin/kustomize'), 'build', str(root / 'config/default')], text=True)
    objects = list(yaml.safe_load_all(rendered))
    agent_rbac = subprocess.check_output([str(root / 'bin/kustomize'), 'build', str(root / 'config/agent-rbac')], text=True)
    objects += list(yaml.safe_load_all(agent_rbac))
    for obj in objects:
        if obj['kind'] == 'Deployment':
            pod = obj['spec']['template']['spec']
            for container in pod['containers']:
                container['image'] = args.image
                container['imagePullPolicy'] = 'IfNotPresent'
            if obj['metadata']['name'] == 'sproozi-gateway':
                container = pod['containers'][0]
                for env in container['env']:
                    if env['name'] == 'SPROOZI_ENABLED_CAPABILITIES':
                        env['value'] = 'model.inference'
                container['env'].append({'name': 'SSL_CERT_FILE', 'value': '/etc/sproozi/fixture-trust/ca-bundle.pem'})
                container['volumeMounts'] += [
                    {'name': 'mcp-credentials', 'mountPath': '/var/run/secrets/sproozi/mcp', 'readOnly': True},
                    {'name': 'fixture-trust', 'mountPath': '/etc/sproozi/fixture-trust', 'readOnly': True},
                ]
                pod['volumes'] += [
                    {'name': 'mcp-credentials', 'secret': {'secretName': 'mcp-provider-credentials', 'items': [
                        {'key': 'docs-token', 'path': 'docs/token'}, {'key': 'inventory-token', 'path': 'inventory/token'}]}},
                    {'name': 'fixture-trust', 'configMap': {'name': 'mcp-provider-trust'}},
                ]
        if obj['kind'] == 'ConfigMap' and obj['metadata']['name'] == 'sproozi-mcp-servers':
            obj['data']['servers.json'] = json.dumps({'servers': {
                name: {'url': 'https://mcp-' + provider + '.sproozi-system.svc:8443/mcp',
                       'bearerTokenFile': '/var/run/secrets/sproozi/mcp/' + provider + '/token'}
                for name, provider in [('docs', 'docs'), ('inventory', 'inventory'), ('unselected', 'docs')]}})
    objects.append({
        'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
        'metadata': {'name': 'mcp-fixture-ingress', 'namespace': 'sproozi-system'},
        'spec': {'podSelector': {'matchExpressions': [{'key': 'app', 'operator': 'In',
                                                      'values': ['mcp-docs', 'mcp-inventory']}]},
                 'policyTypes': ['Ingress'], 'ingress': [{'from': [{'podSelector': {'matchLabels': {
                     'app.kubernetes.io/component': 'shared-gateway'}}}],
                     'ports': [{'port': 8443, 'protocol': 'TCP'}]}]},
    })
    for name in ('docs', 'inventory'):
        labels = {'app': 'mcp-' + name}
        objects += [
            {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': 'mcp-' + name, 'namespace': 'sproozi-system'},
             'spec': {'selector': labels, 'ports': [{'port': 8443, 'targetPort': 8443}]}},
            {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': 'mcp-' + name, 'namespace': 'sproozi-system'},
             'spec': {'replicas': 1, 'selector': {'matchLabels': labels}, 'template': {'metadata': {'labels': labels},
                 'spec': {'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                             'seccompProfile': {'type': 'RuntimeDefault'}},
                    'containers': [{'name': 'provider', 'image': args.provider_image, 'imagePullPolicy': 'IfNotPresent',
                        'env': [{'name': 'PROVIDER', 'value': name}], 'ports': [{'containerPort': 8443}],
                        'securityContext': {'readOnlyRootFilesystem': True, 'allowPrivilegeEscalation': False,
                                            'capabilities': {'drop': ['ALL']}},
                        'volumeMounts': [{'name': 'tls', 'mountPath': '/tls', 'readOnly': True},
                                         {'name': 'credentials', 'mountPath': '/credentials', 'readOnly': True}]}],
                    'volumes': [{'name': 'tls', 'secret': {'secretName': 'sproozi-gateway-tls'}},
                                {'name': 'credentials', 'secret': {'secretName': 'mcp-provider-credentials',
                                                                 'items': [{'key': name + '-token', 'path': 'token'}]}}]}}}},
        ]
    print(yaml.safe_dump_all(objects, sort_keys=False), end='')


if __name__ == '__main__':
    main()
