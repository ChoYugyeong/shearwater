#!/usr/bin/env python3
"""지정한 정적 HTTP Service의 응답과 Shearwater 자원을 읽어 검사한다.

host: Linux에서 API로 Service IP와 선택된 Ready Pod 전체를 찾는다.
기본 예제: default namespace, app=web인 nginx Deployment, Service web/TCP80.
nginx 기본 index.html만 있으면 된다. JS/CSS는 있을 때만 같은 출처에서 읽는다.
GET 원문을 baseline으로 비교한다. --expected-file로 별도 정답도 지정할 수 있다.
HEAD(200), Range(206), ETag(304), 없는 경로(404)는 기본 필수다.
미지원 서버는 실패한다. --skip-check로 명시적으로 제외하면 결과에 남으며
그 기능을 검증했다는 뜻이 아니다. 동적 HTML에는 적합하지 않다.
Service 밖의 Pod 변경은 기록만 한다. 선택된 Pod/Service 변경은 실패다.
probe: Windows/Linux에서 필수 --url을 1 req/s로 GET 비교한다(기능 검사는 host).
서버 시작/종료, 재시작, API 변경은 하지 않는다. host는 실제 부하를 발생시킨다.
"""
import argparse
from collections import Counter
import hashlib
from html.parser import HTMLParser
import http.client
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import shutil
import socket
import stat
import statistics
import struct
import subprocess
import sys
import threading
import time
from urllib.parse import urljoin, urlsplit
import uuid


# 보정된 MONOTONIC으로 실제 기다린 시간을 줄여 세지 않는다.
# Linux는 보정 전 RAW, Windows는 정밀한 QueryPerformanceCounter를 쓴다.
CLOCK_NAME = 'CLOCK_MONOTONIC_RAW' if hasattr(time, 'CLOCK_MONOTONIC_RAW') else 'perf_counter'
CHECKS = ('head', 'range', 'etag', 'not-found')
SCOPE_POLICY = 'Service selector의 모든 Pod 비교; 나머지 Pod 변경은 기록만 함'


class CheckError(Exception):
    """응답 본문/인증 정보를 포함하지 않는, 사용자에게 보여도 되는 오류."""


def require(condition, message):
    # assert는 python -O에서 사라진다. 안전/통과 조건은 항상 실행해야 한다.
    if not condition:
        raise CheckError(message)


def error_record(error):
    # kubectl stderr, HTTP 오류 본문, 토큰을 예외 문자열로 출력하지 않는다.
    result = {'type': type(error).__name__}
    if isinstance(error, CheckError):
        result['message'] = str(error)
    elif isinstance(error, OSError):
        result['errno'] = error.errno
    return result


def elapsed_clock():
    if hasattr(time, 'CLOCK_MONOTONIC_RAW'):
        return time.clock_gettime(time.CLOCK_MONOTONIC_RAW)
    return time.perf_counter()


def pause(seconds, event=None):
    """sleep/Event.wait의 시계가 빨라도 RAW deadline까지 남은 만큼 기다린다."""
    deadline = elapsed_clock() + max(0, seconds)
    while (remaining := deadline - elapsed_clock()) > 0:
        if event is None:
            time.sleep(remaining)
        elif event.wait(remaining):
            break


