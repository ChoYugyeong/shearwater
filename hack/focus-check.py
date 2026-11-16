#!/usr/bin/env python3
"""집중 MCP와 실제 터미널 승인을 격리 API에서 검사한다.

표준 라이브러리만 사용한다. 컨테이너·운영 인증·실제 AI는 사용하지 않는다.
모의 모델이 잘못된 도구를 요청해도 실행되지 않는지 제품의 실제 입구를 거친다.
종료 때 이 스크립트가 만든 자식 프로세스만 멈추고 증거 파일은 남긴다.
"""
import http.server
import json
import os
from pathlib import Path
import pty
import select
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

binary = str(Path(sys.argv[1]).resolve(strict=True))
cache = Path.home()/'.cache/shearwater-dev'
cache.mkdir(parents=True, exist_ok=True)
root = Path(tempfile.mkdtemp(prefix='focus-check-', dir=cache))
os.chmod(root, 0o700)
with socket.socket() as s:
    s.bind(('127.0.0.1', 0))
    listen = '127.0.0.1:' + str(s.getsockname()[1])
connection = ['--config-dir='+str(root/'config'), '--listen='+listen]
children, checks, transcript = [], [], bytearray()
log = (root/'server.log').open('w')
api = subprocess.Popen([binary, 'server', '--runtime=none', '--data-dir='+str(root/'data'), *connection], stdout=log, stderr=log)
children.append(api)
model = thread = None
master = slave = None
passed = False


def check(ok, name):
    assert ok, name
    checks.append(name)


def request(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request('https://'+listen+path, data=data, method=method,
        headers={'Authorization':'Bearer '+(root/'config/admin.token').read_text().strip(), 'Content-Type':'application/json'})
    with urllib.request.urlopen(req, context=ctx, timeout=5) as response:
        return json.load(response)


class MCP:
    def __init__(self, task='inspect', extra=()):
        args = [binary, 'mcp', *connection, '--target=deploy/web', '-n=bench', '--task='+task, *extra]
        if task != 'inspect':
            args += ['--allow-write']
        self.p = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True)
        children.append(self.p)
        self.seq = 0
        self.call('initialize', {'protocolVersion':'2025-11-25','capabilities':{},'clientInfo':{'name':'focus-check','version':'1'}})
        self.p.stdin.write(json.dumps(dict(jsonrpc='2.0',method='notifications/initialized'))+'\n')
        self.p.stdin.flush()

    def call(self, method, params):
        self.seq += 1
        self.p.stdin.write(json.dumps(dict(jsonrpc='2.0', id=self.seq, method=method, params=params))+'\n')
        self.p.stdin.flush()
        assert select.select([self.p.stdout], [], [], 10)[0], 'MCP response timeout'
        result = json.loads(self.p.stdout.readline())
        assert result.get('id') == self.seq and 'error' not in result, result
        return result['result']

    def tool(self, name, arguments):
        return self.call('tools/call', dict(name=name, arguments=arguments))

    def close(self):
        self.p.stdin.close()
        assert self.p.wait(timeout=10) == 0


def content(result):
    assert not result.get('isError'), result
    obj = json.loads(result['content'][0]['text'])
    assert result['structuredContent'] == obj
    return obj


def until(expected):
    deadline, start = time.monotonic()+20, len(transcript)
    while expected.encode() not in transcript[start:]:
        assert time.monotonic() < deadline, ('terminal timeout', expected, transcript.decode(errors='replace'))
        if select.select([master], [], [], .5)[0]:
            transcript.extend(os.read(master, 65536))


