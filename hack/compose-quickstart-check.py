#!/usr/bin/env python3
"""Follow the README's Docker Compose quick start on a real Docker host.

This is the "can a stranger get from nothing to a verified update" check. It
runs the README's commands in order as an ordinary operator account in the
`docker` group, on a host that has Docker and the `docker compose` plugin and
nothing from Shearwater (a fresh GitHub-hosted runner, a new VPS):

1. install `sw` with the README's installer block (or use --sw for a local build);
2. start a small Compose project the operator already runs (`web`, `other`);
3. `sw compose serve` in front of it and `sw task create` for `web`;
4. play the agent through the real `sw mcp --workflow` stdio server;
5. review, approve and read evidence with the README's `sw task` commands.

The agent part is scripted JSON-RPC, not a language model: this checks the
path and the guarantees, not model behavior. Scenarios:

* update to v2: stops at awaiting_approval, runs after approval, healthy;
* update to an image that answers HTTP 200 without the expected text: it is
  approved and applied, but the server reports http_failed, never healthy;
* recover: the server picks the last verified image (v2) and it verifies.

Images are built locally and pushed to a throwaway registry on 127.0.0.1 so
every image has an immutable digest, as with a real registry. Results go to
--out: timings.json, transcript.json (the MCP calls, for the demo), and
summary.md. Exit status is non-zero on the first broken guarantee.

    python3 hack/compose-quickstart-check.py --release v0.1.0-alpha.14 --out /tmp/qs
    python3 hack/compose-quickstart-check.py --sw dist/sw --out /tmp/qs
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.request

BASE_IMAGE = 'mirror.gcr.io/library/nginx@sha256:30f1c0d78e0ad60901648be663a710bdadf19e4c10ac6782c235200619158284'
REGISTRY = '127.0.0.1:5055'
LISTEN = '127.0.0.1:16444'
PROJECT = 'shop'
CONF = Path.home() / '.config' / 'shearwater-compose'
HANDOFF = Path('/srv/sw-handoff')
PAGES = {
    'v1': 'Welcome to the shop, v1',
    'v2': 'Welcome to the shop, v2',
    # Answers 200 like a healthy release, but lost the text the operator asked
    # the check to look for: the kind of break a status code alone misses.
    'broken': 'Maintenance page',
}

started = time.monotonic()
timings, transcript, steps = {}, [], []


def log(msg):
    print(f'[{time.monotonic() - started:6.1f}s] {msg}', flush=True)


def mark(name):
    timings[name] = round(time.monotonic() - started, 2)
    log(f'== {name}')


def run(args, check=True, timeout=600, **kw):
    p = subprocess.run([str(a) for a in args], text=True, capture_output=True, timeout=timeout, **kw)
    if check and p.returncode:
        raise SystemExit(f'command failed ({p.returncode}): {" ".join(map(str, args))}\n{p.stdout[-2000:]}{p.stderr[-3000:]}')
    return p


def expect(cond, message, **detail):
    steps.append({'check': message, 'ok': bool(cond), **({} if cond else {'detail': detail})})
    if not cond:
        log(f'FAILED: {message} {json.dumps(detail)[:1500]}')
        raise SystemExit(message)
    log(f'ok: {message}')


class Agent:
    """The stdio MCP server an AI client starts from the README's step 4."""

    def __init__(self, sw, work):
        self.p = subprocess.Popen([sw, 'mcp', f'--workflow={work}', '--allow-write'], stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.n = 0
        init = self.rpc('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {},
                                       'clientInfo': {'name': 'compose-quickstart-check', 'version': '1'}})
        self.p.stdin.write(json.dumps({'jsonrpc': '2.0', 'method': 'notifications/initialized'}) + '\n')
        self.p.stdin.flush()
        self.tools = [t['name'] for t in self.rpc('tools/list', {})['tools']]
        self.server = init.get('serverInfo')

    def rpc(self, method, params):
        self.n += 1
        self.p.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': self.n, 'method': method, 'params': params}) + '\n')
        self.p.stdin.flush()
        line = self.p.stdout.readline()
        if not line:
            raise SystemExit('MCP server exited: ' + self.p.stderr.read()[-3000:])
        reply = json.loads(line)
        if 'error' in reply:
            raise SystemExit(f'MCP error: {reply}')
        return reply['result']

    def call(self, scene, **arguments):
        t = time.monotonic()
        r = self.rpc('tools/call', {'name': self.tools[0], 'arguments': arguments})
        text = r['content'][0]['text']
        try:
            value = json.loads(text)
        except json.JSONDecodeError:
            value = {'text': text}
        value['_isError'] = r.get('isError', False)
        transcript.append({'scene': scene, 'who': 'agent', 'request': arguments, 'response': value,
                           'seconds': round(time.monotonic() - t, 2)})
        log(f'agent {arguments} -> {value.get("state")} {"(error)" if value["_isError"] else ""}')
        return value

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=10)