def windows_directory(path, create=False, private=False):
    """Python 3.12 mkdir(0700)은 Windows ACL을 제한하지 않아 Win32 API를 쓴다."""
    import ctypes as c
    from ctypes import wintypes as w
    adv = c.WinDLL('advapi32', use_last_error=True)
    kernel = c.WinDLL('kernel32', use_last_error=True)
    ptr = c.c_void_p
    adv.ConvertStringSecurityDescriptorToSecurityDescriptorW.argtypes = [w.LPCWSTR, w.DWORD, c.POINTER(ptr), ptr]
    adv.GetNamedSecurityInfoW.argtypes = [w.LPWSTR, w.DWORD, w.DWORD, c.POINTER(ptr), ptr, ptr, ptr, c.POINTER(ptr)]
    adv.GetNamedSecurityInfoW.restype = w.DWORD
    adv.OpenProcessToken.argtypes = [w.HANDLE, w.DWORD, c.POINTER(w.HANDLE)]
    adv.GetTokenInformation.argtypes = [w.HANDLE, c.c_int, ptr, w.DWORD, c.POINTER(w.DWORD)]
    adv.EqualSid.argtypes = [ptr, ptr]
    adv.ConvertSecurityDescriptorToStringSecurityDescriptorW.argtypes = [ptr, w.DWORD, w.DWORD, c.POINTER(ptr), ptr]
    kernel.LocalFree.argtypes = [ptr]
    kernel.GetCurrentProcess.restype = w.HANDLE
    kernel.CloseHandle.argtypes = [w.HANDLE]

    class Attributes(c.Structure):
        _fields_ = [('length', w.DWORD), ('descriptor', ptr), ('inherit', w.BOOL)]

    kernel.CreateDirectoryW.argtypes = [w.LPCWSTR, c.POINTER(Attributes)]
    # SYSTEM과 소유자만 접근. 상속도 차단하며 자식 파일은 이 ACL을 상속한다.
    sddl = 'D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;OW)'
    sd, token = ptr(), w.HANDLE()
    try:
        if create:
            require(adv.ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl, 1, c.byref(sd), None), 'Windows ACL 생성 실패')
            attributes = Attributes(c.sizeof(Attributes), sd, False)
            require(kernel.CreateDirectoryW(str(path), c.byref(attributes)), '출력 폴더 생성 실패(기존 경로/권한 확인)')
            kernel.LocalFree(sd)
            sd = ptr()
        owner = ptr()
        require(adv.GetNamedSecurityInfoW(str(path), 1, 5, c.byref(owner), None, None, None, c.byref(sd)) == 0, 'Windows 소유권 읽기 실패')
        require(adv.OpenProcessToken(kernel.GetCurrentProcess(), 8, c.byref(token)), 'Windows 사용자 확인 실패')
        size = w.DWORD()
        adv.GetTokenInformation(token, 1, None, 0, c.byref(size))
        buffer = c.create_string_buffer(size.value)
        require(adv.GetTokenInformation(token, 1, buffer, size, c.byref(size)), 'Windows SID 읽기 실패')
        require(adv.EqualSid(owner, c.cast(buffer, c.POINTER(ptr))[0]), '출력 경로는 실행 사용자 소유여야 함')
        if private or create:
            encoded = ptr()
            require(adv.ConvertSecurityDescriptorToStringSecurityDescriptorW(sd, 1, 4, c.byref(encoded), None), 'Windows ACL 읽기 실패')
            try:
                require(c.wstring_at(encoded) == sddl, '증거 폴더의 보호 ACL이 변경됨')
            finally:
                kernel.LocalFree(encoded)
    finally:
        if sd:
            kernel.LocalFree(sd)
        if token:
            kernel.CloseHandle(token)


def check_directory(path, private=False):
    require(path.is_absolute() and path.resolve() == path, '출력 경로는 링크/.. 없는 절대 경로여야 함')
    info = path.stat()
    require(stat.S_ISDIR(info.st_mode), '출력 부모가 디렉터리가 아님')
    if os.name == 'nt':
        windows_directory(path, private=private)
    else:
        require(info.st_uid == os.geteuid(), '출력 경로는 실행 사용자 소유여야 함')
        require(not info.st_mode & (0o077 if private else 0o022),
                '출력 권한이 너무 넓음(증거 0700, 부모는 다른 사용자의 쓰기 금지)')


def evidence_directory(path, root, create=True):
    """명시한 소유 부모 바로 아래의 새 경로만 사용한다. 기존 증거는 보존한다."""
    check_directory(root)
    require(path.is_absolute() and path.parent == root and path.resolve() == path,
            '--output은 --output-root 바로 아래의 링크 없는 절대 경로여야 함')
    if create:
        require(not os.path.lexists(path), '기존 증거를 덮어쓰지 않음: 새 출력 이름을 사용해 줘')
        if os.name == 'nt':
            windows_directory(path, create=True)
        else:
            path.mkdir(mode=0o700)
    check_directory(path, private=True)
    return path


def new_file(path, binary=False):
    # O_EXCL은 기존 파일/링크를 거절한다. Linux 파일 권한은 0600.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0), 0o600)
    return os.fdopen(fd, 'wb' if binary else 'w', **({} if binary else {'encoding': 'utf-8'}))


def write_json(path, data):
    with new_file(path) as file:
        json.dump(data, file, ensure_ascii=False, indent=2)
        file.write('\n')


def summary(values):
    if not values:
        return {}
    ordered = sorted(values)
    return {'count': len(values), 'p50_ms': statistics.median(ordered),
            'p95_ms': ordered[min(len(ordered)-1, int(len(ordered)*.95))],
            'p99_ms': ordered[min(len(ordered)-1, int(len(ordered)*.99))], 'max_ms': max(ordered)}


def command_output(command, label):
    completed = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, timeout=30, check=False)
    # 실패를 숨기지 않되 stderr에 있을지 모르는 토큰은 출력하지 않는다.
    require(completed.returncode == 0,
            f'{label} 실패: exit={completed.returncode}, stderr_sha256={hashlib.sha256(completed.stderr).hexdigest()}')
    return completed.stdout.decode('utf-8')


class API:
    """YAML을 임의로 해석하지 않고 kubectl에 kubeconfig 처리를 맡긴다."""
    def __init__(self, args, output):
        self.args, self.output = args, output

    def get(self, kind, name=None):
        args = self.args
        command = [args.kubectl, '--kubeconfig=' + str(args.kubeconfig),
                   '--cache-dir=' + str(self.output/'discovery'), '--request-timeout=15s',
                   '--namespace=' + args.namespace, 'get', kind]
        if name:
            command.append(name)
        return json.loads(command_output(command + ['-o', 'json'], 'kubectl get ' + kind))


