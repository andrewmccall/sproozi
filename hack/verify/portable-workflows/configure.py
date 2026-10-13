#!/usr/bin/env python3
"""Extend the owned Codex mobility fixture with retained tasks and optional DeepWiki."""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess

import yaml

ROOT = Path(__file__).resolve().parents[3]


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')
    path.chmod(0o600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fixture', type=Path, required=True)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--deepwiki', action='store_true')
    args = parser.parse_args()
    os.umask(0o077)
    fixture = args.fixture.resolve()
    home_path, profile_path = fixture / 'home-config.json', fixture / 'profile-config.json'
    home, profile = json.loads(home_path.read_text()), json.loads(profile_path.read_text())
    if Path(home['gateway']['providerTokenFile']).read_text().strip() != 'tui-placeholder':
        raise RuntimeError('Only the deterministic credential-free fixture is supported')
    if (Path(home['directory']) / 'control.sock').exists():
        raise RuntimeError('Stop the fixture home before changing approval')
    token = fixture / 'work-token'
    if not token.exists():
        token.write_text(secrets.token_hex(32) + '\n')
        token.chmod(0o600)
    repo = fixture / 'source'
    if not repo.exists():
        repo.mkdir(mode=0o700)
        subprocess.run(['git', 'init', '--quiet', str(repo)], check=True)
        (repo / 'note.txt').write_text('Portable workflow fixture\n')
        subprocess.run(['git', '-C', str(repo), 'add', 'note.txt'], check=True)
        subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
                        'commit', '--quiet', '-m', 'Initial fixture'], check=True)
    profile['workTokenFile'] = str(token)
    profile['repositories'] = {'sproozi': {'snapshot': str(repo)}}
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
    write(home_path, home)
    write(profile_path, profile)
    args.output.write_text(yaml.safe_dump_all(documents, sort_keys=False))
    args.output.chmod(0o600)
    print(json.dumps({'configured': True, 'deepwiki': args.deepwiki, 'manifest': str(args.output)}))


if __name__ == '__main__':
    main()
