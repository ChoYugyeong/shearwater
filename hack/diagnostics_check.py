"""Exercise the diagnostic CLI against actual container failures, not mocks.

All fixtures and evidence stay under Shearwater's directories on Linux ext4.
No host package is installed, no containerd configuration is read, and only the
`shearwater` namespace on the explicitly selected socket is queried. The caller
must supply network isolation; refuse an existing sw network or runtime first.
"""

import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time


def main():
    if os.geteuid() != 0 or len(sys.argv) != 3:
        raise SystemExit('Use diagnostics-check.sh as root in an isolated runtime')
    os.umask(0o077)
    sw, kubectl = sys.argv[1:]
    address = os.environ.get('SW_CONTAINERD_SOCKET', '/run/containerd/containerd.sock')
    port = int(os.environ.get('SW_DIAGNOSTICS_PORT', '16456'))
    image = os.environ.get('SW_DIAGNOSTICS_IMAGE', 'busybox:1.36')
    assert address.startswith('/') and 0 < port < 65536
    ctr = ['ctr', '--address', address, '--namespace', 'shearwater']
    for resource in ['containers', 'tasks']:
        assert not subprocess.check_output(ctr + [resource, 'list', '-q'], timeout=20).strip(), 'Runtime is not empty'
    for link in ['sw-br0', 'sw-svc0']:
        assert subprocess.run(['ip', 'link', 'show', 'dev', link], capture_output=True).returncode != 0, 'Existing sw network'
    for args in [['-S', 'SHEARWATER-FORWARD'], ['-t', 'nat', '-S', 'SHEARWATER-POSTROUTING']]:
        assert subprocess.run(['iptables', '-w', '2'] + args, capture_output=True).returncode != 0, 'Existing sw rules'
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', port))
    root = Path('/var/lib/shearwater/check')
    root.mkdir(parents=True, exist_ok=True)
    assert root.resolve() == root
    assert subprocess.check_output(['findmnt', '-n', '-o', 'FSTYPE', '--target', str(root)], text=True).strip() == 'ext4'
    fixture = Path(tempfile.mkdtemp(prefix='diagnostics.', dir=root))
    Path('/run/shearwater').mkdir(exist_ok=True)
    run_dir = Path(tempfile.mkdtemp(prefix='diagnostics.', dir='/run/shearwater'))
    flags = [f'--listen=127.0.0.1:{port}', f'--data-dir={fixture}/data', f'--config-dir={fixture}/etc',
             f'--log-dir={fixture}/log', f'--run-dir={run_dir}', '--node-name=diagnostics-check',
             f'--containerd-socket={address}']
    connection = [f'--listen=127.0.0.1:{port}', f'--config-dir={fixture}/etc', '-n=diagnostics']
    kcmd = [kubectl, f'--kubeconfig={fixture}/etc/kubeconfig', f'--cache-dir={fixture}/discovery', '-n=diagnostics']
    checks = []
    serial = 0
    owned = False
    passed = False
    cleanup = 'not-started'

    def run(args, data=None, ok=True):
        nonlocal serial
        serial += 1
        result = subprocess.run(args, input=data, capture_output=True, text=True, timeout=150)
        (fixture / f'command-{serial:04}.json').write_text(json.dumps(dict(
            command=args, exitCode=result.returncode, stdout=result.stdout, stderr=result.stderr), indent=2))
        if ok and result.returncode:
            raise AssertionError(f'Command failed: {args[0:3]}: {result.stderr}')
        return result

    def k(*args, **kwargs):
        return run(kcmd + list(args), **kwargs)

    def diag(command, *args, **kwargs):
        return run([sw, command] + list(args) + connection, **kwargs)

    def obj(name):
        return json.loads(k('get', 'pod', name, '-o', 'json').stdout)

    def wait(name, predicate, seconds=120):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            pod = obj(name)
            if predicate(pod):
                return pod
            time.sleep(.4)
        raise AssertionError(f'Timed out waiting for {name}; inspect retained Pod/Event evidence')

    def waiting(pod, reasons):
        return any(c.get('state', {}).get('waiting', {}).get('reason') in reasons
                   for c in pod.get('status', {}).get('containerStatuses', []))

    def create(name, command, *, pod_image=None, restart='Always', extra=None, containers=None):
        spec = dict(restartPolicy=restart, containers=containers or [dict(name='app', image=pod_image or image, command=command)])
        spec.update(extra or {})
        body = dict(apiVersion='v1', kind='Pod', metadata=dict(name=name, labels={'suite': 'diagnostics'}), spec=spec)
        k('apply', '-f', '-', data=json.dumps(body))

    def check(name, condition):
        if not condition:
            raise AssertionError(name)
        checks.append(name)
        print('PASS ' + name, flush=True)

    def why(name):
        # Restarting fixtures may legitimately change during one diagnostic
        # read. Retry only the explicit CONFLICT response, never other errors.
        for _ in range(12):
            result = diag('why', 'pod/' + name, '-o=json', ok=False)
            if result.returncode == 0:
                return json.loads(result.stdout)
            if 'CONFLICT retry=safe' not in result.stderr:
                raise AssertionError(result.stderr)
            time.sleep(.2)
        raise AssertionError('Pod never stayed stable long enough for why')

    try:
        owned = True
        run([sw, 'up'] + flags)
        k('create', 'namespace', 'diagnostics')
        # Loopback port 1 has no registry in this isolated namespace. This
        # produces a real, fast image pull failure without a guessed Hub tag.
        with socket.socket() as probe:
            assert probe.connect_ex(('127.0.0.1', 1)) != 0
        create('bad-image', ['true'], pod_image='127.0.0.1:1/shearwater-missing:v1')
        bad = wait('bad-image', lambda p: waiting(p, {'ErrImagePull', 'ImagePullBackOff'}))
        report = why('bad-image')
        check('image pull failure has a reason and actionable next check',
              any(f['code'] in ['ErrImagePull', 'ImagePullBackOff'] and 'image' in f['next'] for f in report['findings']))
        check('unstarted container is reported as unavailable, not an empty healthy log',
              any('has not started' in log.get('unavailable', '') for log in report['logs']))
        check('events refer only to the selected Pod UID', bool(report['events']['items']) and
              all(e['involvedObject']['uid'] == bad['metadata']['uid'] for e in report['events']['items']))

        create('crash', ['sh', '-c', 'echo diagnostics-crash-marker; exit 7'])
        wait('crash', lambda p: waiting(p, {'CrashLoopBackOff'}) and
             any(c.get('restartCount', 0) >= 1 for c in p['status']['containerStatuses']))
        report = why('crash')
        check('crash diagnosis includes exit code and previous/current output',
              any(c.get('lastState', {}).get('terminated', {}).get('exitCode') == 7 for c in report['containers']) and
              any('diagnostics-crash-marker' in log['text'] for log in report['logs']))
        check('previous log can be selected explicitly', 'diagnostics-crash-marker' in diag('logs', 'pod/crash', '--previous').stdout)

        wrong_path = fixture / 'a-file-not-a-directory'
        wrong_path.write_text('retained test fixture\n')
        create('bad-volume', ['sleep', '3600'], extra=dict(
            volumes=[dict(name='data', hostPath=dict(path=str(wrong_path), type='Directory'))],
            containers=[dict(name='app', image=image, command=['sleep', '3600'], volumeMounts=[dict(name='data', mountPath='/data')])]))
        wait('bad-volume', lambda p: waiting(p, {'CreatePodSandboxError'}))
        report = why('bad-volume')
        check('wrong hostPath type is explained', any(f['code'] == 'CreatePodSandboxError' and 'volume' in f['next'] for f in report['findings']))

        create('bad-command', ['/definitely-not-an-executable'])
        wait('bad-command', lambda p: waiting(p, {'RunContainerError'}))
        report = why('bad-command')
        check('missing executable is explained without inventing a log', any(f['code'] == 'RunContainerError' and 'executable' in f['next'] for f in report['findings']) and all(log.get('unavailable') for log in report['logs']))

        create('failed-once', ['sh', '-c', 'echo diagnostics-exit-marker; exit 9'], restart='Never')
        wait('failed-once', lambda p: p.get('status', {}).get('phase') == 'Failed')
        report = why('failed-once')
        check('non-restarting failure has current exit code and logs', any(c.get('state', {}).get('terminated', {}).get('exitCode') == 9 for c in report['containers']) and any('diagnostics-exit-marker' in log['text'] for log in report['logs']))

        create('multi', [], containers=[dict(name=n, image=image, command=['sh', '-c', f'echo {n}-marker; sleep 3600']) for n in ['left', 'right']])
        wait('multi', lambda p: len(p.get('status', {}).get('containerStatuses', [])) == 2 and all(c.get('ready') for c in p['status']['containerStatuses']))
        result = diag('logs', 'pod/multi', ok=False)
        check('ambiguous container requires an explicit choice', result.returncode != 0 and 'INVALID retry=never' in result.stderr and not result.stdout)
        check('container selection returns the requested output', 'right-marker' in diag('logs', 'pod/multi', '-c=right').stdout)
        check('tail zero and byte limit are honored', diag('logs', 'pod/multi', '-c=left', '--tail=0').stdout == '' and len(diag('logs', 'pod/multi', '-c=left', '--limit-bytes=4').stdout.encode()) <= 4)
        # These containers have no probes. Adding probe execution must not turn
        # a running process into an assertion that the app answered a request.
        explanation = diag('why', 'pod/multi', '-c=left').stdout
        check('healthy process is not presented as a verified healthy app',
              'no probe means no application response check' in explanation and 'probes are stored' not in explanation)

        selected = json.loads(diag('get', 'pods', '-l=suite=diagnostics', '--full').stdout)
        check('get supports labels and full API JSON', len(selected['items']) == 6)
        events = json.loads(diag('events', '--for=pod/bad-image', '-o=json').stdout)
        check('events expose a revision and stable UID filtering', events['resourceVersion'].isdigit() and all(e['involvedObject']['uid'] == bad['metadata']['uid'] for e in events['items']))
        result = diag('get', 'pod/does-not-exist', ok=False)
        check('missing object gives NOT_FOUND with no partial stdout', result.returncode != 0 and 'NOT_FOUND retry=never' in result.stderr and not result.stdout)
        k('delete', 'pod', 'bad-image', '--wait=true', '--timeout=60s')
        create('bad-image', ['true'], pod_image='127.0.0.1:1/shearwater-missing:v1')
        recreated = wait('bad-image', lambda p: waiting(p, {'ErrImagePull', 'ImagePullBackOff'}))
        events = json.loads(diag('events', '--for=pod/bad-image', '-o=json').stdout)
        check('same name after recreation follows the new UID', recreated['metadata']['uid'] != bad['metadata']['uid'] and bool(events['items']) and all(e['involvedObject']['uid'] == recreated['metadata']['uid'] for e in events['items']))
        passed = True
    finally:
        if owned:
            result = run([sw, 'down', '--all'] + flags, ok=False)
            cleanup = 'stopped' if result.returncode == 0 else 'failed'
            if result.returncode:
                passed = False
        result = dict(passed=passed, checks=checks, cleanup=cleanup, fixture=str(fixture), runDir=str(run_dir))
        (fixture / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
        print(json.dumps(result, indent=2), flush=True)
    return 0 if passed else 1


if __name__ == '__main__':
    sys.exit(main())