def component(value):
    require(isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*', value),
            '리소스 이름/UID에 안전하지 않은 경로 문자가 있음')
    return value


def metadata(obj):
    value = obj['metadata']
    return {key: value.get(key) for key in ('name', 'uid', 'resourceVersion', 'generation', 'labels', 'deletionTimestamp')}


def pod_record(pod):
    status = pod['status']
    containers = status.get('containerStatuses', [])
    names = [component(c['name']) for c in containers]
    require(names and len(set(names)) == len(names), '컨테이너 상태가 없거나 이름이 중복됨')
    require(set(names) == {c['name'] for c in pod['spec']['containers']}, '컨테이너 spec/status 이름 불일치')
    require(not pod['metadata'].get('deletionTimestamp'), '선택된 Pod가 삭제 중임')
    require(any(c['type'] == 'Ready' and c['status'] == 'True' for c in status.get('conditions', [])), '선택된 Pod가 Ready가 아님')
    require(status.get('podIP') and pod['metadata'].get('uid'), '선택된 Pod IP/UID 없음')
    require(all(c.get('ready') and c.get('containerID') and isinstance(c.get('restartCount'), int) for c in containers),
            '선택된 컨테이너가 준비되지 않았거나 실행 식별 정보 없음')
    return {'metadata': metadata(pod), 'podIP': status['podIP'],
            'containers': {c['name']: {k: c[k] for k in ('containerID', 'restartCount', 'ready')} for c in containers}}


def fixture(api, service_name):
    service = api.get('services', service_name)
    selector = service['spec'].get('selector')
    require(isinstance(selector, dict) and selector, '비어 있지 않은 Service selector가 필요함')
    require(service['metadata'].get('uid') and not service['metadata'].get('deletionTimestamp'), 'Service UID 없음/삭제 중')
    # API의 ClusterIP와 selector를 사용한다. 고정 IP나 첫 번째 Pod를 가정하지 않는다.
    ipaddress.ip_address(service['spec']['clusterIP'])
    selected, unrelated = {}, {}
    for pod in api.get('pods')['items']:
        name = component(pod['metadata']['name'])
        if all(pod['metadata'].get('labels', {}).get(k) == v for k, v in selector.items()):
            require(name not in selected, '중복 Pod 이름')
            selected[name] = pod_record(pod)
        else:
            unrelated[name] = {k: pod['metadata'].get(k) for k in ('uid', 'resourceVersion')}
    require(selected, 'Service selector에 맞는 Ready Pod가 없음')
    return {'service': {'metadata': metadata(service), 'spec': service['spec']},
            'pods': selected, 'unrelated_pods': unrelated, 'scope_policy': SCOPE_POLICY}


def compare_fixture(before, after):
    return before['service'] == after['service'] and before['pods'] == after['pods']


def unrelated_changes(before, after):
    old, new = before['unrelated_pods'], after['unrelated_pods']
    return sorted(name for name in old.keys() | new.keys() if old.get(name) != new.get(name))


def service_url(service, port, path):
    ports = [p for p in service['spec']['ports'] if p.get('protocol', 'TCP') == 'TCP']
    chosen = [p for p in ports if p['port'] == port] if port is not None else ports
    require(len(chosen) == 1, 'TCP 포트가 없거나 여러 개임: --service-port로 하나를 골라 줘')
    host = str(ipaddress.ip_address(service['spec']['clusterIP']))
    if ':' in host:
        host = '[' + host + ']'
    require(path.startswith('/') and not path.startswith('//'), '--path는 /로 시작하는 HTTP 경로여야 함')
    return f'http://{host}:{chosen[0]["port"]}{path}'


def http_target(url):
    value = urlsplit(url)
    require(value.scheme in ('http', 'https') and value.hostname and not value.username and not value.password,
            'URL은 인증정보 없는 http:// 또는 https:// 주소여야 함')
    require(not value.query and not value.fragment and not any(ord(c) < 33 for c in url),
            'URL의 공백/제어문자/query/fragment 금지(토큰 유출 방지)')
    # port 속성을 읽을 때 잘못된 포트도 검출된다.
    require(value.port is None or 1 <= value.port <= 65535, 'URL 포트 범위 오류')
    return value


def connect(url, timeout):
    value = http_target(url)
    cls = http.client.HTTPSConnection if value.scheme == 'https' else http.client.HTTPConnection
    return cls(value.hostname, value.port, timeout=timeout)


