#!/usr/bin/env python3
"""Exercise real terminal approval with a scripted model and an isolated API.

This checks the client/approval machinery, not a real model's intelligence. No
root, containers, production credentials, downloads or external model calls.
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
fixture = Path(tempfile.mkdtemp(prefix='chat-', dir=Path.home()/'.cache/shearwater-dev'))
os.chmod(fixture, 0o700)
with socket.socket() as probe:
    probe.bind(('127.0.0.1', 0))
    listen = '127.0.0.1:' + str(probe.getsockname()[1])
connection = ['--config-dir='+str(fixture/'config'), '--listen='+listen]
plan = None
class Model(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_POST(self):
        global plan
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        last = request['messages'][-1]
        message = dict(role='assistant', content='')
        if last['role'] == 'user':
            name = 'sw_apply' if 'locally reviewed and approved' in last['content'] else 'sw_app_plan'
            args = dict(plan=plan) if name == 'sw_apply' else dict(action='create', name='chat-web', image='nginx:alpine', port=80)
            message['tool_calls'] = [dict(function=dict(name=name, arguments=args))]
        else:
            result = json.loads(last['content'])
            if 'intent' in result:
                plan = result['plan']
                message['content'] = 'PLAN_READY'
            else:
                assert result['verification']['state'] != 'met'
                message['content'] = 'APPLIED_BUT_NOT_READY'
        body = json.dumps(dict(done=True, message=message)).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

log = (fixture/'server.log').open('w')
api = subprocess.Popen([binary, 'server', '--runtime=none', '--data-dir='+str(fixture/'data'), *connection], stdout=log, stderr=log)
model = http.server.HTTPServer(('127.0.0.1',0), Model)
thread = threading.Thread(target=model.serve_forever, daemon=True)
thread.start()
command = [binary, 'chat', '--provider=ollama', '--model=scripted-test',
           '--endpoint=http://127.0.0.1:'+str(model.server_port), '--allow-write', *connection]
master, slave = pty.openpty()
chat = None
transcript = bytearray()
passed = False
def until(expected):
    deadline = time.monotonic()+30
    start = len(transcript)
    while expected.encode() not in transcript[start:]:
        assert time.monotonic()<deadline, ('timeout',expected,transcript.decode(errors='replace'))
        if select.select([master],[],[],1)[0]:
            transcript.extend(os.read(master,65536))
    return transcript[start:].decode(errors='replace')
try:
    for _ in range(100):
        try:
            ctx=ssl.create_default_context(cafile=str(fixture/'config/ca.crt'))
            with urllib.request.urlopen('https://'+listen+'/readyz',context=ctx,timeout=1):
                break
        except OSError:
            time.sleep(.1)
    chat = subprocess.Popen(command, stdin=slave, stdout=slave, stderr=slave, close_fds=True)
    until('/quit')
    os.write(master,b'plan a web app\n')
    until('PLAN_READY')
    assert plan
    approval = fixture/'config/plans'/f'{plan}.approved.json'
    assert not approval.exists(), 'model created its own approval'
    os.write(master,b'/approve\n')
    until('interactive write mode')
    assert not approval.exists(), 'approved before review'
    os.write(master,b'/review\n')
    until('트랜잭션')
    # Same exact plan over an ordinary pipe must still fail the TTY gate.
    piped = subprocess.run(command,input='/review '+plan+'\n/approve\n/quit\n',
                           text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=20)
    assert not approval.exists() and 'interactive write mode' in piped.stdout
    os.write(master,b'/approve\n')
    until('APPLIED_BUT_NOT_READY')
    receipt=json.loads((fixture/'config/plans'/f'{plan}.receipt.json').read_text())
    assert receipt['state']=='complete' and len(receipt['goals'])==2
    assert approval.exists()
    os.write(master,b'/quit\n')
    assert chat.wait(timeout=10)==0
    passed=True
finally:
    if chat and chat.poll() is None:
        chat.terminate()
        chat.wait(timeout=10)
    os.close(master)
    os.close(slave)
    model.shutdown()
    model.server_close()
    thread.join(timeout=5)
    api.terminate()
    api.wait(timeout=10)
    log.close()
    (fixture/'terminal.txt').write_bytes(transcript)
    (fixture/'report.json').write_text(json.dumps(dict(passed=passed,model='scripted, not actual AI',
        checks=['no model approval','approval requires review','pipe cannot approve','TTY short commands bind exact plan','apply binds goals and reports not ready'],
        fixture=str(fixture),cleanup='test children stopped; files retained'),indent=2))
print('PASS terminal approval checks; evidence: '+str(fixture))