def operator(sw, scene, *args):
    p = run([sw, 'task', *args, '--config-dir', CONF, '--listen', LISTEN], check=False)
    value = json.loads(p.stdout) if p.returncode == 0 and p.stdout.strip().startswith('{') else {'error': (p.stdout + p.stderr).strip()[-1500:]}
    transcript.append({'scene': scene, 'who': 'operator', 'request': ['sw', 'task', *map(str, args)], 'response': value})
    return p.returncode, value


def page(url='http://127.0.0.1:18080/'):
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url, timeout=3) as r:
        return r.status, r.read().decode(errors='replace')


def install(release, bindir):
    """The README's installer block, unchanged except for the version."""
    script = f'''set -eu
version={release}
release="https://github.com/ChoYugyeong/shearwater/releases/download/$version"
download_dir=$(mktemp -d)
cd "$download_dir"
curl -fsSLO "$release/install.py"
curl -fsSLO "$release/SHA256SUMS"
sha256sum --check --strict --ignore-missing SHA256SUMS
python3 install.py --version "$version" --apply
"$HOME/.local/share/shearwater/$version/sw" version
'''
    p = run(['bash', '-c', script])
    sw = Path.home() / '.local/share/shearwater' / release / 'sw'
    expect(sw.is_file() and p.stdout.strip().splitlines()[-1].startswith('sw ' + release.lstrip('v')),
           'README installer installs and verifies sw', output=p.stdout[-500:])
    bindir.mkdir(parents=True, exist_ok=True)
    return str(sw)