def request(connection, method, path, headers=None):
    if connection.sock:
        connection.sock.settimeout(connection.timeout)
    connection.request(method, path, headers={'Accept-Encoding': 'identity', **(headers or {})})
    response = connection.getresponse()
    response_headers = {k.lower(): v for k, v in response.getheaders()}
    chunks, size = [], 0
    deadline = elapsed_clock() + connection.timeout
    while True:
        remaining = deadline - elapsed_clock()
        require(remaining > 0, 'HTTP 본문 수신 deadline 초과')
        if connection.sock:
            connection.sock.settimeout(remaining)
        chunk = response.read1(min(65536, 8*1024*1024+1-size))
        if not chunk:
            break
        chunks.append(chunk)
        size += len(chunk)
        require(size <= 8*1024*1024, 'HTTP 본문이 8 MiB 제한을 넘음')
    content = b''.join(chunks)
    # HEAD의 read1()은 빈 바이트를 반환해도 응답 객체를 닫지 않을 수 있다.
    # 응답을 마쳐야 같은 TCP 연결에서 다음 GET을 받을 수 있다.
    response.close()
    # read(크기 제한)는 Content-Length보다 짧은 EOF를 예외 없이 반환할 수 있다.
    # 불완전한 응답을 첫 baseline으로 받아 이후에도 통과시키면 안 된다.
    length = response_headers.get('content-length')
    if length is not None and method != 'HEAD' and response.status not in (204, 304):
        require(length.isdecimal() and int(length) == size, 'HTTP Content-Length보다 짧거나 잘못된 응답')
    return response.status, response_headers, content


def fetch(url, timeout, method='GET', path=None, headers=None):
    connection = connect(url, timeout)
    try:
        return request(connection, method, path or http_target(url).path or '/', headers)
    finally:
        connection.close()


class Assets(HTMLParser):
    def __init__(self):
        super().__init__()
        self.paths, self.base = [], None

    def handle_starttag(self, tag, attrs):
        values = dict(attrs)
        if tag == 'base' and self.base is None:
            self.base = values.get('href')
        path = values.get('src') if tag == 'script' else values.get('href') if tag == 'link' and 'stylesheet' in values.get('rel', '').split() else None
        if path:
            self.paths.append(path)


def http_cases(url, args):
    status, headers, expected = fetch(url, args.timeout)
    require(status == 200 and expected, 'baseline GET은 비어 있지 않은 200 응답이어야 함')
    require(headers.get('content-type', '').split(';')[0].lower() == 'text/html', 'baseline은 정적 text/html이어야 함')
    if args.expected_file:
        require(expected == args.expected_file.read_bytes(), 'baseline이 --expected-file과 다름')
    path = http_target(url).path or '/'
    cases = [('get', 'GET', path, {}, 200, expected, {})]
    assets = Assets()
    assets.feed(expected.decode('utf-8', errors='replace'))
    origin = http_target(url)
    excluded, seen = [], set()
    for reference in assets.paths:
        resolved = urlsplit(urljoin(urljoin(url, assets.base or ''), reference))
        def origin_key(value):
            return value.scheme, value.hostname, value.port or (443 if value.scheme == 'https' else 80)
        if origin_key(resolved) != origin_key(origin) or resolved.query or resolved.fragment:
            excluded.append('외부 출처 또는 query/fragment 자산: 수집하지 않음')
            continue
        asset_path = resolved.path or '/'
        if asset_path in seen:
            continue
        seen.add(asset_path)
        require(len(seen) <= 32, '자산이 32개를 넘음: 작은 정적 fixture를 사용해 줘')
        code, _, body = fetch(url, args.timeout, path=asset_path)
        require(code == 200 and body, '같은 출처 JS/CSS baseline GET 실패')
        cases.append(('asset', 'GET', asset_path, {}, 200, body, {}))
    skip = set(args.skip_check)
    if 'head' not in skip:
        cases.append(('head', 'HEAD', path, {}, 200, b'', {'content-length': str(len(expected))}))
    if 'range' not in skip:
        end = min(1024, len(expected)) - 1
        cases.append(('range', 'GET', path, {'Range': f'bytes=0-{end}'}, 206, expected[:end+1],
                      {'content-range': f'bytes 0-{end}/{len(expected)}'}))
    if 'not-found' not in skip:
        cases.append(('not-found', 'GET', '/soak-intentionally-missing-' + uuid.uuid4().hex, {}, 404, None, {}))
    if 'etag' not in skip:
        etag = headers.get('etag')
        require(etag and re.fullmatch(r'(W/)?"[^"\r\n]*"', etag), 'ETag 없음/형식 오류: 필수 etag 미지원')
        cases.append(('etag', 'GET', path, {'If-None-Match': etag}, 304, b'', {'etag': etag}))
    # 필수 기능은 부하 전에도 확인한다. 무지원 서버를 자동 제외하지 않는다.
    for case in cases:
        check_response(case, fetch(url, args.timeout, case[1], case[2], case[3]))
    return cases, {'sha256': hashlib.sha256(expected).hexdigest(), 'bytes': len(expected),
                   'asset_count': len(seen), 'excluded_assets': excluded,
                   'required_checks': [c for c in CHECKS if c not in skip], 'skipped_checks': sorted(skip)}


