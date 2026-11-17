#!/usr/bin/env python3
"""Actual CLI/MCP/native-chat workflow boundaries against an isolated API.

No real models, root, container runtime, production credentials or app mutations.
The unfinished app is intentionally never marked HTTP-healthy. Temporary evidence
is retained so a failing check can be inspected rather than erased.
"""
import json,os,pathlib,select,socket,subprocess,sys,tempfile,time
SW=str(pathlib.Path(sys.argv[1]).resolve(strict=True))
assert os.geteuid()!=0
root=pathlib.Path(tempfile.mkdtemp(prefix='sw-workflow-check-'));root.chmod(0o700)
with socket.socket() as probe:probe.bind(('127.0.0.1',0));listen='127.0.0.1:'+str(probe.getsockname()[1])
config=root/'etc';work=root/'work.json';checks=[];clients=[]
def check(label,condition):
    assert condition,label
    checks.append(label);print('PASS '+label,flush=True)
def cli(*args,fail=False):
    p=subprocess.run([SW,*args],capture_output=True,text=True,timeout=35)
    assert (p.returncode!=0)==fail,p.stdout+p.stderr
    return p.stdout
server=subprocess.Popen([SW,'server','--runtime=none','--listen='+listen,'--config-dir='+str(config),'--data-dir='+str(root/'data')],stdout=subprocess.DEVNULL,stderr=(root/'server.log').open('w'))
class MCP:
    def __init__(self,write=True):
        self.p=subprocess.Popen([SW,'mcp','--workflow='+str(work)]+(['--allow-write'] if write else []),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=(root/('mcp-'+str(len(clients))+'.log')).open('w'),text=True)
        self.seq=0;clients.append(self.p)
        self.rpc('initialize',dict(protocolVersion='2025-11-25',capabilities={},clientInfo=dict(name='check',version='1')))
        self.p.stdin.write(json.dumps(dict(jsonrpc='2.0',method='notifications/initialized'))+'\n');self.p.stdin.flush()
    def rpc(self,method,params):
        self.seq+=1;self.p.stdin.write(json.dumps(dict(jsonrpc='2.0',id=self.seq,method=method,params=params))+'\n');self.p.stdin.flush()
        assert select.select([self.p.stdout],[],[],35)[0],'MCP timeout'
        return json.loads(self.p.stdout.readline())['result']
    def call(self,action,**kwargs):return self.rpc('tools/call',dict(name='sw_workflow',arguments=dict(action=action,**kwargs)))
