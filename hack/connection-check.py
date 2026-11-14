#!/usr/bin/env python3
"""Real TCP continuity checks. Run in an ISOLATED root network namespace.

No HTTP retry is performed: every failed request remains a failure in JSON.
All workload files live in a new --root on Linux ext4 and are retained. The
caller provides an otherwise empty Shearwater containerd namespace; never use
this against a node hosting real applications. See docs/connection-continuity.md.
"""
import argparse
import collections
import hashlib
import http.client
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import threading
import time
import traceback

p = argparse.ArgumentParser(description=__doc__)
for name in ['sw', 'kubectl', 'containerd-socket', 'root', 'run', 'out']:
    p.add_argument('--' + name, required=True)
p.add_argument('--port', type=int, default=16463)
p.add_argument('--quick', action='store_true')
p.add_argument('--extended-only', action='store_true')
p.add_argument('--soak-seconds', type=int, default=180)
args = p.parse_args()
assert os.geteuid() == 0, 'root in an isolated test namespace is required'
root, run_dir, out = map(Path, (args.root, args.run, args.out))
assert root.is_absolute() and run_dir.is_absolute() and out.is_absolute()
assert not root.exists() and not run_dir.exists(), 'choose NEW test directories'
root.mkdir(parents=True, mode=0o700)
out.mkdir(parents=True, exist_ok=True)
flags = ['--config-dir=' + str(root/'config'), '--data-dir=' + str(root/'data'),
         '--run-dir=' + str(run_dir), '--log-dir=' + str(root/'log'),
         '--listen=127.0.0.1:' + str(args.port), '--containerd-socket=' + args.containerd_socket,
         '--node-name=connection-check']
kflags = [args.kubectl, '--kubeconfig=' + str(root/'config/kubeconfig'), '--request-timeout=15s']
ctr = ['ctr', '--address', args.containerd_socket, '--namespace', 'shearwater']
# Go's elapsed timers use CLOCK_MONOTONIC on Linux. MONOTONIC_RAW can advance
# at a different rate while a WSL guest clock is being corrected. Mixing them
# made a real 30-second drain appear to end in about 27 seconds. Use the same
# clock for deadlines, request pacing and reported latency; retain both clock
# samples so a later reader can identify the measurement basis.
clock = time.monotonic
def clock_sample():
    return {'monotonic':clock(), 'raw':time.clock_gettime(time.CLOCK_MONOTONIC_RAW), 'wall':time.time()}

body = b'x'*1023 + b'\n'
report = {'passed': False, 'phases': [], 'actions': [], 'httpRetries': 0,
          'clock': {'duration':'CLOCK_MONOTONIC', 'start':clock_sample()},
          'binarySHA256': hashlib.sha256(Path(args.sw).read_bytes()).hexdigest()}

def save():
    # Evidence is small and written only by the coordinator. A bounded Windows
    # sharing-violation retry is FILE I/O only; never retry an HTTP request.
    temp = out/'results.json.tmp'
    temp.write_text(json.dumps(report, indent=2) + '\n')
    until = time.monotonic()+2
    while True:
        try: temp.replace(out/'results.json'); return
        except PermissionError:
            if time.monotonic() >= until: raise
            time.sleep(.05)

def cmd(argv, timeout=180):
    r = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    if r.returncode:
        raise RuntimeError(str(argv) + '\n' + r.stdout[-2000:] + r.stderr[-2000:])
    return r.stdout

def sw(*words): return cmd([args.sw, *words, *flags])
def k(*words): return cmd([*kflags, *words])
def data(kind): return json.loads(k('get', kind, '-n', 'continuity', '-o', 'json'))['items']
def apply(name, value):
    path = root/(name + '.json')
    path.write_text(json.dumps(value))
    return k('apply', '-f', str(path))

def signature():
    return sorted((v['metadata']['uid'], v['status'].get('podIP'),
                   tuple((c.get('containerID'), c.get('restartCount')) for c in v['status'].get('containerStatuses', [])))
                  for v in data('pods'))