def check_response(case, received):
    name, _, _, _, wanted, body, wanted_headers = case
    status, headers, content = received
    require(status == wanted, f'{name}: HTTP {status}, 기대값 {wanted}')
    require(body is None or content == body, f'{name}: 응답 바이트가 baseline과 다름')
    for key, value in wanted_headers.items():
        require(headers.get(key) == value, f'{name}: {key} 헤더 불일치/미지원')


def proc_sample(pid):
    require(isinstance(pid, int) and pid > 0, '유효하지 않은 PID')
    base = Path(f'/proc/{pid}')
    # comm에는 공백과 ')'도 들어간다. 마지막 ')' 다음부터 세야 한다.
    fields = (base/'stat').read_text().rsplit(')', 1)[1].split()
    values = dict(row.split(':', 1) for row in (base/'status').read_text().splitlines() if ':' in row)
    io_values = dict(row.split(':', 1) for row in (base/'io').read_text().splitlines())
    return {'start_tick': fields[19], 'rss_kib': int(values['VmRSS'].split()[0]),
            'fd': len(list((base/'fd').iterdir())), 'threads': int(values['Threads']),
            'cpu_ticks': int(fields[11])+int(fields[12]), 'write_bytes': int(io_values['write_bytes'])}


def container_tasks(args, selected):
    text = command_output([args.ctr, '--address', str(args.containerd_socket), '--namespace', args.containerd_namespace,
                           'tasks', 'list'], 'ctr tasks list')
    tasks = {}
    for row in text.splitlines()[1:]:
        fields = row.split()
        if fields:
            require(len(fields) >= 3 and fields[1].isdigit(), 'ctr task 목록 형식 오류')
            tasks[fields[0]] = {'pid': int(fields[1]), 'status': fields[2]}
    selected_tasks = {}
    for name, pod in selected['pods'].items():
        for container, info in pod['containers'].items():
            require(info['containerID'].startswith('containerd://'), 'containerd 실행 정보가 필요함')
            task_id = info['containerID'].removeprefix('containerd://')
            require(task_id in tasks and tasks[task_id]['status'] == 'RUNNING', '선택된 컨테이너의 RUNNING task가 없음')
            selected_tasks[name + '/' + container] = tasks[task_id]
    return selected_tasks


def log_bytes(args, selected):
    total = 0
    for name, pod in selected['pods'].items():
        uid = component(pod['metadata']['uid'])
        for container in pod['containers']:
            directory = args.log_dir/'pods'/f'{args.namespace}_{name}_{uid}'/component(container)
            require(directory.is_dir(), '선택된 컨테이너 로그 디렉터리 없음')
            logs = list(directory.glob('*.log'))
            require(logs, '선택된 컨테이너 로그 파일 없음(계측을 0으로 숨기지 않음)')
            total += sum(p.stat().st_size for p in logs)
    return total


def probe(args):
    started = elapsed_clock()
    expected = hashlib.sha256(args.expected_file.read_bytes()).hexdigest() if args.expected_file else None
    records = []
    while elapsed_clock() - started < args.seconds:
        begin = elapsed_clock()
        record = {'elapsed_s': round(begin-started, 3)}
        try:
            status, _, content = fetch(args.url, args.timeout)
            digest = hashlib.sha256(content).hexdigest()
            record.update(code=status, same_body=False)
            require(status == 200 and content, 'probe GET은 비어 있지 않은 HTTP 200이어야 함')
            if expected is None:
                expected = digest
            record.update(code=status, same_body=(digest == expected))
        except Exception as error:
            record['error'] = error_record(error)
        record['ms'] = (elapsed_clock()-begin)*1000
        records.append(record)
        if len(records) % 60 == 0:
            write_json(args.output/f'progress-{len(records):06d}.json', {'elapsed_s': record['elapsed_s'], 'requests': len(records)})
        pause(min(max(0, 1-(elapsed_clock()-begin)), max(0, args.seconds-(elapsed_clock()-started))))
    errors = sum(not (r.get('code') == 200 and r.get('same_body')) for r in records)
    duration = elapsed_clock()-started
    enough = len(records) >= math.ceil(args.seconds * .8)
    return {'mode': 'probe', 'duration_s': duration, 'planned_s': args.seconds, 'clock_source': CLOCK_NAME,
            'requests': len(records), 'errors': errors, 'sha256': expected, 'enough_requests': enough,
            'checks': ['GET 200', 'baseline bytes'], 'latency': summary([r['ms'] for r in records]),
            'records': records, 'passed': not errors and duration >= args.seconds and enough}


