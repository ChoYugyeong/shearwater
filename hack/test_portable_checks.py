#!/usr/bin/env python3
"""표준 라이브러리 회귀 검사. 실제 API/WSL/ctr는 호출하지 않는다.

127.0.0.1의 임시 HTTP 서버, 합성 API/프로세스 관측, 임시 SQLite만 사용한다.
--artifacts-root 아래 mkdtemp로 만든 실행 폴더와 모든 임시 파일을 보존한다.
실행 예: python -B -X utf8 hack/test_portable_checks.py --artifacts-root /absolute/checks
unittest discover로 실행하면 OS 임시 폴더에 같은 증거 폴더를 만든다.
"""
import argparse
from contextlib import contextmanager, redirect_stdout
import copy
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE/filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


soak = load('soak_under_test', 'soak-check.py')
reboot = load('reboot_under_test', 'reboot-snapshot.py')
ARTIFACTS = None


def initialize_artifacts(parent=None):
    global ARTIFACTS
    if ARTIFACTS is None:
        ARTIFACTS = Path(tempfile.mkdtemp(prefix='portable-checks-', dir=parent)).resolve()
        print('Retained test artifacts:', ARTIFACTS, flush=True)


def setUpModule():
    # discovery는 __main__을 실행하지 않는다. unittest의 모듈 준비 단계에서도
    # 같은 초기화를 거쳐야 HTTP 서버 종료 시 기록 경로가 None이 되지 않는다.
    initialize_artifacts()


def pod(name='web-a', app='web', containers=('nginx',), uid=None):
    return {'metadata': {'name': name, 'uid': uid or name+'-uid', 'resourceVersion': '7',
                         'generation': 1, 'labels': {'app': app}},
            'spec': {'containers': [{'name': name} for name in containers]},
            'status': {'podIP': '192.0.2.21', 'conditions': [{'type': 'Ready', 'status': 'True'}],
                       'containerStatuses': [{'name': name, 'containerID': 'containerd://'+name+'-old',
                                              'restartCount': 2, 'ready': True} for name in containers]}}


class FakeAPI:
    def __init__(self, port=80):
        self.pods = [pod(), pod('unrelated', 'other')]
        self.service = {'metadata': {'name': 'web', 'uid': 'service-uid', 'resourceVersion': '5', 'generation': 1},
                        'spec': {'clusterIP': '127.0.0.1', 'selector': {'app': 'web'},
                                 'ports': [{'port': port, 'protocol': 'TCP'}]}}
        self.calls = []

    def get(self, kind, name=None):
        self.calls.append((kind, name))
        if kind == 'services':
            return copy.deepcopy(self.service)
        if kind == 'pods':
            return {'items': copy.deepcopy(self.pods)}
        raise AssertionError('unexpected API resource: ' + kind)