revision = 0
def deploy(n):
    global revision
    revision += 1
    config = ('worker_processes 1; pid /tmp/nginx.pid; error_log /dev/stderr warn; '
              'events { worker_connections 1024; } http { access_log off; server { listen 8080; '
              'location = /health { return 200 "ok\\n"; } '
              'location = /stream.bin { root /continuity; limit_rate 64k; } '
              'location / { return 200 "' + 'x'*1023 + '\\n"; } } }')
    manifest = {'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':'web','namespace':'continuity'}, 'spec':{
        'replicas':n,'selector':{'matchLabels':{'app':'continuity'}},
        'strategy':{'type':'RollingUpdate','rollingUpdate':{'maxSurge':1,'maxUnavailable':0}},
        'template':{'metadata':{'labels':{'app':'continuity'},'annotations':{'test.shearwater/revision':str(revision)}},
          'spec':{'terminationGracePeriodSeconds':1,
            'volumes':[{'name':'stream','hostPath':{'path':str(root/'content'),'type':'Directory'}}],
            'containers':[{'name':'web','image':'docker.io/library/nginx:1.27',
              'command':['sh','-c','printf %s '+shlex.quote(config)+' > /tmp/continuity.conf; exec nginx -c /tmp/continuity.conf -g "daemon off;"'],
              'volumeMounts':[{'name':'stream','mountPath':'/continuity','readOnly':True}],
              'resources':{'limits':{'cpu':'500m','memory':'64Mi'}},
              'readinessProbe':{'httpGet':{'path':'/health','port':8080},'periodSeconds':1,'timeoutSeconds':2,'failureThreshold':3}}]}}}}
    start = clock()
    apply('deployment-' + str(revision), manifest)
    k('rollout','status','deployment/web','-n','continuity','--timeout=170s')
    report['actions'].append({'kind':'rollout','revision':revision,'replicas':n,'seconds':clock()-start})
    save()

def measure(name, seconds, concurrency=8, rps=80, action=None):
    """Reuse exactly 32 requests/socket, same as the alpha.4 baseline."""
    start = clock(); deadline = start + seconds
    results = []; failures = []; lock = threading.Lock(); samples=[]; sampling=threading.Event()
    def sampler():
        while not sampling.is_set():
            try:
                pid=json.loads((run_dir/'server.json').read_text())['pid']
                proc=Path('/proc')/str(pid)
                stat=(proc/'stat').read_text().rsplit(')',1)[1].split()
                status=dict(line.split(':',1) for line in (proc/'status').read_text().splitlines() if ':' in line)
                samples.append({'at':clock()-start,'identity':str(pid)+':'+stat[19],
                    'rssKiB':int(status.get('VmRSS','0 kB').split()[0]),
                    'cpuSeconds':(int(stat[11])+int(stat[12]))/os.sysconf('SC_CLK_TCK')})
            except (OSError,ValueError,KeyError): pass # server may intentionally be absent
            sampling.wait(1)
    collector=threading.Thread(target=sampler);collector.start()
    def worker(index):
        success = 0; errors = []; latencies = []; connection = None; used = 0
        due = start + index/rps
        while clock() < deadline:
            while clock() < min(due, deadline): time.sleep(min(.02, max(0, min(due,deadline)-clock())))
            if clock() >= deadline: break
            before = clock()
            try:
                if connection is None: connection = http.client.HTTPConnection(vip, 80, timeout=2)
                connection.request('GET','/',headers={'Connection':'keep-alive'})
                response=connection.getresponse(); got=response.read()
                assert response.status==200 and got==body, 'wrong status/body'
                success += 1; used += 1; latencies.append(clock()-before)
                if used == 32: connection.close(); connection=None; used=0
            except Exception as e:
                errors.append({'at':clock()-start,'error':repr(e)})
                if connection: connection.close()
                connection=None; used=0
            due=max(due+concurrency/rps,clock())
        if connection: connection.close()
        with lock: results.append((success, errors, latencies))
    def actor():
        try: action(start)
        except Exception: failures.append(traceback.format_exc())
    threads=[threading.Thread(target=worker,args=(i,)) for i in range(concurrency)]
    if action: threads.append(threading.Thread(target=actor))
    for t in threads:t.start()
    for t in threads:t.join()
    sampling.set();collector.join()
    latency=sorted(x for r in results for x in r[2])
    row={'name':name,'seconds':clock()-start,'success':sum(r[0] for r in results),
         'errors':[e for r in results for e in r[1]],'concurrency':concurrency,'targetRPS':rps,
         'p95ms':latency[max(0,int(len(latency)*.95)-1)]*1000 if latency else None,
         'actionFailures':failures}
    row['swResourceSamples']=samples
    row['swMaxRSSMiB']=max((s['rssKiB'] for s in samples),default=0)/1024
    report['phases'].append(row);save();print(json.dumps({a:b for a,b in row.items() if a!='swResourceSamples'}),flush=True)
    assert not row['errors'] and not failures and row['success']>0, name+' failed'

