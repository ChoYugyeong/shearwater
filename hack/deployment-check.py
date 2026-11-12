#!/usr/bin/env python3
"""실제 앱이 실행 중인 Shearwater에서 별도 네임스페이스로 배포를 검사한다.

기존 m2~m6 검사는 빈 containerd 네임스페이스가 필요하다. 이 검사는 기존
서버를 이용하며 자신이 만든 객체만 정리한다. SQLite·앱 파일은 지우지 않는다.
--restart-server를 지정하면 공유 sw 서버도 두 번 재시작한다. Pod는 계속
실행되지만 API와 Service TCP 프록시는 재시작 동안 잠시 접속할 수 없다.
"""
import argparse
import concurrent.futures
import hashlib
import http.client
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import signal
import statistics
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sw', required=True, type=Path)
    parser.add_argument('--kubectl', required=True, type=Path)
    parser.add_argument('--site-dir', required=True, type=Path)
    parser.add_argument('--nginx-config', type=Path)
    parser.add_argument('--restart-server', action='store_true')
    args = parser.parse_args()
    assert os.geteuid() == 0, 'WSL root에서 실행해 줘'
    assert args.site_dir.resolve().is_relative_to('/opt/shearwater/deployments')
    assert (args.site_dir / 'index.html').is_file()
    assert args.sw.is_absolute() and args.sw.is_file()
    assert args.kubectl.is_absolute() and args.kubectl.is_file()
    if args.nginx_config:
        assert args.nginx_config.is_file() and args.nginx_config.resolve().is_relative_to('/opt/shearwater/deployments')
    root = Path('/var/lib/shearwater/check')
    root.mkdir(exist_ok=True)
    assert root.resolve() == root, '검증 폴더의 심볼릭 링크는 허용하지 않음'
    evidence = Path(tempfile.mkdtemp(prefix='deployment.', dir=root))
    # tempfile에는 '_'도 들어갈 수 있지만 Namespace 이름에는 쓸 수 없다.
    namespace = 'deployqa-' + hashlib.sha256(evidence.name.encode()).hexdigest()[:12]
    results = []
    owned = False
    server_off = False
    kube = [str(args.kubectl), '--kubeconfig=/etc/shearwater/kubeconfig',
            '--request-timeout=15s', '--namespace=' + namespace,
            '--cache-dir=' + str(evidence / 'discovery')]
    print('Evidence: ' + str(evidence), flush=True)

    def save(name, value):
        (evidence / name).write_text(json.dumps(value, ensure_ascii=False, indent=2))

    def command(argv, body=None, ok=True, timeout=100):
        process = subprocess.run(argv, input=body, text=True, capture_output=True, timeout=timeout)
        with (evidence / 'commands.jsonl').open('a') as output:
            output.write(json.dumps({'argv': argv, 'code': process.returncode,
                                     'out': process.stdout, 'err': process.stderr}, ensure_ascii=False) + '\n')
        if ok and process.returncode:
            raise RuntimeError(f'{argv}: {process.stderr or process.stdout}')
        return process

    def k(*argv, **kwargs):
        return command(kube + list(argv), **kwargs)

    def get(kind, name=None, **kwargs):
        return json.loads(k('get', kind, *([name] if name else []), '-o', 'json', **kwargs).stdout)

    def note(name, detail=None):
        results.append({'case': name, 'pass': True, 'detail': detail})
        save('results.json', results)
        print('PASS ' + name, flush=True)

    def wait(fn, seconds=60):
        deadline = time.monotonic() + seconds
        while True:
            value = fn()
            if value:
                return value
            if time.monotonic() >= deadline:
                raise TimeoutError(fn.__name__)
            time.sleep(0.3)

    def request(ip, path='/', method='GET', headers=None):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        req = urllib.request.Request('http://' + ip + path, method=method, headers=headers or {})
        try:
            response = opener.open(req, timeout=6)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.code, dict(response.headers), response.read()

    expected = (args.site_dir / 'index.html').read_bytes()

    def serves(ip, content=expected):
        try:
            code, _, data = request(ip)
            return code == 200 and data == content
        except (OSError, http.client.HTTPException):
            return False

    def ready(pod):
        return not pod['metadata'].get('deletionTimestamp') and any(
            c['type'] == 'Ready' and c['status'] == 'True' for c in pod.get('status', {}).get('conditions', []))

    def app_pods():
        return [p for p in get('pods')['items'] if p['metadata'].get('labels', {}).get('app') == 'dasibom']

    def steady(count):
        pods = app_pods()
        status = get('deployment', 'web').get('status', {})
        return len(pods) == count and all(ready(p) for p in pods) and status.get('readyReplicas', 0) == count

    def endpoint_count():
        slices = get('endpointslices')['items']
        return sum(len(s.get('endpoints', [])) for s in slices)

    def patch(kind, name, body):
        k('patch', kind, name, '--type=merge', '-p', json.dumps(body))

    def identity(pod):
        return [pod['metadata']['uid'], pod['status'].get('podIP'),
                [(c['containerID'], c['restartCount']) for c in pod['status']['containerStatuses']]]

    def all_identities():
        # 공통 k()를 써서 kubectl 캐시도 증거 폴더 안에만 기록한다.
        pods = json.loads(k('get', 'pods', '-A', '-o', 'json').stdout)['items']
        return {p['metadata']['uid']: identity(p) for p in pods if ready(p)}

    def restart_up():
        nonlocal server_off
        command([str(args.sw), 'up'])
        server_off = False
        wait(lambda: serves(service_ip))

    try:
        endpoint = k('config', 'view', '--minify', '-o', 'jsonpath={.clusters[0].cluster.server}').stdout
        assert endpoint == 'https://127.0.0.1:6443', endpoint
        assert k('get', '--raw', '/readyz').stdout == 'ok'
        main_service = json.loads(command(kube + ['get', 'service', 'dasibom', '-n', 'local-apps', '-o', 'json']).stdout)
        main_ip = main_service['spec']['clusterIP']
        main_code, _, main_index = request(main_ip)
        assert main_code == 200 and b'</html>' in main_index, '실제 앱이 정상이어야 검사를 시작한다'
        original = all_identities()
        note('실제 앱의 초기 상태 및 HTTP 본문', original)
        k('create', 'namespace', namespace)
        owned = True
        ns_uid = get('namespace', namespace)['metadata']['uid']
        save('ownership.json', {'namespace': namespace, 'uid': ns_uid})
        deployment = {
            'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': 'web'},
            'spec': {'replicas': 2, 'selector': {'matchLabels': {'app': 'dasibom'}},
                     'strategy': {'type': 'RollingUpdate', 'rollingUpdate': {'maxSurge': 1, 'maxUnavailable': 0}},
                     'template': {'metadata': {'labels': {'app': 'dasibom'}}, 'spec': {
                         'terminationGracePeriodSeconds': 2,
                         'containers': [{'name': 'web', 'image': 'nginx:1.27', 'imagePullPolicy': 'IfNotPresent',
                                         'ports': [{'name': 'http', 'containerPort': 80}],
                                         'volumeMounts': [{'name': 'site', 'mountPath': '/usr/share/nginx/html', 'readOnly': True}]}],
                         'volumes': [{'name': 'site', 'hostPath': {'path': str(args.site_dir), 'type': 'Directory'}}]}}}}
        service = {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': 'web'},
                   'spec': {'selector': {'app': 'dasibom'}, 'ports': [{'name': 'http', 'port': 80, 'targetPort': 'http'}]}}
        if args.nginx_config:
            pod_spec = deployment['spec']['template']['spec']
            pod_spec['volumes'].append({'name': 'nginx-config', 'hostPath': {'path': str(args.nginx_config), 'type': 'File'}})
            pod_spec['containers'][0]['volumeMounts'].append({'name': 'nginx-config', 'mountPath': '/etc/nginx/conf.d/default.conf', 'readOnly': True})
        for obj in (service, deployment):
            k('apply', '-f', '-', body=json.dumps(obj))
        service_ip = get('service', 'web')['spec']['clusterIP']
        wait(lambda: steady(2))
        wait(lambda: endpoint_count() == 2 and serves(service_ip))
        note('별도 네임스페이스에서 2개 복제본과 HTTP 준비', service_ip)
        endpoints = [ep for s in get('endpointslices')['items'] for ep in s.get('endpoints', [])]
        assert all(ep['targetRef']['namespace'] == namespace and ep['targetRef']['uid'] not in original for ep in endpoints)
        note('라벨이 같은 실제 앱과 EndpointSlice 격리')
        assert 'unchanged' in k('apply', '-f', '-', body=json.dumps(deployment)).stdout
        note('같은 매니페스트 재적용은 unchanged')

        class Assets(HTMLParser):
            paths = []
            def handle_starttag(self, tag, attrs):
                attrs = dict(attrs)
                path = attrs.get('src') if tag == 'script' else attrs.get('href') if tag == 'link' else None
                if path and not path.startswith(('http:', 'https:', '//')):
                    self.paths.append('/' + path.lstrip('/'))
        assets = Assets()
        assets.feed(expected.decode())
        app = next(p for p in assets.paths if p.endswith('/app.js'))
        for path in ['/index.html'] + assets.paths + [app.removesuffix('app.js') + 'store.js']:
            code, headers, body = request(service_ip, path)
            assert code == 200 and body == (args.site_dir / path.lstrip('/')).read_bytes()
            assert 'text/html' not in headers.get('Content-Type', '') or path.endswith('.html')
        note('HTML·CSS·ES 모듈 바이트 및 MIME')
        code, headers, body = request(service_ip, method='HEAD')
        assert code == 200 and body == b'' and int(headers['Content-Length']) == len(expected)
        note('HEAD 응답은 길이를 보존하고 본문이 없음')
        code, _, body = request(service_ip, headers={'Range': 'bytes=0-63'})
        assert code == 206 and body == expected[:64]
        note('Range 부분 다운로드')
        if args.nginx_config:
            assert headers.get('Cache-Control') == 'no-cache' and 'ETag' not in headers
            code, _, body = request(service_ip, headers={'If-Modified-Since': headers['Last-Modified'], 'If-None-Match': '"old-version"'})
            assert code == 200 and body == expected
            note('HTML은 이전 캐시 조건에서도 새 내용을 확인')
        else:
            code, _, body = request(service_ip, headers={'If-None-Match': headers['ETag']})
            assert code == 304 and not body
            note('ETag 조건부 요청 304')
        code, asset_headers, body = request(service_ip, app)
        code, _, body = request(service_ip, app, headers={'If-None-Match': asset_headers['ETag']})
        assert code == 304 and not body
        note('버전이 있는 모듈의 조건부 요청 304')
        assert request(service_ip, '/qa-missing-file')[0] == 404
        note('없는 파일은 404')
        def sample(_):
            begin = time.monotonic()
            assert serves(service_ip)
            return (time.monotonic() - begin) * 1000
        with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
            timings = list(pool.map(sample, range(60)))
        note('6개 동시 연결로 60회 본문 일치', {'median_ms': statistics.median(timings), 'max_ms': max(timings)})

        for count in (3, 1, 0, 2):
            k('scale', 'deployment/web', '--replicas=' + str(count))
            wait(lambda: steady(count) and endpoint_count() == count)
            assert serves(service_ip) == (count > 0)
            note('복제본 수 변경 ' + str(count))
        pod = app_pods()[0]
        old_uid = pod['metadata']['uid']
        k('delete', 'pod', pod['metadata']['name'], '--wait=false')
        wait(lambda: steady(2) and all(p['metadata']['uid'] != old_uid for p in app_pods()))
        wait(lambda: serves(service_ip))
        note('Pod 삭제 후 새 UID로 대체')

        # 상태에서 얻은 정확한 태스크 하나만 신호를 보낸다. 다른 containerd
        # 네임스페이스나 이름 전체에 대한 kill/delete는 절대 실행하지 않는다.
        pod = app_pods()[0]
        old = identity(pod)
        task = pod['status']['containerStatuses'][0]['containerID'].removeprefix('containerd://')
        assert task.startswith(pod['metadata']['uid'] + '-')
        command(['ctr', '--namespace', 'shearwater', 'tasks', 'kill', '--signal', 'SIGKILL', task])
        def restarted():
            now = get('pod', pod['metadata']['name'])
            return ready(now) and now['status']['containerStatuses'][0]['restartCount'] > old[2][0][1]
        wait(restarted)
        assert get('pod', pod['metadata']['name'])['status']['podIP'] == old[1]
        assert 'nginx' in k('logs', pod['metadata']['name'], '--previous').stdout
        wait(lambda: serves(service_ip))
        note('컨테이너 강제 종료 후 같은 Pod IP로 재시작 및 이전 로그')

        patch('service', 'web', {'spec': {'selector': {'app': 'missing'}}})
        wait(lambda: endpoint_count() == 0)
        assert not serves(service_ip)
        assert serves(main_ip, main_index)
        note('빈 selector 결과는 연결 거절, 실제 앱 정상')
        patch('service', 'web', {'spec': {'selector': {'app': 'dasibom'}}})
        wait(lambda: endpoint_count() == 2 and serves(service_ip))
        note('selector 복원 후 동일 ClusterIP 복구')

        # 없는 실행 파일을 지정해 새 리비전을 확실하게 실패시킨다. 이미지
        # 다운로드 실패를 기다리지 않고 maxUnavailable=0의 보호를 확인한다.
        patch('deployment', 'web', {'spec': {'template': {'spec': {'containers': [
            {**deployment['spec']['template']['spec']['containers'][0], 'command': ['/does-not-exist-qa']}]}}}})
        wait(lambda: len(app_pods()) == 3 and any(not ready(p) for p in app_pods()))
        failure = k('rollout', 'status', 'deployment/web', '--timeout=4s', ok=False)
        assert failure.returncode and sum(ready(p) for p in app_pods()) == 2
        assert serves(service_ip)
        save('failed-rollout.json', get('pods'))
        note('실패한 롤아웃에서 이전 2개 Pod와 HTTP 보존')
        k('rollout', 'undo', 'deployment/web')
        k('rollout', 'status', 'deployment/web', '--timeout=60s')
        wait(lambda: steady(2) and serves(service_ip))
        history = k('rollout', 'history', 'deployment/web').stdout
        assert 'REVISION' in history
        note('rollout undo 및 이력 조회', history)
        refusal = k('delete', 'namespace', namespace, '--wait=false', ok=False)
        assert refusal.returncode and 'Conflict' in refusal.stderr
        note('비어 있지 않은 namespace 삭제 거절')

        if args.restart_server:
            # 현재 테스트 Pod와 실제 다시봄 Pod만 HTTP를 검사한다. 다른
            # 네임스페이스의 앱이 TCP/80을 제공한다고 가정하지 않는다.
            live_pods = json.loads(k('get', 'pods', '-n', 'local-apps', '-l', 'app=dasibom', '-o', 'json').stdout)['items']
            direct = {p['status']['podIP']: request(p['status']['podIP'])[2] for p in app_pods() + live_pods if ready(p)}
            before = all_identities()
            command([str(args.sw), 'down'])
            server_off = True
            assert all(serves(ip, body) for ip, body in direct.items())
            note('sw down 중 직접 Pod HTTP 유지')
            restart_up()
            assert all_identities() == before and serves(main_ip, main_index)
            note('sw up 후 모든 Pod UID·IP·컨테이너 ID·재시작 수 보존')
            before = all_identities()
            state = json.loads(Path('/run/shearwater/server.json').read_text())
            pid = state['pid']
            handle = os.pidfd_open(pid)
            try:
                assert Path(f'/proc/{pid}/exe').resolve() == args.sw.resolve()
                fields = Path(f'/proc/{pid}/stat').read_text().split(')', 1)[1].split()
                assert fields[19] == state['startTick']
                signal.pidfd_send_signal(handle, signal.SIGKILL)
                server_off = True
            finally:
                os.close(handle)
            assert all(serves(ip, body) for ip, body in direct.items())
            restart_up()
            assert all_identities() == before and serves(main_ip, main_index)
            note('SIGKILL 뒤 동일 태스크 인계 및 Service 복구')
        command([str(args.sw), 'doctor'])
        assert serves(main_ip, main_index)
        assert all(all_identities().get(uid) == value for uid, value in original.items())
        note('doctor 및 실제 앱의 원래 태스크 보존')
    except Exception as error:
        results.append({'case': '검사 중단', 'pass': False, 'error': str(error)})
        save('results.json', results)
        raise
    finally:
        if server_off:
            restart_up()
        if owned:
            # 재실행되거나 다른 작업이 이름을 재사용했어도 남의 객체는 지우지
            # 않도록 처음 기록한 Namespace UID를 확인한다. 증거 파일은 남긴다.
            assert get('namespace', namespace)['metadata']['uid'] == ns_uid
            k('delete', 'deployment', 'web', '--ignore-not-found', '--wait=false')
            k('delete', 'service', 'web', '--ignore-not-found', '--wait=false')
            wait(lambda: not get('pods')['items'] and not get('replicasets')['items'])
            wait(lambda: not get('events')['items'] and not get('endpointslices')['items'])
            k('delete', 'namespace', namespace, '--wait=false')
            note('자체 테스트 객체 정리, 증거 파일 보존')
        print('Evidence: ' + str(evidence), flush=True)


if __name__ == '__main__':
    main()