class LocalServer:
    """기능별 무지원/손상 응답도 만드는 로컬 fixture. 외부 연결은 하지 않는다."""
    def __init__(self, body=b'<html><title>Welcome to nginx!</title><body>Hello</body></html>', unsupported=(),
                 corrupt_range=False, change_after=None, fail_after=None, truncate=False):
        self.body, self.unsupported = body, set(unsupported)
        self.corrupt_range, self.change_after, self.fail_after = corrupt_range, change_after, fail_after
        self.records = []
        self.assets = {}
        self.truncate = truncate
        owner = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *args):
                pass

            def do_HEAD(self):
                self.respond(True)

            def do_GET(self):
                self.respond(False)

            def respond(self, head):
                owner.records.append({'method': self.command, 'path': self.path,
                                      'range': self.headers.get('Range'), 'etag': self.headers.get('If-None-Match')})
                count = len(owner.records)
                status, body, headers = 200, owner.body, {}
                if owner.change_after is not None and count > owner.change_after:
                    body = body.replace(b'Hello', b'Other')
                if self.path in owner.assets:
                    body = owner.assets[self.path]
                elif self.path not in ('/', '/index.html'):
                    status, body = (200, owner.body) if 'not-found' in owner.unsupported else (404, b'missing')
                etag = '"' + hashlib.sha256(body).hexdigest() + '"'
                if 'etag' not in owner.unsupported:
                    headers['ETag'] = etag
                if head and 'head' in owner.unsupported:
                    status, body = 405, b'unsupported'
                if self.headers.get('If-None-Match') == etag and 'etag' not in owner.unsupported:
                    status, body = 304, b''
                if self.headers.get('Range') and 'range' not in owner.unsupported:
                    end = int(self.headers['Range'].split('-')[-1])
                    total = len(body)
                    body, status = body[:end+1], 206
                    headers['Content-Range'] = f'bytes 0-{end}/{total + int(owner.corrupt_range)}'
                if owner.fail_after is not None and count > owner.fail_after:
                    status, body = 503, b'unavailable'
                self.send_response(status)
                self.send_header('Content-Type', 'text/html')
                self.send_header('Content-Length', str(len(body) + (10 if owner.truncate else 0)))
                if owner.truncate:
                    self.send_header('Connection', 'close')
                    self.close_connection = True
                for key, value in headers.items():
                    self.send_header(key, value)
                self.end_headers()
                if not head:
                    try:
                        self.wfile.write(body)
                    except (ConnectionResetError, BrokenPipeError, ConnectionAbortedError):
                        pass

        class Server(ThreadingHTTPServer):
            daemon_threads = True

            def handle_error(self, request, client_address):
                # 의도적인 RST는 fixture 서버의 traceback으로 채팅을 오염시키지 않는다.
                owner.records.append({'server_connection_closed': True})

        self.server = Server(('127.0.0.1', 0), Handler)
        self.url = f'http://127.0.0.1:{self.server.server_port}/'
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={'poll_interval': .02}, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)
        path = ARTIFACTS/('http-requests-' + str(self.server.server_port) + '.json')
        # 드물게 OS가 포트를 재사용해도 기존 기록을 덮어쓰지 않는다.
        if path.exists():
            path = ARTIFACTS/(path.stem + '-' + str(len(list(ARTIFACTS.iterdir()))) + '.json')
        with path.open('x', encoding='utf-8') as file:
            json.dump(self.records, file, indent=2)


