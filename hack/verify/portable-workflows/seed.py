#!/usr/bin/env python3
"""Seed only public Sproozi compiler sources into the owned fixture's PVC."""
import argparse
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[3]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fixture', type=Path, required=True)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    fixture = args.fixture.resolve()
    profile = json.loads((fixture / 'profile-config.json').read_text())
    if (fixture / 'home/control.sock').exists():
        raise ValueError('Seed before starting the home or submitting work')
    kube = ['kubectl', '--kubeconfig', str(args.kubeconfig.resolve())]
    context = subprocess.check_output([*kube, 'config', 'current-context'], text=True).strip()
    if not context.startswith('kind-sproozi-mobility-'):
        raise ValueError('Use the explicit isolated mobility Kind cluster')
    source = fixture / 'sproozi-source'
    if source.exists():
        raise ValueError('Use a fresh seed directory; source revisions are immutable')
    source.mkdir(mode=0o700)
    # No working directory archive: configuration, credentials, docs, build
    # caches and private proof state never enter the source export.
    paths = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard',
                                     '--', 'api', 'cmd', 'internal', 'go.mod', 'go.sum'], cwd=ROOT).split(b'\0')
    for name in filter(None, paths):
        relative = Path(os.fsdecode(name))
        current = ROOT / relative
        if not current.is_file() or current.is_symlink():
            raise ValueError('Expected ordinary public compiler source: ' + str(relative))
        destination = source / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(current, destination)
    for arguments in (['init', '--quiet', '--template=', str(source)], ['-C', str(source), 'add', '.'],
                      ['-C', str(source), '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
                       'commit', '--quiet', '-m', 'Exact public Sproozi compiler source fixture']):
        subprocess.run(['git', *arguments], check=True)
    revision = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode='w') as archive:
        archive.add(source, arcname='.')
    remote = '/state/source-sproozi'
    subprocess.run([*kube, 'exec', '-i', '-n', profile['namespace'], 'deployment/sproozi-work', '--',
                    '/bin/sh', '-ec', 'test ! -e ' + remote + '; mkdir -m 700 ' + remote + '; tar -x -C ' + remote],
                   input=data.getvalue(), check=True)
    record = dict(revision=revision, repositoryReference='https://github.com/andrewmccall/sproozi',
                  source='public compiler source snapshot; upstream commit provenance is not claimed')
    (fixture / 'source-seed.json').write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps(record))


if __name__ == '__main__':
    main()