def wait_until(start, offset):
    while clock()<start+offset: time.sleep(min(.1,start+offset-clock()))

def restart(mode, downtime=5):
    before=signature(); t=clock()
    if mode=='kill':
        state=json.loads((run_dir/'server.json').read_text())
        pid=state['pid']
        assert str(Path('/proc')/str(pid)) != '/proc/1'
        assert Path('/proc',str(pid),'exe').resolve()==Path(args.sw).resolve()
        os.kill(pid,signal.SIGKILL)
    else: sw('down')
    # Traffic keeps running while there is NO sw process, including new
    # connections. This tests forwarding independence, not fast process startup.
    time.sleep(downtime)
    sw('up')
    k('rollout','status','deployment/web','-n','continuity','--timeout=60s')
    assert signature()==before, 'restart changed Pod/container/IP/restartCount'
    report['actions'].append({'kind':'restart-'+mode,'seconds':clock()-t,'identitiesPreserved':True});save()

def empty_endpoint_continuity():
    connection=http.client.HTTPConnection(vip,80,timeout=2)
    connection.request('GET','/');r=connection.getresponse();assert r.read()==body
    identity=connection.sock.getsockname()
    try:
        k('patch','service','web','-n','continuity','--type=merge','-p',json.dumps({'spec':{'selector':{'app':'temporarily-no-endpoints'}}}))
        until=clock()+10
        while 'shearwater:backend:' in cmd(['iptables','-t','nat','-S','SHEARWATER-SERVICES']):
            assert clock()<until;time.sleep(.05)
        for _ in range(100):
            connection.request('GET','/');r=connection.getresponse();assert r.status==200 and r.read()==body
            assert connection.sock.getsockname()==identity
        new=http.client.HTTPConnection(vip,80,timeout=2)
        rejected=False
        try: new.request('GET','/');new.getresponse()
        except (ConnectionRefusedError,ConnectionResetError): rejected=True
        finally:new.close()
        assert rejected,'empty Service reached a host listener'
        report['actions'].append({'kind':'empty-endpoints','sameConnectionRequests':100,'newConnectionRejected':True});save()
    finally:
        connection.close()
        k('patch','service','web','-n','continuity','--type=merge','-p',json.dumps({'spec':{'selector':{'app':'continuity'}}}))
        until=clock()+10
        while 'shearwater:backend:' not in cmd(['iptables','-t','nat','-S','SHEARWATER-SERVICES']):
            assert clock()<until;time.sleep(.05)

def await_drain(uid):
    deadline=clock()+20
    while clock()<deadline:
        for v in data('pods'):
            if v['metadata']['uid']==uid and v['metadata'].get('annotations',{}).get('shearwater.io/connection-drain'):
                assert not v['metadata'].get('deletionTimestamp'), 'terminated before connection drain'
                return clock()
        time.sleep(.1)
    raise AssertionError('old Pod never entered drain')

def settled_single_pod():
    # Rollout readiness alone is not proof that the retired Pod has disappeared.
    # A new experiment must pin its connection to the one current Pod, rather
    # than accidentally timing an already draining Pod from the previous test.
    deadline=clock()+90
    while clock()<deadline:
        pods=data('pods')
        if len(pods)==1:
            pod=pods[0];meta=pod['metadata'];annotations=meta.get('annotations',{})
            if (not meta.get('deletionTimestamp') and not annotations.get('shearwater.io/connection-drain')
                    and annotations.get('test.shearwater/revision')==str(revision)
                    and any(c['type']=='Ready' and c['status']=='True' for c in pod['status'].get('conditions',[]))):
                return meta['uid']
        time.sleep(.1)
    raise AssertionError('previous rollout did not settle to one current Ready Pod')