def build_images(work):
    """v1, v2 and broken, pushed to a local registry so each has a digest."""
    run(['docker', 'run', '-d', '--name', 'swqs-registry', '-p', f'{REGISTRY}:5000', 'mirror.gcr.io/library/registry:2'])
    for _ in range(50):
        try:
            urllib.request.build_opener(urllib.request.ProxyHandler({})).open(f'http://{REGISTRY}/v2/', timeout=1)
            break
        except Exception:
            time.sleep(.2)
    digests = {}
    for tag, text in PAGES.items():
        ctx = work / f'image-{tag}'
        ctx.mkdir(parents=True)
        (ctx / 'index.html').write_text(text + '\n')
        (ctx / 'Dockerfile').write_text(f'FROM {BASE_IMAGE}\nCOPY index.html /usr/share/nginx/html/index.html\n')
        ref = f'{REGISTRY}/shop-web:{tag}'
        run(['docker', 'build', '-q', '-t', ref, ctx])
        run(['docker', 'push', '-q', ref])
        digest = json.loads(run(['docker', 'image', 'inspect', ref]).stdout)[0]['RepoDigests'][0]
        digests[tag] = digest
    return digests


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument('--release', help='install this release with the README installer, e.g. v0.1.0-alpha.14')
    src.add_argument('--sw', help='use this sw binary instead of installing')
    ap.add_argument('--out', required=True)
    ap.add_argument('--work', default='', help='scratch directory (default: a new temporary one)')
    a = ap.parse_args()
    out = Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    work = Path(a.work or tempfile.mkdtemp(prefix='swqs-')).resolve()
    work.mkdir(parents=True, exist_ok=True)
    for tool in ('docker', 'curl', 'sha256sum'):
        if not shutil.which(tool):
            raise SystemExit(f'{tool} is required')
    if CONF.exists():
        raise SystemExit(f'{CONF} already exists; run this on a host where Shearwater was never set up')
    server = None
    agent = None
    try:
        mark('start')
        sw = install(a.release, work / 'bin') if a.release else str(Path(a.sw).resolve())
        version = run([sw, 'version']).stdout.strip()
        mark('installed')

        digests = build_images(work)
        project = work / 'shop'
        project.mkdir()
        compose_file = project / 'compose.yaml'
        compose_file.write_text(f'''services:
  web:
    image: {digests['v1']}
    ports: ["127.0.0.1:18080:80"]
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1/"]
      interval: 2s
      timeout: 1s
      retries: 3
  other:
    image: {digests['v1']}
''')
        run(['docker', 'compose', '-f', compose_file, '-p', PROJECT, 'up', '-d', '--wait'])
        other_id = run(['docker', 'compose', '-f', compose_file, '-p', PROJECT, 'ps', '-q', 'other']).stdout.strip()
        expect(page()[1].startswith(PAGES['v1']), 'operator already runs the app with plain Compose')
        mark('existing_app_running')

        # README step 2.
        server_log = (out / 'server.log').open('w')
        server = subprocess.Popen([sw, 'compose', 'serve', '--file', compose_file, '--project', PROJECT, '--listen', LISTEN],
                                  stdout=server_log, stderr=subprocess.STDOUT)
        for _ in range(150):
            if (CONF / 'ca.crt').exists():
                try:
                    urllib.request.urlopen(f'https://{LISTEN}/healthz', timeout=1,
                                           context=ssl.create_default_context(cafile=str(CONF / 'ca.crt')))
                    break
                except Exception:
                    pass
            if server.poll() is not None:
                raise SystemExit('sw compose serve exited; see server.log')
            time.sleep(.2)
        else:
            raise SystemExit('sw compose serve did not answer')
        mark('serving')

        # README step 3, including the shared handoff folder.
        run(['sudo', 'install', '-d', '-o', os.environ.get('USER') or run(['id', '-un']).stdout.strip(), '-m', '0755', HANDOFF])
        code, grant = operator(sw, 'delegate', 'create', '-n', PROJECT, '--name', 'web', '--port', '80',
                               '--allow', 'update,recover', '--expect-body', 'Welcome', '--out', HANDOFF / 'web', '--ttl', '2h')
        expect(code == 0 and grant.get('operation'), 'operator delegates one service', result=grant)
        operation = grant['operation']
        mark('delegated')

        # README step 4.
        agent = Agent(sw, HANDOFF / 'web' / 'work.json')
        expect(agent.tools, 'agent connects over MCP', tools=agent.tools)
        v = agent.call('connect', action='verify')
        expect(v.get('state') == 'healthy', 'agent verifies the existing app', result=v)

        def approve(scene, plan):
            code, review = operator(sw, scene, 'review', f'--id={operation}')
            expect(code == 0 and review.get('plan') == plan, 'operator review shows the agent plan', review=review)
            code, ok = operator(sw, scene, 'approve', f'--id={operation}', f'--confirm={plan}')
            expect(code == 0 and ok.get('approved'), 'operator approves the exact plan hash', result=ok)

        # Scenario 1: a good update (README step 5).
        u = agent.call('update', action='update', image=digests['v2'])
        expect(u.get('state') == 'awaiting_approval' and not u.get('applied'), 'update stops for approval', result=u)
        expect(page()[1].startswith(PAGES['v1']), 'nothing changed before approval')
        early = agent.call('update', action='continue')
        expect(early.get('_isError') or early.get('state') == 'awaiting_approval', 'continue before approval does not run', result=early)
        approve('update', u['plan'])
        c = agent.call('update', action='continue')
        expect(c.get('state') == 'healthy' and c.get('applied'), 'approved update applies and verifies', result=c)
        expect(page()[1].startswith(PAGES['v2']), 'v2 is live')
        code, ev = operator(sw, 'update', 'evidence', f'--id={operation}')
        expect(code == 0, 'operator reads HTTP evidence', result=ev)
        expect(run(['docker', 'compose', '-f', compose_file, '-p', PROJECT, 'ps', '-q', 'other']).stdout.strip() == other_id,
               'the other service was not touched')
        mark('first_verified_update')

        # Scenario 2: an update that answers 200 but lost the expected text.
        b = agent.call('broken', action='update', image=digests['broken'])
        expect(b.get('state') == 'awaiting_approval', 'broken update stops for approval', result=b)
        approve('broken', b['plan'])
        bc = agent.call('broken', action='continue')
        status, body = page()
        expect(status == 200 and body.startswith(PAGES['broken']), 'broken release really answers HTTP 200', body=body[:80])
        expect(bc.get('state') == 'http_failed' and bc.get('state') != 'healthy',
               'server refuses to call a 200 without the expected text healthy', result=bc)
        bv = agent.call('broken', action='verify')
        expect(bv.get('state') != 'healthy', 'verify still does not report healthy', result=bv)
        d = agent.call('broken', action='diagnose')
        mark('broken_update_caught')

        # Scenario 3: recover to the last verified image.
        r = agent.call('recover', action='recover')
        expect(r.get('state') == 'awaiting_approval', 'recover stops for approval', result=r)
        approve('recover', r['plan'])
        rc = agent.call('recover', action='continue')
        expect(rc.get('state') == 'healthy' and page()[1].startswith(PAGES['v2']), 'recovery returns to the verified v2', result=rc)
        mark('recovered')

        summary = {
            'passed': True, 'sw': version, 'docker': run(['docker', 'version', '--format', '{{.Server.Version}}']).stdout.strip(),
            'compose': run(['docker', 'compose', 'version', '--short']).stdout.strip(),
            'host': os.uname().nodename, 'kernel': os.uname().release, 'timings_s': timings, 'images': digests,
            'diagnose_facts': d.get('facts'),
        }
    except BaseException as e:
        summary = {'passed': False, 'error': str(e)[-2000:], 'timings_s': timings}
        raise
    finally:
        if agent:
            try:
                agent.close()
            except Exception:
                pass
        if server and server.poll() is None:
            server.terminate()
            server.wait(timeout=15)
        (out / 'timings.json').write_text(json.dumps(timings, indent=2))
        (out / 'transcript.json').write_text(json.dumps(transcript, indent=2))
        (out / 'steps.json').write_text(json.dumps(steps, indent=2))
        (out / 'summary.json').write_text(json.dumps(summary, indent=2))
        lines = ['# Compose quick start check', '', f"Result: **{'passed' if summary.get('passed') else 'FAILED'}**", '']
        for k, v in timings.items():
            lines.append(f'- {k}: {v} s')
        lines += ['', f'{sum(s["ok"] for s in steps)}/{len(steps)} checks passed.']
        if not summary.get('passed'):
            lines += ['', '```', summary.get('error', ''), '```']
        (out / 'summary.md').write_text('\n'.join(lines) + '\n')


if __name__ == '__main__':
    main()