try:
    for _ in range(80):
        if (config/'admin.token').exists():break
        assert server.poll() is None;time.sleep(.1)
    connection=json.loads(cli('connect','--workflow='+str(work),'--config-dir='+str(config),'--listen='+listen,'--name=web','--image=nginx:alpine','--expect-body=welcome'))
    check('connect emits usable fixed MCP config',connection['connected'] and connection['mcpServers']['shearwater']['args']==['mcp','--workflow='+str(work),'--allow-write'])
    artifact=root/'release-image.json';artifact.write_text(json.dumps({'image':'nginx:1.28'}))
    staged=json.loads(cli('workflow','stage-image','--file='+str(work),'--image-file='+str(artifact)))
    check('staging is neither approval nor apply',staged['staged'] and not staged['applied'] and not staged['approved'] and not json.loads(work.read_text()).get('pendingPlan'))
    artifact.write_text(json.dumps({'image':'nginx:later'}))
    check('changing build output cannot change selected image',json.loads(work.read_text())['stagedUpdateImage']=='nginx:1.28')
    m=MCP();check('single workflow tool',len(m.rpc('tools/list',{})['tools'])==1)
    check('retyped update image rejected before planning',m.call('update',image='nginx:wrong')['isError'] and not json.loads(work.read_text()).get('pendingPlan'))
    check('arbitrary target rejected',m.call('status',name='another')['isError'])
    check('no model approval',m.call('approve')['isError'])
    ro=MCP(False);check('read-only cannot execute',ro.call('continue')['isError'])
    planned=m.call('deploy',image='nginx:alpine')['structuredContent'];plan=planned['plan']
    check('plan only',planned['state']=='awaiting_approval' and not planned['applied'])
    check('unapproved continuation waits',m.call('continue')['structuredContent']['state']=='awaiting_approval')
    cli('workflow','stage-image','--file='+str(work),'--image-file='+str(artifact),fail=True)
    check('pending plan prevents staging another image',json.loads(work.read_text())['stagedUpdateImage']=='nginx:1.28')
    preview=json.loads(cli('workflow','review','--file='+str(work)));check('full review has both targets',len(preview['actions'])==2)
    cli('workflow','approve','--file='+str(work),'--confirm=wrong',fail=True)
    check('wrong approval leaves no receipt',not (pathlib.Path(str(work)+'.plans')/(plan+'.receipt.json')).exists())
    cli('workflow','approve','--file='+str(work),'--confirm='+plan)
    check('verification cannot skip pending write',m.call('verify')['structuredContent']['state']=='approved_pending')
    # Write with the normal local CLI and restart the MCP. This simulates a
    # client stopping after a complete write, before saving workflow progress.
    cli('apply','--config-dir='+str(config),'--listen='+listen,'--plan-dir='+str(work)+'.plans','--plan='+plan)
    resumed=MCP();verification=resumed.call('verify')['structuredContent']
    check('restart recovers receipt goal',bool(verification['uid']) and verification['goalGeneration']==1)
    check('API-only server never fakes HTTP success',verification['state']!='healthy' and verification['http']=='not_checked')
    check('unverified image cannot be a recovery target',resumed.call('recover')['isError'])
    # Native chat must print the actual tool JSON even if the model mock
    # returns invented prose. This is machinery testing, not an AI score.
    chat=subprocess.Popen([SW,'chat','--workflow='+str(work),'--model=qwen2.5:latest','--model-stdio','--prompt=check status'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=(root/'chat.log').open('w'),text=True)
    clients.append(chat);output=[];requests=0
    for line in chat.stdout:
        frame=json.loads(line)
        if frame['type']=='output':output.append(frame['text']);continue
        requests+=1
        message=dict(role='assistant',content='invented-success')
        if requests==1:message=dict(role='assistant',content='',tool_calls=[dict(function=dict(name='sw_workflow',arguments=dict(action='status')))])
        chat.stdin.write(json.dumps(dict(done=True,message=message))+'\n');chat.stdin.flush()
    check('native chat reports tool evidence, not invented model success',chat.wait(timeout=5)==0 and 'invented-success' not in ''.join(output) and '[Shearwater 확인 결과]' in ''.join(output))
    # The host must stop at the real saved approval boundary, even if a model
    # asks for another tool in the same response or would keep looping later.
    chat=subprocess.Popen([SW,'chat','--workflow='+str(work),'--allow-write','--model=qwen2.5:latest','--model-stdio','--prompt=prepare update'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=(root/'handoff.log').open('w'),text=True)
    clients.append(chat);output=[];requests=0
    for line in chat.stdout:
        frame=json.loads(line)
        if frame['type']=='output':output.append(frame['text']);continue
        requests+=1
        message=dict(role='assistant',content='',tool_calls=[dict(function=dict(name='sw_workflow',arguments=dict(action=a))) for a in ('update','continue')])
        chat.stdin.write(json.dumps(dict(done=True,message=message))+'\n');chat.stdin.flush()
    saved=json.loads(work.read_text());actual=''.join(output)
    check('native host stops at saved local approval without a second model request',chat.wait(timeout=5)==0 and requests==1 and 'local_approval' in actual and '[Shearwater 도구' not in actual and saved['pendingPlan'] in actual and not (pathlib.Path(str(work)+'.plans')/(saved['pendingPlan']+'.receipt.json')).exists())
finally:
    for p in clients:
        if p.poll() is None:
            p.stdin.close()
            try:p.wait(timeout=5)
            except subprocess.TimeoutExpired:p.terminate();p.wait(timeout=5)
    server.terminate();server.wait(timeout=10)
    (root/'result.json').write_text(json.dumps(dict(checks=checks,processesStopped=all(p.poll() is not None for p in clients),serverExit=server.returncode),indent=2))
print(json.dumps(dict(passed=len(checks),evidence=str(root))))