def slow_download_during_rollout():
    deploy(1);old=settled_single_pod()
    connection=http.client.HTTPConnection(vip,80,timeout=3)
    connection.request('GET','/stream.bin');response=connection.getresponse()
    assert response.status==200 and response.getheader('Content-Length')==str(1024*1024)
    errors=[];received=[];roll_errors=[];started_at=clock()
    def read():
        try: received.append(response.read())
        except Exception:errors.append(traceback.format_exc())
        finally:connection.close()
    def roll():
        try:deploy(1)
        except Exception:roll_errors.append(traceback.format_exc())
    reader=threading.Thread(target=read);roller=threading.Thread(target=roll)
    reader.start();roller.start();await_drain(old)
    before=signature()
    sw('down');time.sleep(3);sw('up')
    assert signature()==before,'restart during drain replaced a live container'
    reader.join(25);roller.join(50)
    assert not reader.is_alive() and not roller.is_alive()
    assert not errors and not roll_errors,(errors,roll_errors)
    assert received==[b's'*(1024*1024)],'slow response truncated or corrupted'
    assert old not in [v['metadata']['uid'] for v in data('pods')]
    report['actions'].append({'kind':'slow-download-roll-and-restart','bytes':len(received[0]),
        'sha256':hashlib.sha256(received[0]).hexdigest(),'seconds':clock()-started_at,'errors':0});save()

def bounded_drain():
    deploy(1);old=settled_single_pod()
    connection=http.client.HTTPConnection(vip,80,timeout=2)
    connection.request('GET','/');r=connection.getresponse();assert r.read()==body
    # Events belong to their Pod and are garbage-collected with it. Watch
    # BEFORE retirement instead of listing after rollout has removed the Pod.
    # Keep the raw watch output as evidence that the warning really occurred.
    events_path=out/'drain-timeout-events.txt'
    event_output=events_path.open('w')
    event_errors=(out/'drain-timeout-watch.stderr').open('w')
    watcher=subprocess.Popen([*kflags[:-1], '--request-timeout=60s', 'get', 'events',
        '-n', 'continuity', '--field-selector=involvedObject.uid='+old,
        '--watch', '--output-watch-events', '-o', 'jsonpath={.object.reason}{"\\n"}'],
        stdout=event_output, stderr=event_errors)
    errors=[]
    def roll():
        try:deploy(1)
        except Exception:errors.append(traceback.format_exc())
    roller=threading.Thread(target=roll);roller.start()
    closed=None;success=0
    try:
        start=await_drain(old)
        while clock()-start<45:
            try:
                connection.request('GET','/');r=connection.getresponse();assert r.status==200 and r.read()==body
                success+=1
            except (OSError,http.client.HTTPException) as e:closed={'afterSeconds':clock()-start,'error':repr(e)};break
            time.sleep(.1)
    finally:
        connection.close();roller.join(50)
        watcher.terminate()
        try:watcher.wait(timeout=5)
        except subprocess.TimeoutExpired:watcher.kill();watcher.wait()
        event_output.close();event_errors.close()
    assert not roller.is_alive() and not errors,errors
    assert closed and 29<=closed['afterSeconds']<=40,closed
    warnings='ConnectionDrainTimeout' in events_path.read_text().splitlines()
    assert warnings,'drain timeout missing its diagnostic event'
    report['actions'].append({'kind':'bounded-drain-expected-close','requestsBeforeClose':success,'timeoutEvent':True,**closed});save()

