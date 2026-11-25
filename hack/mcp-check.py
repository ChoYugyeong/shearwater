#!/usr/bin/env python3
"""Test real stdio MCP and local approval against a disposable API-only server.

Usage (ordinary Linux/WSL user): python3 hack/mcp-check.py /absolute/path/to/sw
No containerd, root, application deployment or system service is needed. The
server gets a new private directory and an unused loopback port; it never uses
the running cluster's credentials. Keep the fixture for inspection after exit.
"""
import json
import os
from pathlib import Path
import select
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def main():
    if len(sys.argv) != 2 or not Path(sys.argv[1]).is_absolute():
        raise SystemExit('usage: python3 hack/mcp-check.py /absolute/path/to/sw')
    binary = str(Path(sys.argv[1]).resolve(strict=True))
    cache = Path.home() / '.cache/shearwater-dev'
    cache.mkdir(parents=True, exist_ok=True)
    fixture = Path(tempfile.mkdtemp(prefix='mcp-', dir=cache))
    os.chmod(fixture, 0o700)
    config = fixture / 'config'
    with socket.socket() as port_probe:
        port_probe.bind(('127.0.0.1', 0))
        address = f'127.0.0.1:{port_probe.getsockname()[1]}'
    connection = ['--config-dir=' + str(config), '--listen=' + address]
    checks = []
    measurements = {}
    clients = []

    def check(name, condition):
        assert condition, name
        checks.append(name)
        print('PASS ' + name, flush=True)

    def cli(*args, failure=False):
        p = subprocess.run([binary, *args, *connection], text=True,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=40)
        assert (p.returncode != 0) == failure, (args, p.returncode, p.stdout, p.stderr)
        return p.stdout

    class MCP:
        def __init__(self, write=False):
            self.process = subprocess.Popen(
                [binary, 'mcp', *connection, *(['--allow-write'] if write else [])],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, bufsize=1)
            clients.append(self)
            self.counter = 0
            self.call('initialize', dict(protocolVersion='2025-11-25', capabilities={},
                                        clientInfo=dict(name='shearwater-check', version='1')))
            self.process.stdin.write(json.dumps(dict(jsonrpc='2.0', method='notifications/initialized')) + '\n')
            self.process.stdin.flush()

        def call(self, method, params=None):
            self.counter += 1
            request = dict(jsonrpc='2.0', id=self.counter, method=method, params=params or {})
            self.process.stdin.write(json.dumps(request) + '\n')
            self.process.stdin.flush()
            assert select.select([self.process.stdout], [], [], 40)[0], 'MCP response timeout'
            response = json.loads(self.process.stdout.readline())
            assert response.get('id') == self.counter and 'error' not in response, response
            return response['result']

        def tool(self, name, arguments=None, failure=False):
            response = self.call('tools/call', dict(name=name, arguments=arguments or {}))
            assert bool(response.get('isError')) == failure, response
            return response['content'][0]['text']

        def close(self):
            if self.process.poll() is None:
                self.process.stdin.close()
                try:
                    self.process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    # Only the child created above is stopped, never a process
                    # found by name or an existing user's MCP connection.
                    self.process.kill()
                    self.process.wait(timeout=10)
            assert self.process.returncode == 0, self.process.stderr.read()

    passed = False
    log = (fixture / 'server.log').open('w')
    server = subprocess.Popen([binary, 'server', '--runtime=none', *connection,
                               '--data-dir=' + str(fixture / 'data')], stdout=log, stderr=log)
    try:
        deadline = time.monotonic() + 20
        while True:
            assert server.poll() is None, 'Disposable API exited; inspect server.log'
            try:
                # The CA belongs to this fresh process, not a global trust store.
                context = ssl.create_default_context(cafile=str(config / 'ca.crt'))
                with urllib.request.urlopen('https://' + address + '/readyz', context=context, timeout=1) as response:
                    assert response.read().strip() == b'ok'
                break
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(.1)
        read = MCP()
        names = {item['name'] for item in read.call('tools/list')['tools']}
        check('read-only mode exposes diagnostics and agent observation tools', names == {
            'sw_status', 'sw_tree', 'sw_get', 'sw_logs', 'sw_events', 'sw_why',
            'sw_capabilities', 'sw_observe', 'sw_verify', 'sw_verify_plan', 'sw_receipt', 'sw_changes'})
        check('capabilities explicitly distinguish readiness from HTTP',
              json.loads(read.tool('sw_capabilities'))['observation']['reachability'] == 'not checked')
        read.tool('sw_apply', {'plan': '0' * 64}, failure=True)
        read.tool('sw_get', {'resource': '--config-dir=/another-instance'}, failure=True)
        check('read-only writes and resource-as-flag injection are refused', True)
        writer = MCP(write=True)
        manifest = dict(apiVersion='v1', kind='Namespace', metadata=dict(name='reviewed'))
        planned = json.loads(writer.tool('sw_plan', dict(manifest=manifest)))
        plan = planned['plan']
        before = json.loads(read.tool('sw_get', dict(resource='namespaces', full=True)))
        check('planning does not create its target', not any(x['metadata']['name'] == 'reviewed' for x in before['items']))
        writer.tool('sw_apply', dict(plan=plan), failure=True)
        cli('approve', '--plan=' + plan, '--confirm=' + '0' * 64, failure=True)
        check('apply requires the exact locally confirmed plan', True)
        saved = json.loads(cli('plan', '--plan=' + plan))
        check('CLI reviews the saved before/after without creating a new plan', saved['actions'] == planned['actions'])
        cli('approve', '--plan=' + plan, '--confirm=' + plan)
        applied = json.loads(writer.tool('sw_apply', dict(plan=plan)))
        check('approved MCP apply records the exact created object', applied['state'] == 'complete' and applied['applied'] == ['/api/v1/namespaces/reviewed'])
        after = json.loads(read.tool('sw_get', dict(resource='namespace/reviewed', full=True)))
        check('the approved object exists on the real API', after['metadata']['name'] == 'reviewed')
        writer.tool('sw_apply', dict(plan=plan), failure=True)
        check('a consumed plan cannot be replayed', True)
        # before/after can exceed the response limit even when a manifest is
        # valid. The small reply must still contain a usable plan ID and a way
        # for the human to review every byte before approval.
        annotation = 'large review value; ' * 9000
        manifest['metadata'] = dict(name='large-preview', annotations=dict(review=annotation))
        large = json.loads(writer.tool('sw_plan', dict(manifest=manifest)))
        full = json.loads(cli('plan', '--plan=' + large['plan']))
        check('large MCP preview remains valid JSON with an exact full-review command',
              large['fullPreview'] is False and large['reviewCommand'] == 'sw plan --plan=' + large['plan'])
        check('the saved large plan preserves the complete manifest', full['actions'][0]['body']['metadata']['annotations']['review'] == annotation)
        app = json.loads(writer.tool('sw_app_plan', dict(action='create', name='intent-web', image='nginx:alpine', port=80)))
        check('typed app plan is concise and does not claim deployment', not app['applied'] and app['reviewRequired'])
        writer.tool('sw_apply', dict(plan=app['plan']), failure=True)
        cli('approve', '--plan=' + app['plan'], '--confirm=' + app['plan'])
        writer.tool('sw_apply', dict(plan=app['plan']))
        receipt = json.loads(read.tool('sw_receipt', dict(plan=app['plan'])))
        check('execution receipt is readable through read-only MCP', receipt['state'] == 'complete')
        check('write responses bind both original app verification goals', len(receipt['goals']) == 2)
        bound = json.loads(read.tool('sw_verify_plan', dict(plan=app['plan'])))
        check('applied API-only goals are not confused with ready workloads', bound['state'] != 'met')
        observation = read.call('tools/call', dict(name='sw_observe', arguments=dict(resource='deploy/intent-web')))
        state = observation['structuredContent']
        full_observation = read.call('tools/call', dict(name='sw_get', arguments=dict(resource='deploy/intent-web', full=True)))
        encoded_size = lambda value: len(json.dumps(value, ensure_ascii=False, separators=(',', ':')).encode('utf-8'))
        measurements['deploymentObservation'] = dict(
            compactTextBytes=len(observation['content'][0]['text'].encode('utf-8')),
            fullTextBytes=len(full_observation['content'][0]['text'].encode('utf-8')),
            compactMCPResultBytes=encoded_size(observation),
            fullMCPResultBytes=encoded_size(full_observation),
            note='One API-only Deployment. Bytes, not tokens or model success rate; compact result also carries structuredContent.')
        check('structured observation matches compatibility text JSON', state == json.loads(observation['content'][0]['text']))
        item = state['items'][0]
        goal = dict(resource=item['resource'], uid=item['uid'], generation=item['generation'])
        result = json.loads(read.tool('sw_verify', goal))
        check('API-only deployment is not falsely declared ready', result['state'] != 'met' and result['observation']['reachability'] == 'not_checked')
        cli('verify', '--resource=deploy/intent-web', '--uid=' + item['uid'], '--generation=' + str(item['generation']), failure=True)
        stale = json.loads(read.tool('sw_verify', dict(goal, uid='replaced-uid')))
        check('verification rejects a replacement with the same name', stale['state'] == 'stale')
        scale = json.loads(writer.tool('sw_app_plan', dict(action='scale', name='intent-web', replicas=0,
                          uid=item['uid'], resource_version=item['resourceVersion'])))
        check('scale plan carries the exact observed version', scale['intent']['resource_version'] == item['resourceVersion'])
        read.tool('sw_observe', dict(limit=0), failure=True)
        read.tool('sw_observe', dict(resource='--listen=other'), failure=True)
        read.tool('sw_app_plan', dict(action='create', name='not-allowed', image='nginx', port=80), failure=True)
        check('agent schemas and read-only boundary reject invalid actions', True)
        # Optional real-model smoke: no API billing and no production credentials.
        # Set SW_QWEN_SMOKE=qwen2.5:latest only when Ollama runs in this Linux environment.
        model = os.environ.get('SW_QWEN_SMOKE')
        if model:
            before_ai = cli('get', 'deploy/intent-web', '--full')
            command = [binary, 'chat', '--model=' + model, *connection,
                       '--prompt=Use sw_observe with resource deploy/intent-web. Report whether readiness is known. Do not change anything.']
            chat = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=240)
            (fixture / 'qwen-chat.txt').write_text(chat.stdout + '\n' + chat.stderr)
            check('real Qwen completes an observation tool call', chat.returncode == 0 and '[Shearwater 도구 sw_observe]' in chat.stdout)
            check('real Qwen read leaves the target unchanged', cli('get', 'deploy/intent-web', '--full') == before_ai)
        passed = True
    finally:
        cleanup_errors = []
        try:
            for client in clients:
                try:
                    client.close()
                except Exception as error:
                    cleanup_errors.append(str(error))
        finally:
            server.terminate()
            try:
                server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait(timeout=10)
            log.close()
            report = dict(passed=passed and not cleanup_errors, checks=checks, fixture=str(fixture),
                          cleanup='children stopped; files retained', cleanupErrors=cleanup_errors, measurements=measurements)
            (fixture / 'report.json').write_text(json.dumps(report, indent=2))
            print('Evidence: ' + str(fixture), flush=True)
        if cleanup_errors:
            raise RuntimeError('MCP child cleanup failed: ' + '; '.join(cleanup_errors))
    return 0


if __name__ == '__main__':
    sys.exit(main())