try:
    for _ in range(100):
        try:
            ctx = ssl.create_default_context(cafile=str(root/'config/ca.crt'))
            with urllib.request.urlopen('https://'+listen+'/readyz', context=ctx, timeout=1):
                break
        except OSError:
            time.sleep(.1)
    else:
        raise RuntimeError('isolated API did not start')
    request('POST', '/api/v1/namespaces', dict(apiVersion='v1',kind='Namespace',metadata={'name':'bench'}))
    path = '/apis/apps/v1/namespaces/bench/deployments/web'
    obj = request('POST', path.rsplit('/',1)[0], dict(apiVersion='apps/v1',kind='Deployment',metadata={'name':'web','namespace':'bench'},
        spec={'replicas':1,'selector':{'matchLabels':{'app':'web'}},'template':{'metadata':{'labels':{'app':'web'}},'spec':{
            'containers':[{'name':'app','image':'nginx:alpine','env':[{'name':'SECRET','value':'SYNTHETIC-PRIVATE'}]}]}}}))
    obj['status'] = dict(observedGeneration=1, replicas=1, updatedReplicas=1, readyReplicas=1, availableReplicas=1)
    obj = request('PUT', path+'/status', obj)
    m = MCP()
    check([t['name'] for t in m.call('tools/list', {})['tools']] == ['sw_target_status'], 'MCP actual catalog has one status tool')
    status = content(m.tool('sw_target_status', {}))
    check(status['state']=='met' and status['uid']==obj['metadata']['uid'] and status['http']=='not_checked', 'current object evidence and API readiness')
    check('SYNTHETIC-PRIVATE' not in json.dumps(status), 'status does not expose environment')
    check(m.tool('sw_get', {'resource':'deploy/web','namespace':'default'})['isError'], 'hidden global tool rejected')
    check(m.tool('sw_target_status', {'namespace':'default'})['isError'], 'namespace override rejected')
    obj['spec']['replicas'] = 2
    obj = request('PUT', path, obj)
    check(content(m.tool('sw_target_status', {}))['state']=='stale', 'generation cannot redefine original goal')
    m.close()
    m = MCP('image')
    check({t['name'] for t in m.call('tools/list', {})['tools']}=={'sw_target_status','sw_target_image'}, 'image session has exactly two actual tools')
    plan = content(m.tool('sw_target_image', {'image':'nginx:stable'}))
    check(not plan['applied'] and request('GET',path)==obj, 'plan does not modify API')
    check(m.tool('sw_apply', {'plan':plan['plan']})['isError'], 'model cannot apply saved plan')
    request('DELETE',path,{'gracePeriodSeconds':0})
    missing = content(m.tool('sw_target_status', {}))
    check(missing['state']=='unknown' and missing['uid']=='' and missing['generation']==0, 'missing response never echoes goal as evidence')
    obj.pop('status',None)
    obj['metadata'] = {'name':'web','namespace':'bench'}
    obj = request('POST', path.rsplit('/',1)[0], obj)
    check(content(m.tool('sw_target_status', {}))['state']=='stale', 'same-name replacement is stale')
    check(m.tool('sw_target_image', {'image':'nginx:stable'})['isError'], 'replacement cannot be planned in old session')
    m.close()

    # 실제 가상 터미널에서 /review → /approve를 입력한다. 모의 모델은
    # 계획만 제안하며, 승인은 모델의 추가 호출 없이 CLI가 직접 실행해야 한다.
    model_calls, saved_plan = [], []
    class Model(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            model_calls.append(payload)
            names = {t['function']['name'] for t in payload['tools']}
            assert names == {'sw_target_status','sw_target_image'}
            last = payload['messages'][-1]
            message = dict(role='assistant', content='')
            if last['role']=='user':
                message['tool_calls']=[dict(function=dict(name='sw_target_image',arguments={'image':'nginx:stable'}))]
            else:
                result = json.loads(last['content'])
                saved_plan.append(result['plan'])
                message['content']='PLAN_READY'
            body = json.dumps(dict(done=True,message=message)).encode()
            self.send_response(200)
            self.send_header('Content-Length',str(len(body)))
            self.end_headers()
            self.wfile.write(body)
    model = http.server.HTTPServer(('127.0.0.1',0),Model)
    thread = threading.Thread(target=model.serve_forever,daemon=True)
    thread.start()
    command = [binary,'chat',*connection,'--model=scripted','--endpoint=http://127.0.0.1:'+str(model.server_port),
               '--target=deploy/web','-n=bench','--task=image','--allow-write']
    master,slave = pty.openpty()
    chat = subprocess.Popen(command,stdin=slave,stdout=slave,stderr=slave,close_fds=True)
    children.append(chat)
    until('/quit')
    os.write(master,b'plan image\n')
    until('PLAN_READY')
    plan_id = saved_plan[-1]
    approval = root/'config/plans'/f'{plan_id}.approved.json'
    check(not approval.exists(),'focused model creates no approval')
    os.write(master,b'/approve\n')
    until('interactive write mode')
    check(not approval.exists(),'focused approval requires review')
    os.write(master,('/review '+'a'*64+'\n').encode())
    until('only plans created in this session')
    os.write(master,b'/review\n')
    until('트랜잭션')
    calls_before = len(model_calls)
    os.write(master,b'/approve\n')
    until('"verification"')
    receipt = json.loads((root/'config/plans'/f'{plan_id}.receipt.json').read_text())
    current = request('GET',path)
    check(receipt['state']=='complete' and len(receipt['goals'])==1, 'focused TTY apply writes one exact receipt goal')
    check(current['spec']['template']['spec']['containers'][0]['image']=='nginx:stable', 'human approval applies image')
    check(len(model_calls)==calls_before, 'focused approval never gives apply to model')
    os.write(master,b'/quit\n')
    check(chat.wait(timeout=10)==0,'focused chat closes normally')
    passed=True
finally:
    for p in reversed(children):
        if p.poll() is None:
            p.terminate()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait(timeout=5)
    if master is not None:
        os.close(master)
        os.close(slave)
    if model:
        model.shutdown()
        model.server_close()
        thread.join(timeout=5)
    log.close()
    (root/'terminal.txt').write_bytes(transcript)
    (root/'report.json').write_text(json.dumps(dict(passed=passed,checks=checks,count=len(checks),
        model='scripted, not actual AI',runtime='none',childrenStopped=all(p.poll() is not None for p in children)),indent=2))
print('PASS',len(checks),'focused checks; evidence:',root)
