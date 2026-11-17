#!/usr/bin/env python3
"""Check one-shot approved execution through a real API and scripted model.

모델은 이 검사에서 의도한 도구 호출만 내는 대역이다. 실제 모델 성능 시험과
구분한다. API/SQLite/계획/승인/영수증은 실제 sw를 쓰되 runtime=none인 임시
서버만 만든다. 운영 인증·컨테이너·root는 사용하지 않고 결과 파일은 남긴다.
"""
import http.server
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

binary = str(Path(sys.argv[1]).resolve(strict=True))
fixture = Path(tempfile.mkdtemp(prefix='chat-resume-', dir=Path.home()/'.cache/shearwater-dev'))
os.chmod(fixture, 0o700)
with socket.socket() as probe:
    probe.bind(('127.0.0.1', 0))
    listen = '127.0.0.1:'+str(probe.getsockname()[1])
connection = ['--config-dir='+str(fixture/'config'), '--listen='+listen]
requests = []
phase = 'catalog'
plan = None
model_errors = []


class Model(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        requests.append(request)
        try:
            names = {t['function']['name'] for t in request['tools']}
            message = dict(role='assistant', content='CHECKED')
            if phase == 'catalog':
                assert 'sw_apply' not in names and 'sw_app_plan' in names
            else:
                assert names == {'sw_apply', 'sw_receipt', 'sw_verify_plan'}
                for tool in request['tools']:
                    assert tool['function']['parameters']['properties']['plan']['enum'] == [plan]
                last = request['messages'][-1]
                if last['role'] == 'user':
                    message['content'] = ''
                    message['tool_calls'] = [dict(function=dict(name='sw_apply', arguments=dict(plan=plan)))]
                elif last['tool_name'] == 'sw_apply' and phase == 'execute':
                    receipt = json.loads(last['content'])
                    assert receipt['state'] == 'complete'
                    message['content'] = ''
                    message['tool_calls'] = [dict(function=dict(name='sw_verify_plan', arguments=dict(plan=plan)))]
                elif phase == 'execute':
                    assert json.loads(last['content'])['state'] != 'met'
                else:
                    assert 'Tool error:' in last['content']
        except Exception as exc:
            model_errors.append(repr(exc))
            message = dict(role='assistant', content='SCRIPTED_MODEL_ASSERTION_FAILED')
        body = json.dumps(dict(done=True, message=message)).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def run(*args):
    return subprocess.run([binary, *args, *connection], text=True, capture_output=True, timeout=30)


log = (fixture/'server.log').open('w')
api = subprocess.Popen([binary, 'server', '--runtime=none', '--data-dir='+str(fixture/'data'), *connection], stdout=log, stderr=log)
model = http.server.HTTPServer(('127.0.0.1', 0), Model)
thread = threading.Thread(target=model.serve_forever, daemon=True)
thread.start()
passed = False
try:
    for attempt in range(100):
        try:
            ctx = ssl.create_default_context(cafile=str(fixture/'config/ca.crt'))
            with urllib.request.urlopen('https://'+listen+'/readyz', context=ctx, timeout=1):
                break
        except OSError:
            time.sleep(.1)
    else:
        raise AssertionError('isolated API not ready')
    command = ['chat', '--provider=ollama', '--model=scripted-test',
               '--endpoint=http://127.0.0.1:'+str(model.server_port), '--allow-write', '--prompt=check']
    ordinary = run(*command)
    assert ordinary.returncode == 0 and requests and not model_errors
    planned = run('app-plan', '--action=create', '--name=resume-web', '--image=nginx:alpine', '--port=80')
    assert planned.returncode == 0, planned.stderr
    plan = json.loads(planned.stdout)['plan']
    resume = command + ['--approved-plan='+plan]
    count = len(requests)
    unapproved = run(*resume)
    assert unapproved.returncode != 0 and len(requests) == count
    assert 'prior local review' in unapproved.stderr
    review = run('plan', '--plan='+plan)
    assert review.returncode == 0 and len(json.loads(review.stdout)['actions']) == 2
    approved = run('approve', '--plan='+plan, '--confirm='+plan)
    assert approved.returncode == 0, approved.stderr
    phase = 'execute'
    executed = run(*resume)
    (fixture/'execute.txt').write_text(executed.stdout+executed.stderr)
    assert executed.returncode == 0 and not model_errors, model_errors
    receipt_path = fixture/'config/plans'/(plan+'.receipt.json')
    receipt = receipt_path.read_bytes()
    assert json.loads(receipt)['state'] == 'complete' and len(json.loads(receipt)['goals']) == 2
    # A new process must not bypass executePlan's durable one-application guard.
    phase = 'repeat'
    repeated = run(*resume)
    assert repeated.returncode == 0 and not model_errors, model_errors
    assert receipt_path.read_bytes() == receipt
    passed = True
finally:
    model.shutdown()
    model.server_close()
    thread.join(timeout=5)
    api.terminate()
    api.wait(timeout=10)
    log.close()
    (fixture/'model-requests.json').write_text(json.dumps(requests, indent=2))
    (fixture/'report.json').write_text(json.dumps(dict(passed=passed,
        checks=['ordinary one-shot hides apply', 'missing approval rejects before model',
                'three tools bound to exact hash', 'approved application stores original goals',
                'readiness stays unverified without runtime', 'second process cannot reapply'],
        model='scripted, not actual AI', errors=model_errors, fixture=str(fixture),
        cleanup='owned API/model stopped; files retained'), indent=2))
print('PASS 6 one-shot resume checks; evidence: '+str(fixture))