class PortableChecks(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix=self._testMethodName+'-', dir=ARTIFACTS)).resolve()
        if os.name != 'nt':
            self.root.chmod(0o700)

    def options(self, **kwargs):
        return SimpleNamespace(timeout=1, expected_file=None, skip_check=[], **kwargs)

    def cli(self, script, *args):
        completed = subprocess.run([sys.executable, '-B', '-X', 'utf8', str(HERE/script), *map(str, args)],
                                   capture_output=True, text=True, encoding='utf-8', timeout=20)
        index = len(list(self.root.glob('cli-*.json')))
        (self.root/f'cli-{index}.json').write_text(json.dumps({'args': list(map(str, args)),
            'code': completed.returncode, 'stdout': completed.stdout, 'stderr': completed.stderr},
            ensure_ascii=False, indent=2), encoding='utf-8')
        return completed

    def test_cli_help_and_required_probe_url(self):
        for script, args in [('soak-check.py', []), ('soak-check.py', ['host']),
                             ('soak-check.py', ['probe']), ('reboot-snapshot.py', [])]:
            result = self.cli(script, *args, '--help')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('검사' if script == 'soak-check.py' else '증거', result.stdout)
        result = self.cli('soak-check.py', 'probe', '--output', self.root/'no-url', '--output-root', self.root)
        self.assertEqual(result.returncode, 2)
        self.assertIn('--url', result.stderr)
        self.assertFalse((self.root/'no-url').exists())

    def test_nginx_index_without_assets_all_capabilities(self):
        with LocalServer() as server:
            cases, info = soak.http_cases(server.url, self.options())
            self.assertEqual(info['asset_count'], 0)
            self.assertEqual(set(info['required_checks']), set(soak.CHECKS))
            self.assertEqual({case[0] for case in cases}, {'get', *soak.CHECKS})
            # 짧은 HTML에도 올바른 마지막 바이트/전체 길이를 사용한다.
            range_case = next(c for c in cases if c[0] == 'range')
            self.assertEqual(range_case[5], server.body)
            self.assertEqual(range_case[6]['content-range'], f'bytes 0-{len(server.body)-1}/{len(server.body)}')

    def test_each_unsupported_capability_fails_unless_explicit(self):
        for missing in soak.CHECKS:
            with self.subTest(missing=missing), LocalServer(unsupported=[missing]) as server:
                with self.assertRaises(soak.CheckError):
                    soak.http_cases(server.url, self.options())
                args = self.options()
                args.skip_check = [missing]
                cases, info = soak.http_cases(server.url, args)
                self.assertEqual(info['skipped_checks'], [missing])
                self.assertNotIn(missing, [c[0] for c in cases])

    def test_corrupt_range_header_fails(self):
        with LocalServer(corrupt_range=True) as server:
            with self.assertRaisesRegex(soak.CheckError, 'content-range'):
                soak.http_cases(server.url, self.options())

    def test_truncated_http_is_not_accepted_as_baseline(self):
        with LocalServer(truncate=True) as server:
            with self.assertRaisesRegex(soak.CheckError, 'Content-Length'):
                soak.http_cases(server.url, self.options())
            result = self.cli('soak-check.py', 'probe', '--url', server.url, '--seconds', 1,
                              '--output-root', self.root, '--output', self.root/'evidence')
            self.assertEqual(result.returncode, 1)
            report = json.loads((self.root/'evidence'/'result.json').read_text(encoding='utf-8'))
            self.assertIsNone(report['sha256'])

    def test_head_then_get_reuses_connection(self):
        with LocalServer() as server:
            connection = soak.connect(server.url, 1)
            try:
                head = soak.request(connection, 'HEAD', '/')
                body = soak.request(connection, 'GET', '/')
                self.assertEqual(head[2], b'')
                self.assertEqual(body[2], server.body)
            finally:
                connection.close()

    def test_assets_relative_names_and_no_external_fetch(self):
        html = b'<html><base href="/static/"><script src="bundle-123.js"></script><link rel="stylesheet" href="theme.css"><script src="https://example.invalid/a.js"></script>Hello</html>'
        with LocalServer(body=html) as server:
            server.assets = {'/static/bundle-123.js': b'let test=1;', '/static/theme.css': b'body{margin:0}'}
            cases, info = soak.http_cases(server.url, self.options())
            self.assertEqual(info['asset_count'], 2)
            self.assertEqual(len(info['excluded_assets']), 1)
            self.assertEqual({c[2] for c in cases if c[0] == 'asset'}, set(server.assets))

    def test_expected_file_mismatch_fails_preflight(self):
        expected = self.root/'index.html'
        expected.write_bytes(b'wrong')
        args = self.options()
        args.expected_file = expected
        with LocalServer() as server, self.assertRaisesRegex(soak.CheckError, 'expected-file'):
            soak.http_cases(server.url, args)

    def test_probe_cli_records_real_elapsed_and_refuses_overwrite(self):
        with LocalServer() as server:
            expected = self.root/'index.html'
            expected.write_bytes(server.body)
            args = ['probe', '--url', server.url, '--seconds', '2', '--output-root', self.root,
                    '--output', self.root/'evidence', '--expected-file', expected]
            result = self.cli('soak-check.py', *args)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            report_path = self.root/'evidence'/'result.json'
            report = json.loads(report_path.read_text(encoding='utf-8'))
            self.assertTrue(report['passed'])
            self.assertGreaterEqual(report['duration_s'], 2)
            self.assertGreaterEqual(report['requests'], 2)
            self.assertTrue(all(r['ms'] > 0 for r in report['records']))
            self.assertEqual(report['clock_source'], soak.CLOCK_NAME)
            original = report_path.read_bytes()
            self.assertEqual(self.cli('soak-check.py', *args).returncode, 2)
            self.assertEqual(report_path.read_bytes(), original)

    def test_probe_changed_body_fails_and_retains_evidence(self):
        with LocalServer(change_after=1) as server:
            result = self.cli('soak-check.py', 'probe', '--url', server.url, '--seconds', 2,
                              '--output-root', self.root, '--output', self.root/'evidence')
            self.assertEqual(result.returncode, 1, result.stderr)
            report = json.loads((self.root/'evidence'/'result.json').read_text(encoding='utf-8'))
            self.assertEqual(report['errors'], 1)
            self.assertFalse(report['passed'])

    def test_probe_http_error_fails_without_body_leak(self):
        with LocalServer(fail_after=0) as server:
            result = self.cli('soak-check.py', 'probe', '--url', server.url, '--seconds', 1,
                              '--output-root', self.root, '--output', self.root/'evidence')
            self.assertEqual(result.returncode, 1)
            report = json.loads((self.root/'evidence'/'result.json').read_text(encoding='utf-8'))
            self.assertEqual(report['errors'], 1)
            self.assertEqual(report['records'][0]['code'], 503)
            self.assertNotIn('unavailable', result.stdout)

    def test_output_root_ownership_permissions_and_exclusive_files(self):
        evidence = soak.evidence_directory(self.root/'evidence', self.root)
        soak.write_json(evidence/'once.json', {'ok': True})
        with self.assertRaises(FileExistsError):
            soak.write_json(evidence/'once.json', {'ok': False})
        with self.assertRaises(soak.CheckError):
            soak.evidence_directory(self.root.parent/'escape', self.root)
        with self.assertRaises(soak.CheckError):
            soak.evidence_directory(evidence, self.root)
        if os.name == 'nt':
            soak.windows_directory(evidence, private=True)
            permissive = self.root/'ordinary'
            permissive.mkdir()
            with self.assertRaises(soak.CheckError):
                soak.evidence_directory(permissive, self.root, create=False)
        else:
            self.assertEqual(evidence.stat().st_mode & 0o777, 0o700)
            self.assertEqual((evidence/'once.json').stat().st_mode & 0o777, 0o600)
            permissive = self.root/'ordinary'
            permissive.mkdir(mode=0o755)
            with self.assertRaises(soak.CheckError):
                soak.evidence_directory(permissive, self.root, create=False)

    def test_output_symlink_refused_when_platform_allows(self):
        target = self.root/'target'
        target.mkdir()
        link = self.root/'link'
        try:
            link.symlink_to(target, target_is_directory=True)
        except OSError:
            # 권한 없는 Windows는 symlink를 만들 수 없다. 이 분기는 결과에 명시된다.
            self.skipTest('symlink creation unavailable; no privilege escalation')
        with self.assertRaises(soak.CheckError):
            soak.evidence_directory(link, self.root)

    def test_token_urls_and_secret_stderr_are_not_printed(self):
        secret = 'fake-sensitive-token-ONLY-TEST'
        for url in [f'http://user:{secret}@localhost/', f'http://localhost/?token={secret}', 'http://localhost/#fragment']:
            with self.assertRaises(soak.CheckError) as raised:
                soak.http_target(url)
            self.assertNotIn(secret, str(raised.exception))
        result = subprocess.CompletedProcess([], 9, b'', secret.encode())
        with patch.object(soak.subprocess, 'run', return_value=result), self.assertRaises(soak.CheckError) as raised:
            soak.command_output(['kubectl'], 'kubectl get pods')
        self.assertNotIn(secret, str(raised.exception))
        self.assertIn('exit=9', str(raised.exception))
        self.assertNotIn(secret, json.dumps(soak.error_record(RuntimeError(secret))))

    def test_api_uses_explicit_kubeconfig_namespace_and_read_only_get(self):
        args = SimpleNamespace(kubectl='custom-kubectl', kubeconfig=self.root/'custom config', namespace='fixture')
        with patch.object(soak, 'command_output', return_value='{"items": []}') as run:
            self.assertEqual(soak.API(args, self.root).get('pods'), {'items': []})
        command = run.call_args.args[0]
        self.assertIn('--kubeconfig='+str(args.kubeconfig), command)
        self.assertIn('--namespace=fixture', command)
        self.assertEqual(command[-4:], ['get', 'pods', '-o', 'json'])

    def test_fixture_selects_all_pods_and_ignores_unrelated_revision(self):
        api = FakeAPI()
        api.pods.append(pod('web-b'))
        before = soak.fixture(api, 'web')
        api.pods[1]['metadata']['resourceVersion'] = '999'
        after = soak.fixture(api, 'web')
        self.assertEqual(set(before['pods']), {'web-a', 'web-b'})
        self.assertTrue(soak.compare_fixture(before, after))
        self.assertEqual(soak.unrelated_changes(before, after), ['unrelated'])
        api.pods[2]['status']['containerStatuses'][0]['restartCount'] += 1
        self.assertFalse(soak.compare_fixture(before, soak.fixture(api, 'web')))

    def test_selected_pod_uid_ip_revision_and_service_change_fail(self):
        before = soak.fixture(FakeAPI(), 'web')
        paths = [('pods', 'web-a', 'metadata', 'uid'), ('pods', 'web-a', 'podIP'),
                 ('pods', 'web-a', 'metadata', 'resourceVersion'), ('service', 'spec', 'clusterIP')]
        for path in paths:
            after = copy.deepcopy(before)
            value = after
            for key in path[:-1]:
                value = value[key]
            value[path[-1]] = 'changed'
            self.assertFalse(soak.compare_fixture(before, after), path)

    def test_no_matching_unready_or_empty_containers_fail(self):
        for variant in ('unselected', 'unready', 'empty', 'missing-ip'):
            api = FakeAPI()
            if variant == 'unselected':
                api.pods[0]['metadata']['labels'] = {'app': 'other'}
            elif variant == 'unready':
                api.pods[0]['status']['conditions'][0]['status'] = 'False'
            elif variant == 'empty':
                api.pods[0]['status']['containerStatuses'] = []
            else:
                api.pods[0]['status']['podIP'] = ''
            with self.subTest(variant=variant), self.assertRaises(soak.CheckError):
                soak.fixture(api, 'web')

    def test_service_port_selection_and_readiness_cli(self):
        service = FakeAPI().service
        service['spec']['ports'].append({'port': 8080, 'protocol': 'TCP'})
        with self.assertRaises(soak.CheckError):
            soak.service_url(service, None, '/')
        self.assertEqual(soak.service_url(service, 8080, '/index.html'), 'http://127.0.0.1:8080/index.html')
        args = soak.build_parser().parse_args(['host', '--output', str(self.root/'ready'),
            '--service', 'ready-green', '--namespace', 'probe-demo', '--service-port', '8080', '--rates', '5', '10', '5'])
        self.assertEqual((args.service, args.namespace, args.rates), ('ready-green', 'probe-demo', [5, 10, 5]))

    def test_raw_and_windows_clock_dispatch_and_deadline_wait(self):
        fake_raw = SimpleNamespace(CLOCK_MONOTONIC_RAW=4, clock_gettime=lambda c: 12.75)
        fake_win = SimpleNamespace(perf_counter=lambda: 18.125)
        with patch.object(soak, 'time', fake_raw):
            self.assertEqual(soak.elapsed_clock(), 12.75)
        with patch.object(soak, 'time', fake_win):
            self.assertEqual(soak.elapsed_clock(), 18.125)
        with patch.object(soak, 'elapsed_clock', side_effect=[0, 0, .25, .9, 1]), patch.object(soak.time, 'sleep') as sleep:
            soak.pause(1)
            self.assertEqual(sleep.call_count, 3)
            self.assertAlmostEqual(sleep.call_args.args[0], .1)
        event = SimpleNamespace(wait=lambda remaining: True)
        with patch.object(soak, 'elapsed_clock', side_effect=[0, 0]):
            soak.pause(100, event)

    def test_proc_parser_parentheses_and_missing_io_are_not_hidden(self):
        process = self.root/'proc'
        process.mkdir()
        fields = ['0'] * 22
        fields[11], fields[12], fields[19] = '15', '17', '99'
        (process/'stat').write_text('123 (name with ) spaces) ' + ' '.join(fields))
        (process/'status').write_text('VmRSS:\t42 kB\nThreads:\t3\n')
        (process/'io').write_text('write_bytes: 512\n')
        (process/'fd').mkdir()
        with patch.object(soak, 'Path', return_value=process):
            result = soak.proc_sample(123)
        self.assertEqual((result['start_tick'], result['cpu_ticks'], result['rss_kib']), ('99', 32, 42))
        (process/'io').write_text('read_bytes: 0\n')
        with patch.object(soak, 'Path', return_value=process), self.assertRaises(KeyError):
            soak.proc_sample(123)

    @contextmanager
    def host_environment(self, server, ending_failure=False):
        args = soak.build_parser().parse_args(['host', '--output', str(self.root/'host')])
        args.output = soak.evidence_directory(args.output, self.root)
        args.run_dir = self.root/'run'
        args.run_dir.mkdir()
        (args.run_dir/'server.json').write_text('{"pid": 1234, "startTick": "77"}')
        args.data_dir = self.root
        args.log_dir = self.root/'log'
        args.log_dir.mkdir()
        (args.log_dir/'server.log').write_bytes(b'log')
        (args.run_dir/'server.json').write_text(json.dumps({'pid': 1234, 'startTick': '77',
                                                          'options': {'LogDir': str(args.log_dir)}}))
        logs = args.log_dir/'pods'/'default_web-a_web-a-uid'/'nginx'
        logs.mkdir(parents=True)
        (logs/'0.log').write_bytes(b'access\n')
        args.seconds, args.cooldown, args.workers, args.rates = 1.2, 0, 2, [20, 20, 20]
        args.timeout = 1
        api = FakeAPI(server.server.server_port)
        sample = {'start_tick': '77', 'rss_kib': 64, 'fd': 4, 'threads': 2, 'cpu_ticks': 5, 'write_bytes': 0}
        calls = [0]

        def process(pid):
            calls[0] += 1
            if ending_failure and calls[0] >= 8:
                raise PermissionError('test observation denied')
            return dict(sample)

        with patch.object(soak, 'API', return_value=api), \
             patch.object(soak, 'container_tasks', return_value={'web-a/nginx': {'pid': 5678, 'status': 'RUNNING'}}), \
             patch.object(soak, 'proc_sample', side_effect=process), \
             patch.object(soak.shutil, 'disk_usage', return_value=SimpleNamespace(free=2*1024**3)), \
             patch.object(soak.os, 'sysconf', return_value=100, create=True):
            yield args, api

    def test_host_end_to_end_local_http_and_synthetic_metrics(self):
        with LocalServer() as server, self.host_environment(server) as (args, api):
            args.log_dir = None  # 서버 상태의 경로도 실제로 따라가는지 확인한다.
            report = soak.host(args)
        (self.root/'host-result.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
        self.assertTrue(report['passed'], report)
        self.assertTrue(report['all_cases_exercised'])
        self.assertTrue(report['workers_stopped'])
        self.assertGreater(report['totals']['intentional_cancellations'], 0)
        self.assertTrue((args.output/'metrics.jsonl').read_text())
        self.assertEqual(api.calls[0], ('services', 'web'))

    def test_host_final_measurement_failure_is_a_failed_report(self):
        with LocalServer() as server, self.host_environment(server, ending_failure=True) as (args, _):
            report = soak.host(args)
        self.assertFalse(report['passed'])
        self.assertEqual(report['failure']['type'], 'PermissionError')
        self.assertTrue(report['errors'])
        self.assertTrue(report['workers_stopped'])

    def test_host_worker_http_failures_are_counted(self):
        with LocalServer(fail_after=7) as server, self.host_environment(server) as (args, _):
            report = soak.host(args)
        self.assertFalse(report['passed'])
        self.assertGreater(report['totals']['errors'], 0)
        self.assertTrue(report['errors'])
        self.assertIn('503', report['status_counts'])

    def snapshots(self):
        api = FakeAPI()
        api.pods[0] = pod(containers=('nginx', 'sidecar'))
        selected = soak.fixture(api, 'web')
        selected['deployment'] = {'metadata': {'name': 'web', 'uid': 'deployment-uid', 'generation': 1},
                                  'replicas': 1, 'selector': {'matchLabels': {'app': 'web'}}}
        before = {'fixture': selected, 'auth_sha256': {'admin.token': 'hash', 'ca.crt': 'ca', 'kubeconfig': 'config'},
                  'sqlite': {'quick_check': [['ok']], 'foreign_keys': []}}
        after = copy.deepcopy(before)
        containers = after['fixture']['pods']['web-a']['containers']
        for value in containers.values():
            value['restartCount'] += 1
            value['containerID'] += '-new'
        after['fixture']['pods']['web-a']['containers'] = dict(reversed(list(containers.items())))
        return before, after

    def test_reboot_restores_names_not_container_array_positions(self):
        before, after = self.snapshots()
        checks = reboot.compare_restored(before, after)
        self.assertIn('pod/web-a', checks)
        after['fixture']['unrelated_pods']['new-background'] = {'uid': 'new', 'resourceVersion': '99'}
        self.assertEqual(reboot.compare_restored(before, after), checks)

    def test_reboot_rejects_bad_identity_ip_restart_auth_and_db(self):
        for variant in ('uid', 'ip', 'restart', 'container', 'empty', 'service-ip', 'auth', 'sqlite'):
            before, after = self.snapshots()
            p = after['fixture']['pods']['web-a']
            if variant == 'uid':
                p['metadata']['uid'] = 'replacement'
            elif variant == 'ip':
                p['podIP'] = '192.0.2.222'
            elif variant == 'restart':
                p['containers']['nginx']['restartCount'] = 2
            elif variant == 'container':
                p['containers']['nginx']['containerID'] = 'containerd://nginx-old'
            elif variant == 'empty':
                p['containers'] = {}
            elif variant == 'service-ip':
                after['fixture']['service']['spec']['clusterIP'] = '127.0.0.2'
            elif variant == 'auth':
                after['auth_sha256']['admin.token'] = 'other-hash'
            else:
                after['sqlite']['foreign_keys'] = [['bad', 1]]
            with self.subTest(variant=variant), self.assertRaises(reboot.common.CheckError):
                reboot.compare_restored(before, after)

    def test_sqlite_read_only_backup_includes_wal(self):
        database, backup = self.root/'state.db', self.root/'state-before.db'
        connection = sqlite3.connect(database)
        try:
            connection.execute('PRAGMA journal_mode=WAL')
            connection.execute('CREATE TABLE observations (value TEXT)')
            connection.execute("INSERT INTO observations VALUES ('persisted-in-wal')")
            connection.commit()
            result = reboot.database_checks(database, backup)
            self.assertEqual(result, {'quick_check': [['ok']], 'foreign_keys': []})
            with sqlite3.connect(backup) as copied:
                self.assertEqual(copied.execute('SELECT value FROM observations').fetchone(), ('persisted-in-wal',))
            with self.assertRaises(FileExistsError):
                reboot.database_checks(database, backup)
        finally:
            connection.close()

    def test_reboot_collection_uses_custom_paths_and_does_not_serialize_secrets(self):
        args = self.phase_args()
        args.ctr = 'custom-ctr'
        args.containerd_socket = self.root/'containerd.sock'
        secret = 'fake-token-never-print-abcdef'
        for filename in ('admin.token', 'ca.crt', 'kubeconfig'):
            (self.root/filename).write_text(secret, encoding='utf-8')
        (args.run_dir/'server.json').write_text('{"pid": 1234, "startTick": "77"}')
        with sqlite3.connect(self.root/'state.db') as database:
            database.execute('CREATE TABLE example (value TEXT)')
        api = FakeAPI()
        api.pods[0]['metadata']['annotations'] = {'private': secret}
        api.pods[0]['spec']['containers'][0]['env'] = [{'name': 'SECRET', 'value': secret}]
        deployment = {'metadata': {'name': 'web', 'uid': 'deploy-uid', 'generation': 1},
                      'spec': {'replicas': 1, 'selector': {'matchLabels': {'app': 'web'}},
                               'template': {'metadata': {'labels': {'app': 'web'}}}}}

        def get(kind, name=None):
            if kind == 'deployments':
                return deployment
            if kind == 'endpointslices':
                return {'items': []}
            return api.get(kind, name)

        with patch.object(reboot.common, 'API', return_value=SimpleNamespace(get=get)), \
             patch.object(reboot.common, 'proc_sample', return_value={'start_tick': '77'}), \
             patch.object(reboot.common, 'command_output', return_value='TASK PID STATUS\nnginx-old 5678 RUNNING\n') as command:
            result = reboot.collect(args)
        self.assertNotIn(secret, json.dumps(result))
        self.assertEqual(result['auth_sha256']['kubeconfig'], hashlib.sha256(secret.encode()).hexdigest())
        self.assertEqual(result['tasks']['web-a/nginx']['pid'], 5678)
        self.assertEqual(command.call_args.args[0][:3], ['custom-ctr', '--address', str(args.containerd_socket)])
        self.assertTrue((args.evidence/'state-before.db').exists())

    def test_missing_container_task_and_log_are_failures(self):
        args = SimpleNamespace(ctr='ctr', containerd_socket=self.root/'socket', containerd_namespace='shearwater',
                               log_dir=self.root, namespace='default')
        selected = soak.fixture(FakeAPI(), 'web')
        with patch.object(soak, 'command_output', return_value='TASK PID STATUS\n'), self.assertRaises(soak.CheckError):
            soak.container_tasks(args, selected)
        with self.assertRaises(soak.CheckError):
            soak.log_bytes(args, selected)

    def phase_args(self):
        args = reboot.build_parser().parse_args(['before', str(self.root/'evidence')])
        args.evidence = soak.evidence_directory(args.evidence, self.root)
        args.run_dir = self.root/'run'
        args.run_dir.mkdir()
        if os.name != 'nt':
            args.run_dir.chmod(0o700)
        args.config_dir, args.data_dir, args.kubeconfig = self.root, self.root, self.root/'kubeconfig'
        return args

    def phase(self, args, observation, collection):
        with patch.object(reboot, 'boot_record', return_value=observation), \
             patch.object(reboot, 'collect', return_value=collection) as collect, redirect_stdout(io.StringIO()):
            code = reboot.run_phase(args)
        return code, collect

    def test_reboot_phase_chain_without_real_reboot_or_api(self):
        args = self.phase_args()
        before, after = self.snapshots()
        initial = {'boot_id': 'boot-A', 'init_start_tick': '10', 'marker_present': False, 'server_state_present': True}
        booted = {'boot_id': 'boot-B', 'init_start_tick': '20', 'marker_present': False, 'server_state_present': False}
        self.assertEqual(self.phase(args, initial, before)[0], 0)
        args.phase = 'booted'
        code, collect = self.phase(args, booted, None)
        self.assertEqual(code, 0)
        collect.assert_not_called()
        args.phase = 'after'
        self.assertEqual(self.phase(args, {**booted, 'server_state_present': True}, after)[0], 0)
        self.assertTrue(json.loads((args.evidence/'after.json').read_text(encoding='utf-8'))['passed'])
        self.assertEqual(len(list(args.run_dir.glob('*.marker'))), 1)  # 표식도 삭제하지 않는다.

    def test_reboot_booted_failure_written_and_after_refused(self):
        args = self.phase_args()
        before, after = self.snapshots()
        observation = {'boot_id': 'same', 'init_start_tick': '10', 'marker_present': False, 'server_state_present': True}
        self.assertEqual(self.phase(args, observation, before)[0], 0)
        args.phase = 'booted'
        code, collect = self.phase(args, {**observation, 'marker_present': True}, None)
        self.assertEqual(code, 1)
        collect.assert_not_called()
        self.assertFalse(json.loads((args.evidence/'booted.json').read_text(encoding='utf-8'))['passed'])
        args.phase = 'after'
        self.assertEqual(self.phase(args, {**observation, 'boot_id': 'new'}, after)[0], 1)

    def test_booted_allows_missing_run_directory_without_creating_it(self):
        args = self.phase_args()
        args.run_dir = self.root/'absent-after-boot'
        before = {'passed': True, 'target': reboot.target_record(args),
                  'boot_id': 'old-boot', 'init_start_tick': '10'}
        soak.write_json(args.evidence/'before.json', before)
        args.phase = 'booted'
        observation = {'boot_id': 'new-boot', 'init_start_tick': '20',
                       'marker_present': False, 'server_state_present': False}
        code, collect = self.phase(args, observation, None)
        self.assertEqual(code, 0)
        collect.assert_not_called()
        self.assertFalse(args.run_dir.exists())

    def test_reboot_no_boot_change_and_target_mismatch_rejected(self):
        args = self.phase_args()
        before, _ = self.snapshots()
        observation = {'boot_id': 'same', 'init_start_tick': '10', 'marker_present': False, 'server_state_present': True}
        self.assertEqual(self.phase(args, observation, before)[0], 0)
        args.phase = 'booted'
        self.assertEqual(self.phase(args, {**observation, 'server_state_present': False}, None)[0], 1)
        args.phase = 'after'
        args.service = 'different'
        code, collect = self.phase(args, {**observation, 'boot_id': 'new'}, None)
        self.assertEqual(code, 1)
        collect.assert_not_called()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts-root', type=Path, help='기존 테스트 기록 부모; 생략하면 OS 임시 폴더')
    args = parser.parse_args()
    initialize_artifacts(args.artifacts_root)
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(PortableChecks)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    (ARTIFACTS/'summary.json').write_text(json.dumps({
        'tests': result.testsRun, 'failures': len(result.failures), 'errors': len(result.errors),
        'skipped': [(str(test), reason) for test, reason in result.skipped],
        'successful': result.wasSuccessful()}, indent=2), encoding='utf-8')
    sys.exit(0 if result.wasSuccessful() else 1)