def recreated_service():
    before=data('services')[0]['metadata']['uid']
    connection=http.client.HTTPConnection(vip,80,timeout=2)
    connection.request('GET','/');r=connection.getresponse();assert r.read()==body
    k('delete','service','web','-n','continuity','--wait=true','--timeout=20s')
    deadline=clock()+10
    while 'shearwater:service:'+before in cmd(['iptables','-t','nat','-S','SHEARWATER-SERVICES']):
        assert clock()<deadline;time.sleep(.1)
    manifest={'apiVersion':'v1','kind':'Service','metadata':{'name':'web','namespace':'continuity'},
        'spec':{'clusterIP':vip,'selector':{'app':'no-endpoints'},'ports':[{'port':80,'targetPort':8080}]}}
    apply('recreated-service',manifest)
    time.sleep(.5)
    rejected=False
    try:connection.request('GET','/');connection.getresponse()
    except (OSError,http.client.HTTPException):rejected=True
    finally:connection.close()
    assert rejected,'old connection reached recreated Service'
    assert data('services')[0]['metadata']['uid']!=before
    manifest['spec']['selector']={'app':'continuity'};apply('restored-service',manifest)
    deadline=clock()+10
    while 'shearwater:backend:' not in cmd(['iptables','-t','nat','-S','SHEARWATER-SERVICES']):
        assert clock()<deadline;time.sleep(.05)
    report['actions'].append({'kind':'service-IP-reuse','oldConnectionRejected':True,'newUID':True});save()

assert not cmd(ctr+['tasks','list','-q']).strip(), 'QA runtime must be empty'
assert not cmd(ctr+['containers','list','-q']).strip(), 'QA runtime must be empty'
(root/'content').mkdir()
(root/'content/stream.bin').write_bytes(b's'*(1024*1024))
started=False
try:
    sw('up');started=True
    k('create','namespace','continuity')
    apply('service',{'apiVersion':'v1','kind':'Service','metadata':{'name':'web','namespace':'continuity'},
        'spec':{'selector':{'app':'continuity'},'ports':[{'port':80,'targetPort':8080}]}})
    deploy(1 if args.quick else 3)
    vip=data('services')[0]['spec']['clusterIP']
    time.sleep(2)
    def normal_restart(start): wait_until(start,3);restart('term')
    measure('restart-term',15,action=normal_restart)
    empty_endpoint_continuity()
    if not args.quick:
        def doctor_during_load(start):
            wait_until(start,3)
            result=sw('doctor')
            assert 'FAIL ' not in result
            (out/'doctor.txt').write_text(result)
        measure('doctor-under-load',10,action=doctor_during_load)
    if not args.quick and not args.extended_only:
        def killed_restart(start): wait_until(start,3);restart('kill')
        measure('restart-kill',15,action=killed_restart)
        for repeat in range(1,4):
            deploy(3)
            def recovery(start):
                wait_until(start,10)
                k('scale','deployment/web','-n','continuity','--replicas=10')
                k('rollout','status','deployment/web','-n','continuity','--timeout=90s')
                wait_until(start,40);deploy(10)
                wait_until(start,80);restart('term')
            measure('baseline-recovery-'+str(repeat),120,action=recovery)
        def busy_roll(start): wait_until(start,5);deploy(10)
        measure('rolling-1000rps',60,32,1000,busy_roll)
    if not args.quick:
        slow_download_during_rollout()
        bounded_drain()
        recreated_service()
        deploy(10)
        def repeated_restarts(start):
            for n,offset in enumerate(range(20,args.soak_seconds-10,30)):
                wait_until(start,offset);restart('kill' if n%2 else 'term')
        measure('soak-repeated-restarts',args.soak_seconds,8,100,repeated_restarts)
    report['passed']=True
except Exception:
    report['failure']=traceback.format_exc();print(report['failure'],flush=True)
finally:
    if started:
        try:
            # down --all only stops this fixture's resources and leaves all
            # SQLite, hostPath contents, configuration and logs for inspection.
            sw('down','--all')
            assert not cmd(ctr+['tasks','list','-q']).strip()
            assert not cmd(ctr+['containers','list','-q']).strip()
            report['cleanup']=True
        except Exception: report['cleanupFailure']=traceback.format_exc()
    report['clock']['end']=clock_sample()
    save()
raise SystemExit(0 if report['passed'] and report.get('cleanup') else 1)
