#!/usr/bin/env python3
"""WSL/Linux 재부팅 전/기동 직후/서버 복구 후의 읽기 전용 증거를 남긴다.

실행 순서: before → 사용자가 재부팅 → booted → 사용자가 sw up → after.
이 스크립트는 재부팅/서버 시작/종료/API 변경을 하지 않는다. booted는 API를
호출하지 않는다. 쓰기는 전용 증거, kubectl 캐시, run-dir의 소유 표식뿐이다.
기본 fixture: default의 nginx Deployment web(app=web)과 Service web(TCP80).
Service selector에 맞는 모든 Pod의 UID/IP, 이름별 컨테이너 ID/재시작을 비교한다.
fixture 밖의 Pod/Deployment/Service 변경은 판정 대상이 아니다.
인증 파일은 해시만, DB는 읽기 전용 온라인 백업으로 수집한다. 실패도 JSON으로
남긴다. 파일 존재만 보지 말고 passed와 프로그램 종료 코드 0을 함께 확인해 줘.
"""
import argparse
from contextlib import closing
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import sys
import time

# 외부 패키지 없이 두 CLI가 같은 소유권/출력/선택 범위 규칙을 사용한다.
_spec = importlib.util.spec_from_file_location('portable_soak_checks', Path(__file__).with_name('soak-check.py'))
common = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(common)
require = common.require


def target_record(args):
    return {'config_dir': str(args.config_dir), 'run_dir': str(args.run_dir),
            'data_dir': str(args.data_dir), 'kubeconfig': str(args.kubeconfig),
            'namespace': args.namespace, 'service': args.service, 'deployment': args.deployment,
            'containerd_socket': str(args.containerd_socket), 'containerd_namespace': args.containerd_namespace}


def boot_record(run_dir, marker):
    return {'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
            'init_start_tick': Path('/proc/1/stat').read_text().rsplit(')', 1)[1].split()[19],
            'marker_present': os.path.lexists(marker),
            'server_state_present': (run_dir/'server.json').exists()}


def same_boot(first, second):
    return (first['boot_id'], first['init_start_tick']) == (second['boot_id'], second['init_start_tick'])


def read_evidence(path):
    require(path.is_file() and path.resolve() == path, '이전 단계 증거가 없거나 링크임')
    if os.name != 'nt':
        info = path.stat()
        require(info.st_uid == os.geteuid() and not info.st_mode & 0o077 and info.st_nlink == 1, '이전 증거의 소유권/권한 오류')
    return json.loads(path.read_text(encoding='utf-8'))


def database_checks(database, backup=None):
    # mode=ro는 원본 DB를 만들거나 고치지 않는다. WAL까지 읽어 일관된 백업을 만든다.
    with closing(sqlite3.connect(database.as_uri() + '?mode=ro', uri=True, timeout=15)) as source:
        source.execute('PRAGMA query_only=ON')
        result = {'quick_check': source.execute('PRAGMA quick_check').fetchall(),
                  'foreign_keys': source.execute('PRAGMA foreign_key_check').fetchall()}
        if backup:
            with common.new_file(backup, binary=True):
                pass
            deadline = common.elapsed_clock() + 30

            def progress(status, remaining, total):
                require(common.elapsed_clock() <= deadline, 'SQLite 백업이 30초 제한을 넘음')

            with closing(sqlite3.connect(backup)) as destination:
                source.backup(destination, pages=128, progress=progress, sleep=.1)
        # JSON 저장 뒤 읽어도 tuple/list 차이가 없게 통일한다.
        return json.loads(json.dumps(result))