def host(args):
    state = json.loads((args.run_dir/'server.json').read_text())
    if args.log_dir is None:
        saved_log_dir = state.get('options', {}).get('LogDir')
        require(saved_log_dir, '서버 상태에 로그 경로 없음: sw 실행 때의 --log-dir를 지정해 줘')
        args.log_dir = Path(saved_log_dir)
    require(args.log_dir.is_absolute(), '--log-dir는 절대 경로여야 함')
    api = API(args, args.output)
    before = fixture(api, args.service)
    url = service_url(before['service'], args.service_port, args.path)
    cases, baseline_http = http_cases(url, args)
    pid = state['pid']
    tasks = container_tasks(args, before)
    process_starts = {name: proc_sample(task['pid'])['start_tick'] for name, task in tasks.items()}
    lock, stop = threading.Lock(), threading.Event()
    latencies, interval, errors, measurements, unrelated = [], [], [], [], []
    totals, statuses, case_counts = Counter(), Counter(), Counter()
    started, started_unix = elapsed_clock(), time.time()

    def rate():
        part = (elapsed_clock()-started) / args.seconds
        return args.rates[1] if 1/3 <= part < 2/3 else args.rates[0] if part < 1/3 else args.rates[2]

    def record_error(error, **details):
        with lock:
            if len(errors) < 100:
                errors.append({'elapsed_s': elapsed_clock()-started, **details, **error_record(error)})

    def worker(index):
        connection, number = None, 0
        try:
            while not stop.is_set() and elapsed_clock()-started < args.seconds:
                begin = elapsed_clock()
                case = cases[(index+number) % len(cases)]
                try:
                    if connection is None:
                        connection = connect(url, args.timeout)
                    response = request(connection, case[1], case[2], case[3])
                    with lock:
                        statuses[str(response[0])] += 1
                    check_response(case, response)
                    latency = (elapsed_clock()-begin)*1000
                    with lock:
                        totals['ok'] += 1
                        case_counts[case[0] + ' ' + case[2]] += 1
                        latencies.append(latency)
                        interval.append(latency)
                except Exception as error:
                    with lock:
                        totals['errors'] += 1
                    record_error(error, worker=index, check=case[0])
                    if connection:
                        connection.close()
                        connection = None
                # 실패는 재시도로 지우지 않는다. 반은 새 TCP, 반은 8회 재사용한다.
                number += 1
                if connection and (index % 2 or number % 8 == 0):
                    connection.close()
                    connection = None
                pause(max(0, args.workers/rate() - (elapsed_clock()-begin)), stop)
        except BaseException as error:
            with lock:
                totals['worker_failures'] += 1
            record_error(error, worker=index)
            stop.set()
        finally:
            if connection:
                connection.close()

    def cancellations():
        target = http_target(url)
        try:
            while not stop.is_set() and elapsed_clock()-started < args.seconds:
                try:
                    with socket.create_connection((target.hostname, target.port), timeout=args.timeout) as sock:
                        sock.sendall(f'GET {target.path or "/"} HTTP/1.1\r\nHost: {target.netloc}\r\n\r\n'.encode('ascii'))
                        # 응답 도중 탭을 닫는 RST를 흉내낸다. 정상 응답과 따로 센다.
                        sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('hh' if os.name == 'nt' else 'ii', 1, 0))
                    with lock:
                        totals['intentional_cancellations'] += 1
                except Exception as error:
                    with lock:
                        totals['cancel_connect_errors'] += 1
                    record_error(error, check='cancellation')
                pause(1, stop)
        except BaseException as error:
            with lock:
                totals['worker_failures'] += 1
            record_error(error, check='cancellation thread')
            stop.set()

    def measure(phase):
        sw = proc_sample(pid)
        require(sw['start_tick'] == state['startTick'], 'sw 프로세스 시작 tick이 바뀜')
        containers = {name: proc_sample(task['pid']) for name, task in tasks.items()}
        require(all(containers[n]['start_tick'] == tick for n, tick in process_starts.items()), '선택된 컨테이너 프로세스가 교체됨')
        disk = shutil.disk_usage(args.data_dir)
        sample = {'elapsed_s': round(elapsed_clock()-started, 3), 'phase': phase, 'sw': sw,
                  'containers': containers, 'load_generator': proc_sample(os.getpid()),
                  'pod_log_bytes': log_bytes(args, before), 'server_log_bytes': (args.log_dir/'server.log').stat().st_size,
                  'disk_free_bytes': disk.free}
        measurements.append(sample)
        metrics.write(json.dumps(sample) + '\n')
        metrics.flush()
        require(sw['rss_kib'] < 256*1024, 'sw RSS 안전 상한 256 MiB 초과')
        require(disk.free > 1024**3, '디스크 여유 안전 하한 1 GiB 미만')
        return sample

    failure, same_state, baseline, ending = None, False, None, None
    load_started = False
    threads = [threading.Thread(target=worker, args=(i,), daemon=True) for i in range(args.workers)]
    threads.append(threading.Thread(target=cancellations, daemon=True))
    with new_file(args.output/'metrics.jsonl') as metrics:
        try:
            baseline = measure('before')
            write_json(args.output/'baseline.json', {'observed_unix': time.time(), 'clock_source': CLOCK_NAME,
                       'planned_s': args.seconds, 'rates': args.rates, 'workers': args.workers,
                       'fixture': before, 'url': url, 'http': baseline_http, 'sample': baseline})
            # baseline 수집/쓰기 시간은 실제 부하 시간에 포함하지 않는다.
            started, started_unix = elapsed_clock(), time.time()
            load_started = True
            for thread in threads:
                thread.start()
            last_report, report_number = started, 0
            while elapsed_clock()-started < args.seconds and not stop.is_set():
                pause(min(10, max(0, args.seconds-(elapsed_clock()-started))), stop)
                sample = measure('load')
                if elapsed_clock()-last_report >= 59:
                    current = fixture(api, args.service)
                    unrelated.append({'elapsed_s': sample['elapsed_s'], 'names': unrelated_changes(before, current)})
                    require(compare_fixture(before, current), '선택된 Pod 또는 Service가 부하 중 변경됨')
                    with lock:
                        progress = {'elapsed_s': sample['elapsed_s'], 'totals': dict(totals),
                                    'status_counts': dict(statuses), 'latency': summary(interval), 'sw': sample['sw']}
                        interval.clear()
                    report_number += 1
                    write_json(args.output/f'progress-{report_number:06d}.json', progress)
                    print(json.dumps(progress), flush=True)
                    last_report = elapsed_clock()
                    require(totals['errors'] < 10 and not totals['cancel_connect_errors'], '반복 연결 오류로 중단')
        except BaseException as error:
            failure = error_record(error)
        finally:
            load_elapsed = elapsed_clock()-started if load_started else 0
            stop.set()
            for thread in threads:
                if thread.ident is not None:
                    thread.join(timeout=args.timeout+2)
        try:
            # 부하 시간과 종료 뒤 FD 회수 관찰 시간을 분리한다.
            cooldown_start = elapsed_clock()
            while elapsed_clock()-cooldown_start < args.cooldown:
                pause(max(0, min(10, args.cooldown-(elapsed_clock()-cooldown_start))))
                measure('cooldown')
            ending = measure('after')
            current = fixture(api, args.service)
            unrelated.append({'elapsed_s': elapsed_clock()-started, 'names': unrelated_changes(before, current)})
            same_state = compare_fixture(before, current)
        except BaseException as error:
            record_error(error, check='ending evidence')
            failure = failure or error_record(error)
    expected_requests = args.seconds * sum(args.rates) / 3
    enough = totals['ok'] >= expected_requests * .8
    workers_stopped = all(not thread.is_alive() for thread in threads)
    covered = all(case_counts[c[0] + ' ' + c[2]] for c in cases)
    report = {'mode': 'host', 'planned_s': args.seconds, 'clock_source': CLOCK_NAME,
              'load_elapsed_s': load_elapsed, 'cooldown_s': args.cooldown, 'started_unix': started_unix,
              'finished_unix': time.time(), 'totals': dict(totals), 'expected_requests': expected_requests,
              'enough_requests': enough, 'achieved_rps': totals['ok']/load_elapsed if load_elapsed else 0, 'workers_stopped': workers_stopped,
              'status_counts': dict(statuses), 'case_counts': dict(case_counts), 'all_cases_exercised': covered,
              'latency': summary(latencies), 'errors': errors, 'same_pod_and_service': same_state,
              'scope_policy': SCOPE_POLICY, 'unrelated_pod_changes': unrelated, 'failure': failure,
              'http': baseline_http, 'baseline': baseline, 'ending': ending,
              'passed': not failure and not totals['errors'] and not totals['worker_failures'] and
                        not totals['cancel_connect_errors'] and same_state and load_elapsed >= args.seconds and
                        enough and covered and workers_stopped}
    if baseline and ending:
        ticks = ending['sw']['cpu_ticks']-baseline['sw']['cpu_ticks']
        report['sw_cpu_percent_one_core'] = 100*ticks/os.sysconf('SC_CLK_TCK')/(elapsed_clock()-started)
        report['sw_rss_peak_kib'] = max(m['sw']['rss_kib'] for m in measurements)
    return report


