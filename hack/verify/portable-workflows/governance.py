"""Check actual execution-bound audit events and retained fixture artifacts."""
import base64
import io
import json
from pathlib import Path
import subprocess
import tarfile


def verify(home, fixture, kubeconfig, session_id, output):
    catalogue = Path(home['directory']) / 'sessions'
    record = json.loads((catalogue / (session_id + '.json')).read_text())
    remote = record['original']['stopped']['incarnation']['executionId']
    local = record['committed']['incarnation']['resourceUid']
    remote_log = subprocess.run(['kubectl', '--kubeconfig', str(kubeconfig.resolve()), 'logs',
                                 '-n', 'sproozi-system', 'deployment/sproozi-gateway'],
                                capture_output=True, text=True, check=True).stdout
    (output / 'remote-audit.log').write_text(remote_log)
    events = {}
    for name, uid, text in [('remote', remote, remote_log), ('local', local, (output / 'home.log').read_text())]:
        selected = []
        for line in text.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get('runUID') == uid and event.get('capability') == 'kubernetes.read':
                selected.append(event)
        if not any(e.get('allowed') is True and e.get('upstreamStatus') == 200 for e in selected):
            raise RuntimeError(name + ' lacks an execution-bound approved Kubernetes audit event')
        if not any(e.get('allowed') is False and e.get('denyReason') for e in selected):
            raise RuntimeError(name + ' lacks an execution-bound Kubernetes denial audit event')
        events[name] = selected
    secrets = [(fixture / 'work-token').read_bytes().strip(),
               Path(home['gateway']['kubernetes']['tokenFile']).read_bytes().strip()]
    for secret in secrets:
        if secret in (remote_log + (output / 'home.log').read_text()).encode():
            raise RuntimeError('A fixture credential entered audit logs')
    for stopped in [record['original']['stopped'], record['committed']]:
        data = (catalogue / (stopped['checkpointDigest'] + '.checkpoint')).read_bytes()
        artifact = json.loads(data)
        buffers = [data, base64.b64decode(artifact['codex']['Rollout'])]
        workspace = base64.b64decode(artifact['workspace'])
        with tarfile.open(fileobj=io.BytesIO(workspace), mode='r:') as archive:
            for member in archive:
                if member.isfile():
                    buffers.append(archive.extractfile(member).read())
        if any(secret in buffer for secret in secrets for buffer in buffers):
            raise RuntimeError('A fixture owner/gateway credential entered portable state')
    return dict(audit=events, fixtureCredentialsAbsentFromAuditAndSelectedArtifacts=True)