def collect(args):
    api = common.API(args, args.evidence)
    selected = common.fixture(api, args.service)
    deployment = api.get('deployments', args.deployment)
    require(deployment['metadata'].get('uid') and not deployment['metadata'].get('deletionTimestamp'), 'Deployment UID 없음/삭제 중')
    labels = deployment['spec']['template']['metadata'].get('labels', {})
    require(all(labels.get(k) == v for k, v in selected['service']['spec']['selector'].items()),
            'Deployment template이 Service selector와 맞지 않음')
    # 환경변수/annotations에는 비밀이 있을 수 있다. 전체 리소스는 저장하지 않는다.
    selected['deployment'] = {'metadata': common.metadata(deployment),
                              'replicas': deployment['spec'].get('replicas', 1),
                              'selector': deployment['spec']['selector']}
    require(len(selected['pods']) == selected['deployment']['replicas'], 'Deployment replica 수와 선택된 Pod 수가 다름')
    slices = api.get('endpointslices')['items']
    selected['endpointslices'] = [
        {'name': s['metadata']['name'], 'uid': s['metadata']['uid'],
         'addresses': sorted(a for e in s.get('endpoints', []) for a in e.get('addresses', []))}
        for s in slices if s['metadata'].get('labels', {}).get('kubernetes.io/service-name') == args.service]
    state = json.loads((args.run_dir/'server.json').read_text())
    require(common.proc_sample(state['pid'])['start_tick'] == state['startTick'], 'sw PID/시작 tick이 실제 프로세스와 다름')
    return {'fixture': selected,
            'auth_sha256': {name: hashlib.sha256(path.read_bytes()).hexdigest()
                            for name, path in [('admin.token', args.config_dir/'admin.token'),
                                               ('ca.crt', args.config_dir/'ca.crt'), ('kubeconfig', args.kubeconfig)]},
            'sqlite': database_checks(args.data_dir/'state.db',
                                     args.evidence/'state-before.db' if args.phase == 'before' else None),
            'tasks': common.container_tasks(args, selected),
            'server_process': {'pid': state['pid'], 'start_tick': state['startTick']}}


def compare_restored(before, after):
    """컨테이너 배열 순서가 달라도 이름별로 비교하고 비어 있는 비교를 금지한다."""
    require(before['auth_sha256'] == after['auth_sha256'], '인증 파일 해시가 바뀜')
    require(after['sqlite'] == {'quick_check': [['ok']], 'foreign_keys': []}, '복구된 SQLite 무결성 검사 실패')
    old, new = before['fixture'], after['fixture']
    checks = []
    for kind in ('service', 'deployment'):
        require(old[kind]['metadata']['uid'] == new[kind]['metadata']['uid'], f'{kind} UID가 바뀜')
        require(old[kind]['metadata']['name'] == new[kind]['metadata']['name'], f'{kind} 이름이 바뀜')
        require(old[kind]['metadata']['generation'] == new[kind]['metadata']['generation'], f'{kind} generation이 바뀜')
        checks.append(kind + '/' + old[kind]['metadata']['name'])
    require(old['service']['spec'] == new['service']['spec'], 'Service IP/selector/포트 설정이 바뀜')
    require(old['deployment']['replicas'] == new['deployment']['replicas'] and
            old['deployment']['selector'] == new['deployment']['selector'], 'Deployment replica/selector가 바뀜')
    require(old['pods'] and old['pods'].keys() == new['pods'].keys(), '선택된 Pod 목록이 비었거나 바뀜')
    for name, pod in old['pods'].items():
        restored = new['pods'][name]
        require(pod['metadata']['uid'] == restored['metadata']['uid'], 'Pod UID가 바뀜')
        require(pod['podIP'] == restored['podIP'], 'Pod IP가 바뀜')
        require(pod['containers'] and pod['containers'].keys() == restored['containers'].keys(), '컨테이너 이름 집합이 바뀜')
        for container, previous in pod['containers'].items():
            current = restored['containers'][container]
            require(current['ready'], '복구 컨테이너가 Ready가 아님')
            require(current['restartCount'] > previous['restartCount'] and
                    current['containerID'] != previous['containerID'], '컨테이너 재시작 횟수/ID가 복구를 증명하지 못함')
        checks.append('pod/' + name)
    return checks