def add_instance_arguments(parser):
    parser.add_argument('--config-dir', type=Path, default=Path('/etc/shearwater'), help='인증 파일 디렉터리 (기본: %(default)s)')
    parser.add_argument('--run-dir', type=Path, default=Path('/run/shearwater'), help='해당 인스턴스의 임시 상태 디렉터리')
    parser.add_argument('--data-dir', type=Path, default=Path('/var/lib/shearwater'), help='해당 인스턴스의 SQLite/데이터 디렉터리')
    parser.add_argument('--kubeconfig', type=Path, help='접속 설정 (생략하면 config-dir/kubeconfig); 내용은 출력하지 않음')
    parser.add_argument('--kubectl', default='kubectl', help='kubectl 실행 파일 경로 또는 PATH 이름')
    parser.add_argument('--namespace', default='default', help='검사 namespace (기본: default)')
    parser.add_argument('--service', default='web', help='검사 Service (기본: web); readiness 예제는 해당 Service 이름 지정')
    parser.add_argument('--ctr', default='ctr', help='containerd 조회 도구 실행 파일 경로 또는 PATH 이름')
    parser.add_argument('--containerd-socket', type=Path, default=Path('/run/containerd/containerd.sock'), help='이미 실행 중인 containerd 소켓')
    parser.add_argument('--containerd-namespace', default='shearwater', help='이 인스턴스가 소유한 containerd namespace')
    parser.add_argument('--output-root', type=Path, help='기존 실행 사용자 소유 증거 부모(다른 사용자 쓰기 금지); 기본: data-dir/check')