def run_phase(args):
    marker_id = hashlib.sha256(str(args.evidence).encode('utf-8')).hexdigest()[:24]
    marker = args.run_dir/('reboot-check-' + marker_id + '.marker')
    result = {'phase': args.phase, 'unix_time': time.time(), 'target': target_record(args),
              'scope_policy': common.SCOPE_POLICY, 'passed': False}
    try:
        # 재부팅 표식은 이 run-dir에만 만든다. 기존 파일/링크는 덮어쓰지 않는다.
        # /run은 재부팅으로 디렉터리째 사라질 수 있다. booted는 이를 정상적인
        # 소멸 증거로 읽어야 하며, sw up 전에 폴더를 새로 만들지 않는다.
        if args.phase != 'booted' or args.run_dir.exists():
            common.check_directory(args.run_dir)
        else:
            require(args.run_dir.resolve() == args.run_dir, 'run-dir 경로에 링크/..가 있음')
        result.update(boot_record(args.run_dir, marker))
        if args.phase == 'before':
            require(not result['marker_present'], '같은 검사 표식이 이미 있음')
            with common.new_file(marker) as file:
                file.write('Only this evidence directory owns this reboot marker.\n')
            result['marker_created'] = True
        else:
            before = read_evidence(args.evidence/'before.json')
            require(before['passed'], 'before 단계가 통과하지 않았음: 새 증거 폴더로 시작해 줘')
            require(before['target'] == result['target'], '단계 사이 인스턴스/fixture 인자가 바뀜')
            require(not result['marker_present'], 'run-dir의 재부팅 표식이 남아 있음')
            require(not same_boot(before, result), '커널 boot ID와 init 시작 tick 모두 같음: 재부팅 증거 없음')
        if args.phase == 'booted':
            require(not result['server_state_present'], 'booted는 sw up 전 실행해야 함: 서버 상태 파일이 남아 있음')
        else:
            result.update(collect(args))
            require(result['sqlite'] == {'quick_check': [['ok']], 'foreign_keys': []}, 'SQLite 무결성 검사 실패')
        if args.phase == 'after':
            booted = read_evidence(args.evidence/'booted.json')
            require(booted['passed'] and booted['target'] == result['target'], '유효한 booted 증거가 필요함')
            require(same_boot(booted, result), 'booted 이후 추가 재부팅됨')
            result['identity_restored'] = compare_restored(before, result)
            result['unrelated_pod_changes'] = common.unrelated_changes(before['fixture'], result['fixture'])
        result['passed'] = True
    except BaseException as error:
        result['failure'] = common.error_record(error)
    common.write_json(args.evidence/(args.phase + '.json'), result)
    print(json.dumps({k: result[k] for k in ('phase', 'unix_time', 'passed', 'failure', 'identity_restored',
                                           'boot_id', 'init_start_tick', 'marker_present', 'server_state_present') if k in result},
                     ensure_ascii=False))
    return 0 if result['passed'] else 1


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('phase', choices=['before', 'booted', 'after'], help='전/기동 직후/서버 복구 후; 순서대로 실행')
    parser.add_argument('evidence', type=Path, help='모든 단계에 같은 증거 폴더 절대 경로; before는 새 폴더만 허용')
    common.add_instance_arguments(parser)
    parser.add_argument('--deployment', default='web', help='복구 비교 Deployment 이름 (기본: web)')
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        common.instance_defaults(args)
        common.component(args.deployment)
        require(sys.platform.startswith('linux'), '재부팅 증거는 /proc가 있는 Linux에서 수집해 줘')
        common.evidence_directory(args.evidence, args.output_root, create=args.phase == 'before')
        require(not os.path.lexists(args.evidence/(args.phase + '.json')), '이 단계의 증거가 이미 있음: 덮어쓰지 않음')
    except (common.CheckError, OSError, ValueError) as error:
        parser.error(json.dumps(common.error_record(error), ensure_ascii=False))
    return run_phase(args)


if __name__ == '__main__':
    sys.exit(main())