def instance_defaults(args):
    args.kubeconfig = args.kubeconfig or args.config_dir/'kubeconfig'
    args.output_root = args.output_root or args.data_dir/'check'
    for value in (args.config_dir, args.run_dir, args.data_dir, args.kubeconfig, args.containerd_socket):
        require(value.is_absolute(), '인스턴스 경로 인자는 절대 경로를 사용해 줘')
    component(args.namespace)
    component(args.service)
    require(args.containerd_namespace not in ('moby', 'moby_history'), 'Docker containerd namespace는 검사 대상이 아님')


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    modes = parser.add_subparsers(dest='mode', required=True)
    for name, help_text in [('host', 'Linux 호스트의 Service/Pod/자원 검사'), ('probe', '명시 URL의 GET 연속 검사(Windows 지원)')]:
        sub = modes.add_parser(name, help=help_text, description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
        sub.add_argument('--seconds', type=int, default=3600, help='검사 시간 초 (기본: 3600; host는 10 이상)')
        sub.add_argument('--output', type=Path, required=True, help='새 증거 폴더 절대 경로; 기존 폴더는 덮어쓰지 않음')
        sub.add_argument('--expected-file', type=Path, help='선택적 정답 HTML; 없으면 첫 정상 GET 바이트가 기준')
        sub.add_argument('--timeout', type=float, default=5, help='HTTP 소켓 시간 제한 초 (0 초과, 30 이하)')
        if name == 'probe':
            sub.add_argument('--url', required=True, help='필수 HTTP(S) URL; 인증/query/fragment 금지, redirect 추적 없음')
            sub.add_argument('--output-root', type=Path, required=True, help='기존 실행 사용자 소유 부모 폴더 절대 경로')
        else:
            add_instance_arguments(sub)
            sub.add_argument('--log-dir', type=Path, help='sw 로그 경로; 생략하면 run-dir/server.json에 저장된 LogDir 사용')
            sub.add_argument('--service-port', type=int, help='TCP Service 포트; 생략하면 유일한 TCP 포트 사용')
            sub.add_argument('--path', default='/', help='정적 HTML 경로 (기본: /)')
            sub.add_argument('--cooldown', type=int, default=60, help='부하 종료 뒤 자원 관찰 초 (0..300)')
            sub.add_argument('--workers', type=int, default=6, help='HTTP 작업자 수 (1..12)')
            sub.add_argument('--rates', type=int, nargs=3, default=[20, 60, 20], metavar=('EARLY', 'MIDDLE', 'LATE'), help='세 구간 목표 req/s (각 1..60); readiness Service도 별도 실행 가능')
            sub.add_argument('--skip-check', action='append', choices=CHECKS, default=[], help='명시적으로 제외할 기능(반복 가능). 미지원 기능은 자동 제외하지 않음')
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        require(args.seconds > 0 and 0 < args.timeout <= 30, 'seconds는 양수, timeout은 0 초과 30 이하여야 함')
        if args.mode == 'host':
            instance_defaults(args)
            require(sys.platform.startswith('linux'), 'host는 /proc 계측이 가능한 Linux에서 실행해 줘')
            require(args.seconds >= 10 and 0 <= args.cooldown <= 300 and 1 <= args.workers <= 12, 'host 시간/작업자 범위 오류')
            require(all(1 <= n <= 60 for n in args.rates), 'rates는 각각 1..60 req/s여야 함')
            require(args.log_dir is None or args.log_dir.is_absolute(), '--log-dir는 절대 경로여야 함')
            require(args.service_port is None or 1 <= args.service_port <= 65535, 'Service 포트 범위 오류')
            require(args.path.startswith('/') and not args.path.startswith('//'), '--path는 /로 시작하는 HTTP 경로여야 함')
            http_target('http://localhost' + args.path)
        else:
            http_target(args.url)
        evidence_directory(args.output, args.output_root)
    except (CheckError, OSError, ValueError) as error:
        parser.error(json.dumps(error_record(error), ensure_ascii=False))
    try:
        result = (host if args.mode == 'host' else probe)(args)
    except BaseException as error:
        result = {'mode': args.mode, 'passed': False, 'failure': error_record(error), 'stage': 'setup or collection'}
    write_json(args.output/'result.json', result)
    print(json.dumps({k: v for k, v in result.items() if k != 'records'}, ensure_ascii=False), flush=True)
    return 0 if result['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
