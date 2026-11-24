#!/usr/bin/env python3
"""Same-host runtime comparison: Docker Compose, K3s, minikube and Shearwater.

This is the portable form of the 2026-11-17 measurement behind
docs/benchmarks/runtime-comparison-alpha9.md. It needs root on a disposable
Linux host with cgroup v2 (a GitHub-hosted runner, a throwaway VM). It does
not touch the host's Docker, containerd or Kubernetes:

* every tool runs in a private network namespace (`swbench`) with one veth
  uplink and two owned iptables chains for NAT;
* every tool runs in its own cgroup with the same CPU/memory cap. The
  engine, containerd, shims, apps and helper pods are all charged to it. The
  load generator, collector and CLIs stay outside the cgroup;
* only one tool runs at a time. The order rotates between repeats.

While it runs it sets a few kernel values that kubelet otherwise sets itself
(vm.overcommit_memory=1, kernel.panic=10, ...) so all tools see the same
kernel, and it restores them when it finishes.

    sudo python3 hack/bench_runtime.py run --yes --out OUT --sw dist/sw \\
        --loadgen dist/benchload --tool-dir TOOLS
    python3 hack/bench_runtime.py summarize OUT

Tools are looked up in --tool-dir first, then PATH: `dockerd`, `containerd`,
`ctr`, `runc`, `docker`, `docker-compose`, `k3s`, `minikube`, `kubectl`.
Results are per phase raw JSONL plus summary.csv / summary.json / summary.md.
A failed tool keeps its records and, with --keep-going, does not stop the
others. Nothing is retried silently.
"""
import argparse
import collections
import csv
import json
import os
from pathlib import Path
import re
import shutil
import signal
import statistics
import subprocess
import sys
import threading
import time
import traceback

TOOLS = ('compose', 'k3s', 'minikube', 'shearwater')
DEFAULT_IMAGE = 'docker.io/library/nginx@sha256:30f1c0d78e0ad60901648be663a710bdadf19e4c10ac6782c235200619158284'
NETNS = 'swbench'
HOST_LINK = 'swb-host0'
PEER_LINK = 'swb-peer0'
HOST_IP = '192.0.2.1'
NS_IP = '192.0.2.2'
FWD_CHAIN = 'SWBENCH-FWD'
NAT_CHAIN = 'SWBENCH-NAT'
COMMENT = 'shearwater-runtime-bench'
CGROUP_PREFIX = 'swbench-'
RUN = '/run/swbench'
API_PORT = 16462
LABEL = {'bench': 'runtime'}
BODY_SIZE = 1024
# kubelet sets these unless --protect-kernel-defaults; minikube's kubelet runs
# privileged and would change them for the whole host mid-run. Setting them
# once for every tool keeps the kernel identical across the comparison.
KERNEL = {
    'vm.overcommit_memory': '1',
    'vm.panic_on_oom': '0',
    'kernel.panic': '10',
    'kernel.panic_on_oops': '1',
    'kernel.keys.root_maxkeys': '1000000',
    'kernel.keys.root_maxbytes': '25000000',
    # The namespace's NAT uplink needs forwarding on the host.
    'net.ipv4.ip_forward': '1',
}
MARKER = '.swbench-owned'


# ---------------------------------------------------------------- workload

def workload(n, version, image):
    """Compose services and Kubernetes objects for n independent apps.

    Ten apps are ten one-replica deployments with one address each, not one
    ten-replica Service. Every app returns exactly BODY_SIZE bytes that start
    with its version, so the load generator can verify each response.
    """
    body = version + ' ' + 'x' * (BODY_SIZE - 3)
    conf = ('worker_processes 1; error_log /dev/stderr warn; pid /tmp/nginx.pid; '
            'events { worker_connections 256; } http { access_log off; server { listen 8080; '
            'location /health { return 200 "ok"; } location / { root /usr/share/nginx/html; } } }')
    command = (f"printf '%s' '{body}' > /usr/share/nginx/html/index.html; "
               f"printf '%s' '{conf}' > /etc/nginx/nginx.conf; exec nginx -g 'daemon off;'")
    check = ['wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:8080/health']
    services, objects = {}, []
    for i in range(n):
        name = f'web{i:02d}'
        labels = dict(LABEL, app=name)
        services[name] = {
            'image': image, 'pull_policy': 'never', 'entrypoint': ['/bin/sh', '-c'], 'command': [command],
            'ports': [f'127.0.0.1:{18000 + i}:8080'], 'cpus': 0.5, 'mem_limit': '64m',
            'stop_grace_period': '5s', 'restart': 'always', 'labels': labels,
            'healthcheck': {'test': ['CMD', *check], 'interval': '5s', 'timeout': '1s', 'retries': 3},
        }
        objects.append({
            'apiVersion': 'apps/v1', 'kind': 'Deployment',
            'metadata': {'name': name, 'namespace': 'default', 'labels': labels},
            'spec': {
                'replicas': 1, 'selector': {'matchLabels': labels},
                'strategy': {'type': 'RollingUpdate', 'rollingUpdate': {'maxUnavailable': 0, 'maxSurge': 1}},
                'template': {'metadata': {'labels': labels}, 'spec': {
                    'terminationGracePeriodSeconds': 5,
                    'containers': [{
                        'name': 'web', 'image': image, 'imagePullPolicy': 'Never',
                        'command': ['/bin/sh', '-c', command],
                        'resources': {'limits': {'cpu': '500m', 'memory': '64Mi'}},
                        'readinessProbe': {'exec': {'command': check}, 'periodSeconds': 5,
                                           'timeoutSeconds': 1, 'failureThreshold': 3},
                    }]}},
            },
        })
        objects.append({
            'apiVersion': 'v1', 'kind': 'Service',
            'metadata': {'name': name, 'namespace': 'default', 'labels': labels},
            'spec': {'selector': labels, 'ports': [{'port': 80, 'targetPort': 8080, 'protocol': 'TCP'}]},
        })
    return {'services': services}, {'apiVersion': 'v1', 'kind': 'List', 'items': objects}


def write_workload(folder, n, version, image):
    compose, kube = workload(n, version, image)
    folder.mkdir(parents=True, exist_ok=True)
    (folder / f'compose-{version}.json').write_text(json.dumps(compose, indent=2))
    (folder / f'kube-{version}.json').write_text(json.dumps(kube, indent=2))


# ---------------------------------------------------------------- analysis

def quantile(xs, p):
    if not xs:
        return None
    s = sorted(xs)
    at = (len(s) - 1) * p
    i = int(at)
    return s[i] + (s[min(i + 1, len(s) - 1)] - s[i]) * (at - i)


def jsonl(path):
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def error_kind(text):
    low = text.lower()
    if 'deadline exceeded' in low or 'timeout' in low:
        return 'timeout'
    for label in ('connection refused', 'connection reset by peer', 'EOF',
                  'generator-concurrency-limit', 'wrong-body', 'http-status'):
        if label in text:
            return label
    return text[:160]


def summarize_case(folder, tool, trial):
    """Numbers for one tool/apps/repeat case, computed from preserved raw rows."""
    result = {
        'tool': tool, 'trial': trial, 'apps': int(folder.name.split('-')[1]),
        'deploy_s': json.loads((folder / 'deploy.json').read_text())['seconds'],
        'update_s': json.loads((folder / 'update.json').read_text())['seconds'],
    }
    for phase in ('idle', 'load', 'update'):
        rows = jsonl(folder / f'{phase}-resources.jsonl')
        first, last = rows[0], rows[-1]
        span = last['monotonic'] - first['monotonic']
        stats = {
            'samples': len(rows), 'span_s': span,
            'memory_mib': statistics.median(r['memory_current'] for r in rows) / 2**20,
            'peak_memory_mib': max(r['memory_current'] for r in rows) / 2**20,
            'pss_mib': statistics.median(r['pss'] for r in rows) / 2**20,
            'peak_pss_mib': max(r['pss'] for r in rows) / 2**20,
            'anon_mib': statistics.median(r['memory_stat'].get('anon', 0) for r in rows) / 2**20,
            'file_mib': statistics.median(r['memory_stat'].get('file', 0) for r in rows) / 2**20,
            'kernel_mib': statistics.median(r['memory_stat'].get('kernel', 0) for r in rows) / 2**20,
            'cpu_one_core_percent': (last['cpu']['usage_usec'] - first['cpu']['usage_usec']) / 1e6 / span * 100 if span else None,
            'throttled_s': (last['cpu'].get('throttled_usec', 0) - first['cpu'].get('throttled_usec', 0)) / 1e6,
            'oom_kills': last['memory_events'].get('oom_kill', 0),
            'phase_memory_max_hits': last['memory_events'].get('max', 0) - first['memory_events'].get('max', 0),
        }
        if phase != 'idle':
            req = jsonl(folder / f'{phase}-requests.jsonl')
            ok = [r for r in req if not r.get('error')]
            stats.update(
                requests=len(req), successful=len(ok), errors=len(req) - len(ok),
                error_types=dict(collections.Counter(error_kind(r['error']) for r in req if r.get('error'))),
                versions=dict(collections.Counter(r['version'] for r in ok)),
                p50_ms=quantile([r['latency_ms'] for r in ok], .5),
                p95_ms=quantile([r['latency_ms'] for r in ok], .95),
                p99_ms=quantile([r['latency_ms'] for r in ok], .99),
                late_over_5ms=sum(r['dispatch_lag_ms'] > 5 for r in req),
                dispatch_p95_ms=quantile([r['dispatch_lag_ms'] for r in req], .95),
            )
        result[phase] = stats
    return result


def flatten(row):
    flat = {k: row[k] for k in ('tool', 'trial', 'apps', 'deploy_s', 'update_s')}
    for phase in ('idle', 'load', 'update'):
        for k, v in row[phase].items():
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                flat[f'{phase}_{k}'] = v
    return flat


def fmt(values, digits=2):
    values = [v for v in values if v is not None]
    if not values:
        return 'n/a'
    med = statistics.median(values)
    if len(values) == 1:
        return f'{med:.{digits}f}'
    return f'{med:.{digits}f} [{min(values):.{digits}f}–{max(values):.{digits}f}]'


# Order of rows in the Markdown table: the assignment's three first.
TABLE_ORDER = ('minikube', 'k3s', 'shearwater', 'compose')
TABLE_NAMES = {'minikube': 'minikube', 'k3s': 'K3s', 'shearwater': 'Shearwater', 'compose': 'Docker Compose'}


def markdown(rows, failures, meta):
    lines = ['# Runtime comparison', '']
    if meta:
        lines += [f"- {k}: `{v}`" for k, v in meta.items()] + ['']
    lines += ['Each cell is the median [min–max] over repeats; update errors are summed. '
              'A shared CI runner makes latency noisy: read memory, deploy time and update errors first. '
              'This is not a product ranking: the tools cover different features.', '']
    for apps in sorted({r['apps'] for r in rows}):
        lines += [f'## {apps} app' + ('s' if apps != 1 else ''), '',
                  '| Tool | Repeats | Process PSS, MiB | cgroup, MiB | Load CPU, 1 core=100% | Deploy, s | Load p95, ms | Update errors/requests |',
                  '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
        for tool in TABLE_ORDER:
            group = [r for r in rows if r['tool'] == tool and r['apps'] == apps]
            if not group:
                continue
            errors = sum(r['update']['errors'] for r in group)
            requests = sum(r['update']['requests'] for r in group)
            lines.append('| ' + ' | '.join([
                TABLE_NAMES[tool], str(len(group)),
                fmt([r['idle']['pss_mib'] for r in group]),
                fmt([r['idle']['memory_mib'] for r in group]),
                fmt([r['load']['cpu_one_core_percent'] for r in group]),
                fmt([r['deploy_s'] for r in group]),
                fmt([r['load']['p95_ms'] for r in group]),
                f'{errors:,}/{requests:,}',
            ]) + ' |')
        lines.append('')
    oom = [f"{r['tool']} apps={r['apps']} {r['trial']}" for r in rows
           if any(r[p]['oom_kills'] or r[p]['phase_memory_max_hits'] for p in ('idle', 'load', 'update'))]
    lines.append('Cases that hit the memory cap or an OOM kill: ' + (', '.join(oom) if oom else 'none') + '.')
    if failures:
        lines += ['', '## Failed', ''] + [f'- {f}' for f in failures]
    return '\n'.join(lines) + '\n'


def summarize(out):
    out = Path(out)
    rows, failures = [], []
    for trial in sorted(p for p in out.iterdir() if p.is_dir() and re.fullmatch(r'r\d+-[a-z0-9]+', p.name)):
        tool = trial.name.split('-', 1)[1]
        for folder in sorted(trial.glob('apps-*')):
            if (folder / 'completed.json').exists():
                rows.append(summarize_case(folder, tool, trial.name))
            else:
                failures.append(f'{trial.name}/{folder.name}: incomplete')
        for f in sorted(trial.glob('failure-*.txt')):
            failures.append(f'{trial.name}: ' + f.read_text().strip().splitlines()[-1][:300])
    meta = json.loads((out / 'run.json').read_text()).get('meta', {}) if (out / 'run.json').exists() else {}
    (out / 'summary.json').write_text(json.dumps({'cases': rows, 'failures': failures}, indent=2))
    flat = [flatten(r) for r in rows]
    fields = list(dict.fromkeys(k for r in flat for k in r)) or ['tool', 'trial', 'apps']
    with (out / 'summary.csv').open('w', newline='') as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        w.writerows(flat)
    (out / 'summary.md').write_text(markdown(rows, failures, meta))
    return rows, failures


# ---------------------------------------------------------------- host side

def sh(args, check=True, **kw):
    return subprocess.run([str(a) for a in args], check=check, text=True, **kw)


def find_tool(name, tool_dir):
    if tool_dir and (Path(tool_dir) / name).is_file():
        return str(Path(tool_dir) / name)
    found = shutil.which(name)
    if not found:
        raise SystemExit(f'{name} not found in --tool-dir or PATH')
    return found


def network_up():
    if sh(['ip', 'netns', 'list'], capture_output=True).stdout.split().count(NETNS):
        raise SystemExit(f'network namespace {NETNS} already exists; refusing to adopt it')
    if sh(['ip', 'link', 'show', HOST_LINK], check=False, capture_output=True).returncode == 0:
        raise SystemExit(f'link {HOST_LINK} already exists; refusing to adopt it')
    for table, chain in (('filter', FWD_CHAIN), ('nat', NAT_CHAIN)):
        if sh(['iptables', '-w', '5', '-t', table, '-S', chain], check=False, capture_output=True).returncode == 0:
            raise SystemExit(f'iptables chain {chain} already exists; refusing to adopt it')
    sh(['ip', 'netns', 'add', NETNS])
    sh(['ip', 'link', 'add', HOST_LINK, 'type', 'veth', 'peer', 'name', PEER_LINK])
    sh(['ip', 'link', 'set', PEER_LINK, 'netns', NETNS])
    sh(['ip', 'addr', 'add', f'{HOST_IP}/30', 'dev', HOST_LINK])
    sh(['ip', 'link', 'set', HOST_LINK, 'up'])
    sh(['ip', '-n', NETNS, 'link', 'set', 'lo', 'up'])
    sh(['ip', '-n', NETNS, 'link', 'set', PEER_LINK, 'name', 'eth0'])
    sh(['ip', '-n', NETNS, 'addr', 'add', f'{NS_IP}/30', 'dev', 'eth0'])
    sh(['ip', '-n', NETNS, 'link', 'set', 'eth0', 'up'])
    sh(['ip', '-n', NETNS, 'route', 'add', 'default', 'via', HOST_IP])
    sh(['ip', 'netns', 'exec', NETNS, 'sysctl', '-qw', 'net.ipv4.ip_forward=1'])
    ipt = ['iptables', '-w', '5']
    sh(ipt + ['-N', FWD_CHAIN])
    sh(ipt + ['-A', FWD_CHAIN, '-i', HOST_LINK, '-s', f'{NS_IP}/32', '-j', 'ACCEPT'])
    sh(ipt + ['-A', FWD_CHAIN, '-o', HOST_LINK, '-d', f'{NS_IP}/32', '-m', 'conntrack',
              '--ctstate', 'ESTABLISHED,RELATED', '-j', 'ACCEPT'])
    sh(ipt + ['-I', 'FORWARD', '1', '-m', 'comment', '--comment', COMMENT, '-j', FWD_CHAIN])
    sh(ipt + ['-t', 'nat', '-N', NAT_CHAIN])
    sh(ipt + ['-t', 'nat', '-A', NAT_CHAIN, '-s', f'{NS_IP}/32', '-j', 'MASQUERADE'])
    sh(ipt + ['-t', 'nat', '-A', 'POSTROUTING', '-s', f'{NS_IP}/32', '-m', 'comment', '--comment', COMMENT, '-j', NAT_CHAIN])


def network_down():
    ipt = ['iptables', '-w', '5']
    sh(ipt + ['-D', 'FORWARD', '-m', 'comment', '--comment', COMMENT, '-j', FWD_CHAIN], check=False, capture_output=True)
    sh(ipt + ['-t', 'nat', '-D', 'POSTROUTING', '-s', f'{NS_IP}/32', '-m', 'comment', '--comment', COMMENT, '-j', NAT_CHAIN],
       check=False, capture_output=True)
    for table, chain in (('filter', FWD_CHAIN), ('nat', NAT_CHAIN)):
        sh(ipt + ['-t', table, '-F', chain], check=False, capture_output=True)
        sh(ipt + ['-t', table, '-X', chain], check=False, capture_output=True)
    sh(['ip', 'netns', 'delete', NETNS], check=False, capture_output=True)
    sh(['ip', 'link', 'delete', HOST_LINK], check=False, capture_output=True)


def upstream_resolv():
    for p in ('/run/systemd/resolve/resolv.conf', '/etc/resolv.conf'):
        try:
            text = Path(p).read_text()
        except OSError:
            continue
        servers = [line for line in text.splitlines()
                   if line.startswith('nameserver') and not line.split()[1].startswith('127.')]
        if servers:
            return '\n'.join(servers) + '\n'
    return 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n'


def kernel_apply():
    before = {name: (Path('/proc/sys') / name.replace('.', '/')).read_text().strip() for name in KERNEL}
    try:
        for name, value in KERNEL.items():
            (Path('/proc/sys') / name.replace('.', '/')).write_text(value)
    except OSError:
        kernel_restore(before)
        raise
    return before


def kernel_restore(before):
    for name, value in before.items():
        (Path('/proc/sys') / name.replace('.', '/')).write_text(value)


def cgroup_pids(group):
    pids = set()
    for f in group.rglob('cgroup.procs'):
        try:
            pids.update(map(int, f.read_text().split()))
        except (FileNotFoundError, OSError):
            pass
    return pids


def kv(path):
    out = {}
    for line in path.read_text().splitlines():
        parts = line.split()
        if len(parts) == 2:
            out[parts[0]] = int(parts[1])
    return out


class Engine:
    """One tool's engine inside the private network namespace and cgroup."""

    def __init__(self, args, tool, trial, out):
        self.a, self.tool, self.trial = args, tool, trial
        self.out = out / trial
        self.root = Path(args.work).resolve() / trial
        self.cg = Path('/sys/fs/cgroup') / (CGROUP_PREFIX + trial)
        self.pid = None
        self.proc = None
        self.env = dict(os.environ, GOMAXPROCS=str(args.cpus), HOME=str(self.root / 'home'),
                        MINIKUBE_HOME=str(self.root / 'home' / '.minikube'),
                        DOCKER_HOST=f'unix://{RUN}/docker.sock', DOCKER_CONFIG=str(self.root / 'home' / '.docker'),
                        KUBECONFIG=str(self.kubeconfig()), COMPOSE_PARALLEL_LIMIT='10',
                        MINIKUBE_IN_STYLE='false', MINIKUBE_WANTUPDATENOTIFICATION='false')
        self.env['PATH'] = os.pathsep.join(filter(None, [args.tool_dir, self.env.get('PATH', '')]))

    def kubeconfig(self):
        return {'k3s': self.root / 'k3s' / 'kubeconfig'}.get(self.tool, self.root / 'kubeconfig')

    # -- lifecycle

    def start(self):
        if self.root.exists() or self.cg.exists() or self.out.exists():
            raise RuntimeError(f'{self.trial}: work, cgroup or output already exists')
        self.root.mkdir(parents=True, mode=0o700)
        (self.root / MARKER).write_text(self.trial)
        for sub in ('tmp', 'home'):
            (self.root / sub).mkdir()
        self.out.mkdir(parents=True)
        self.cg.mkdir()
        (self.cg / 'memory.max').write_text(str(self.a.memory_mib * 2**20))
        (self.cg / 'memory.swap.max').write_text('0')
        (self.cg / 'cpu.max').write_text(f'{self.a.cpus * 100000} 100000')
        (self.root / 'resolv.conf').write_text(upstream_resolv())
        log = (self.out / 'guardian.log').open('w')
        cg = self.cg

        def enter_cgroup():
            (cg / 'cgroup.procs').write_text(str(os.getpid()))

        self.proc = subprocess.Popen(
            ['nsenter', f'--net=/run/netns/{NETNS}', 'unshare', '--cgroup', '--mount',
             sys.executable, str(Path(__file__).resolve()), '_guardian', self.tool, str(self.root), str(self.out),
             str(self.cg), self.a.sw, self.a.tool_dir or ''],
            preexec_fn=enter_cgroup, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
            start_new_session=True, env=self.env)
        self.pid = self.proc.pid
        end = time.monotonic() + 60
        while not (self.out / 'guardian-ready.json').exists():
            if self.proc.poll() is not None or time.monotonic() > end:
                raise RuntimeError(f'{self.trial}: guardian did not start, see guardian.log')
            time.sleep(.25)

    def stop(self):
        if self.proc is not None and self.proc.poll() is None:
            os.kill(self.pid, signal.SIGTERM)
        end = time.monotonic() + 45
        while time.monotonic() < end and cgroup_pids(self.cg):
            time.sleep(.5)
        forced = sorted(cgroup_pids(self.cg))
        if forced:
            # This subtree was created by start() for this trial only, so a
            # cgroup kill cannot reach anything else; it also catches orphaned shims.
            (self.cg / 'cgroup.kill').write_text('1')
            end = time.monotonic() + 15
            while time.monotonic() < end and cgroup_pids(self.cg):
                time.sleep(.5)
        if self.cg.exists():
            for p in sorted((p for p in self.cg.rglob('*') if p.is_dir()), key=lambda p: len(p.parts), reverse=True):
                try:
                    p.rmdir()
                except OSError:
                    pass
            try:
                self.cg.rmdir()
            except OSError as e:
                print(f'{self.trial}: cgroup not removed: {e}', file=sys.stderr)
        (self.out / 'stopped.json').write_text(json.dumps({'forced': forced, 'time': time.time()}))
        if not self.a.keep_work and (self.root / MARKER).exists():
            shutil.rmtree(self.root, ignore_errors=True)

    # -- commands inside the namespace (outside the cgroup)

    def guard(self):
        if self.proc is None or self.proc.poll() is not None or (self.out / 'engine-exit.json').exists():
            raise RuntimeError(f'{self.trial}: engine exited; see engine logs')

    def cmd(self, args, timeout=180, check=True):
        self.guard()
        prefix = ['nsenter', '-t', str(self.pid), '--mount', '--net', '--cgroup', '--']
        p = subprocess.run(prefix + [str(a) for a in args], env=self.env, text=True, capture_output=True, timeout=timeout)
        if check and p.returncode:
            raise RuntimeError(f'command failed ({p.returncode}): {" ".join(map(str, args[:4]))}\n'
                               f'{p.stdout[-3000:]}{p.stderr[-5000:]}')
        return p

    def kube(self, *args, **kw):
        return self.cmd([find_tool('kubectl', self.a.tool_dir), f'--kubeconfig={self.kubeconfig()}', *args], **kw)

    def docker(self, *args, **kw):
        return self.cmd([find_tool('docker', self.a.tool_dir), f'--host=unix://{RUN}/docker.sock', *args], **kw)

    def compose(self, folder, version, *args, **kw):
        return self.cmd([find_tool('docker-compose', self.a.tool_dir), '--project-name=swbench',
                         '-f', folder / f'compose-{version}.json', *args], **kw)

    def minikube(self, *args, **kw):
        return self.cmd([find_tool('minikube', self.a.tool_dir), '--profile=swbench', *args], **kw)

    # -- measurement

    def sample(self):
        row = {'time': time.time(), 'monotonic': time.monotonic(),
               'memory_current': int((self.cg / 'memory.current').read_text()),
               'memory_stat': kv(self.cg / 'memory.stat'), 'cpu': kv(self.cg / 'cpu.stat'),
               'memory_events': kv(self.cg / 'memory.events')}
        procs = []
        for pid in sorted(cgroup_pids(self.cg)):
            d = Path('/proc') / str(pid)
            try:
                mem = {}
                for line in (d / 'smaps_rollup').read_text().splitlines()[1:]:
                    parts = line.split()
                    if len(parts) == 3:
                        mem[parts[0].rstrip(':')] = int(parts[1]) * 1024
                procs.append({'pid': pid, 'name': (d / 'comm').read_text().strip(),
                              'pss': mem.get('Pss', 0), 'rss': mem.get('Rss', 0)})
            except (FileNotFoundError, ProcessLookupError, PermissionError):
                pass
        row.update(processes=procs, pss=sum(p['pss'] for p in procs), rss=sum(p['rss'] for p in procs))
        return row

    def collect(self, folder, phase, seconds, work=None):
        stop, errors = threading.Event(), []

        def loop():
            with (folder / f'{phase}-resources.jsonl').open('w', buffering=1) as f:
                while not stop.is_set():
                    try:
                        self.guard()
                        f.write(json.dumps(self.sample()) + '\n')
                    except Exception as e:  # keep the phase failure, do not hide it
                        errors.append(repr(e))
                        break
                    stop.wait(1)

        thread = threading.Thread(target=loop)
        thread.start()
        start = time.monotonic()
        try:
            result = work() if work else None
            while time.monotonic() - start < seconds:
                self.guard()
                time.sleep(.2)
        finally:
            stop.set()
            thread.join()
        if errors:
            raise RuntimeError(f'{phase} sampling failed: {errors}')
        return result

    def endpoints(self, n):
        if self.tool == 'compose':
            return [f'http://127.0.0.1:{18000 + i}/' for i in range(n)]
        items = json.loads(self.kube('get', 'svc', '-l', 'bench=runtime', '-o', 'json').stdout)['items']
        return ['http://' + s['spec']['clusterIP'] + '/' for s in sorted(items, key=lambda x: x['metadata']['name'])]

    def http_check(self, urls, version):
        code = ('import urllib.request,sys\n'
                'o=urllib.request.build_opener(urllib.request.ProxyHandler({}))\n'
                'v=sys.argv[1].encode()\n'
                f'sys.exit(0 if all((lambda b: b[:2]==v and len(b)=={BODY_SIZE})(o.open(u,timeout=2).read()) for u in sys.argv[2:]) else 1)\n')
        try:
            return self.cmd([sys.executable, '-c', code, version, *urls], timeout=max(10, len(urls) * 3), check=False).returncode == 0
        except subprocess.TimeoutExpired:
            return False

    def ready(self, n):
        if self.tool == 'compose':
            ids = self.docker('ps', '-q', '--filter', 'label=com.docker.compose.project=swbench').stdout.split()
            if len(ids) != n:
                return False
            return all(x['State'].get('Health', {}).get('Status') == 'healthy'
                       for x in json.loads(self.docker('inspect', *ids).stdout))
        rows = json.loads(self.kube('get', 'deploy', '-l', 'bench=runtime', '-o', 'json').stdout)['items']
        return len(rows) == n and all(
            x.get('status', {}).get('observedGeneration', 0) >= x['metadata'].get('generation', 1)
            and x.get('status', {}).get('updatedReplicas') == 1
            and x.get('status', {}).get('availableReplicas', 0) >= 1
            and x.get('status', {}).get('replicas') == 1 for x in rows)

    def wait_apps(self, n, version, timeout=240):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if self.ready(n):
                urls = self.endpoints(n)
                if len(urls) == n and self.http_check(urls, version):
                    return urls
            time.sleep(.5)
        raise RuntimeError(f'{n} apps did not become {version} ready')

    def check_limits(self, n):
        """Read the kernel limits of each app's cgroup, not the submitted spec.

        Every nginx master process charged to this trial's cgroup is an app;
        this works the same for all four tools, including minikube's node
        container, because the collector sees host PIDs and cgroup paths.
        """
        masters = []
        for pid in cgroup_pids(self.cg):
            try:
                if (Path('/proc') / str(pid) / 'comm').read_text().strip() != 'nginx':
                    continue
                ppid = int((Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()[1])
                if (Path('/proc') / str(ppid) / 'comm').read_text().strip() == 'nginx':
                    continue
                relative = (Path('/proc') / str(pid) / 'cgroup').read_text().strip().split('::', 1)[1]
                masters.append((pid, relative))
            except (FileNotFoundError, ProcessLookupError, IndexError):
                pass
        checked = []
        for pid, relative in masters:
            if not relative.startswith('/' + self.cg.name + '/'):
                raise RuntimeError(f'app {pid} is outside the trial cgroup: {relative}')
            path = Path('/sys/fs/cgroup') / relative.lstrip('/')
            memory = (path / 'memory.max').read_text().strip()
            quota, period = (path / 'cpu.max').read_text().split()
            checked.append({'pid': pid, 'cgroup': relative, 'memory_max': memory, 'cpu_max': f'{quota} {period}'})
            if memory != str(64 * 2**20) or quota == 'max' or int(quota) / int(period) != .5:
                raise RuntimeError(f'app limits not applied: {checked[-1]}')
        if len(checked) != n:
            raise RuntimeError(f'expected {n} app processes, found {len(checked)}')
        return checked

    def prepare(self):
        end = time.monotonic() + 600
        while True:
            try:
                if self.tool in ('compose', 'minikube'):
                    ok = self.docker('info', '--format', '{{.ServerVersion}}', timeout=10, check=False).returncode == 0
                elif self.tool == 'shearwater':
                    ok = self.make_sw_kubeconfig() and self.node_ready()
                else:
                    ok = self.node_ready()
                if ok:
                    break
            except (RuntimeError, subprocess.TimeoutExpired, json.JSONDecodeError, KeyError):
                pass
            if time.monotonic() > end:
                raise RuntimeError('engine did not become ready')
            self.guard()
            time.sleep(2)
        image = self.a.image
        if self.tool == 'minikube':
            p = self.minikube('start', '--driver=docker', '--force', '--container-runtime=containerd',
                              f'--kubernetes-version={self.a.minikube_kubernetes}', f'--cpus={self.a.cpus}',
                              f'--memory={self.a.minikube_memory_mib}mb', '--wait=all', '--interactive=false',
                              timeout=900)
            (self.out / 'minikube-start.log').write_text(p.stdout + p.stderr)
            node = self.minikube('ip').stdout.strip()
            cidr = json.loads(self.kube('get', 'svc', 'kubernetes', '-n', 'default', '-o', 'json').stdout)['spec']['clusterIP']
            # Reach Service IPs from the namespace the same way `minikube tunnel`
            # does: route the service range to the node, whose kube-proxy DNATs.
            self.cmd(['ip', 'route', 'replace', service_range(cidr), 'via', node])
            self.minikube('image', 'pull', image, timeout=600)
        elif self.tool == 'compose':
            self.docker('pull', image, timeout=600)
        elif self.tool == 'shearwater':
            self.cmd([find_tool('ctr', self.a.tool_dir), f'--address={RUN}/containerd.sock', '--namespace=shearwater',
                      'images', 'pull', '--platform=linux/amd64', image], timeout=600)
        else:
            self.cmd([find_tool('k3s', self.a.tool_dir), 'crictl', '--runtime-endpoint',
                      'unix:///run/k3s/containerd/containerd.sock', 'pull', image], timeout=600)
        if self.tool in ('k3s', 'minikube'):
            self.wait_system()
        (self.out / 'prepared.json').write_text(json.dumps({'time': time.time(), 'image': image}))

    def node_ready(self):
        nodes = json.loads(self.kube('get', 'nodes', '-o', 'json', timeout=10).stdout)['items']
        return len(nodes) == 1 and any(c['type'] == 'Ready' and c['status'] == 'True'
                                       for c in nodes[0]['status']['conditions'])

    def make_sw_kubeconfig(self):
        etc = self.root / 'etc'
        if not ((etc / 'admin.token').exists() and (etc / 'ca.crt').exists()):
            return False
        target = self.kubeconfig()
        if not target.exists():
            target.write_text(json.dumps({
                'apiVersion': 'v1', 'kind': 'Config', 'current-context': 'swbench',
                'clusters': [{'name': 'swbench', 'cluster': {'server': f'https://127.0.0.1:{API_PORT}',
                                                              'certificate-authority': str(etc / 'ca.crt')}}],
                'users': [{'name': 'swbench', 'user': {'token': (etc / 'admin.token').read_text().strip()}}],
                'contexts': [{'name': 'swbench', 'context': {'cluster': 'swbench', 'user': 'swbench', 'namespace': 'default'}}],
            }))
            target.chmod(0o600)
        return True

    def wait_system(self):
        end = time.monotonic() + 600
        while time.monotonic() < end:
            pods = json.loads(self.kube('get', 'pods', '-A', '-o', 'json').stdout)['items']
            deps = json.loads(self.kube('get', 'deployments', '-A', '-o', 'json').stdout)['items']
            if deps and all(d.get('status', {}).get('availableReplicas', 0) >= d['spec'].get('replicas', 1) for d in deps) \
                    and all(p['status'].get('phase') == 'Succeeded' or
                            (p['status'].get('containerStatuses') and all(c.get('ready') for c in p['status']['containerStatuses']))
                            for p in pods):
                (self.out / 'system-ready.json').write_text(json.dumps({
                    'pods': [p['metadata']['namespace'] + '/' + p['metadata']['name'] for p in pods],
                    'deployments': [d['metadata']['namespace'] + '/' + d['metadata']['name'] for d in deps]}, indent=2))
                return
            time.sleep(3)
        raise RuntimeError('default add-ons did not become ready')

    def deploy(self, folder, version):
        if self.tool == 'compose':
            return self.compose(folder, version, 'up', '-d', '--wait', '--wait-timeout', '180', timeout=210)
        return self.kube('apply', '--validate=false', '-f', folder / f'kube-{version}.json')

    def remove(self, folder, version):
        if self.tool == 'compose':
            return self.compose(folder, version, 'down', '--timeout', '5')
        p = self.kube('delete', '-f', folder / f'kube-{version}.json', '--wait=true', '--timeout=90s')
        self.wait_gone()
        return p

    def wait_gone(self):
        if self.tool == 'compose':
            return
        end = time.monotonic() + 100
        while json.loads(self.kube('get', 'pods', '-l', 'bench=runtime', '-o', 'json').stdout)['items']:
            if time.monotonic() > end:
                raise RuntimeError('previous Pods did not terminate')
            time.sleep(1)

    def warmup(self):
        """One unmeasured launch checks the exact cached image and limits, so a
        lazy unpack cannot show up as a deployment-speed difference."""
        folder = self.out / 'prewarm'
        write_workload(folder, 1, 'v1', self.a.image)
        self.deploy(folder, 'v1')
        urls = self.wait_apps(1, 'v1')
        limits = self.check_limits(1)
        (folder / 'verified.json').write_text(json.dumps({'image': self.a.image, 'urls': urls, 'limits': limits}))
        self.remove(folder, 'v1')
        time.sleep(5)

    def load(self, folder, phase, seconds, urls, update=None):
        args = ['nsenter', '-t', str(self.pid), '--net', '--', self.a.loadgen, '--urls=' + ','.join(urls),
                f'--duration={seconds}s', f'--rate={self.a.rate}', '--out=' + str(folder / f'{phase}-requests.jsonl')]
        started = time.monotonic()
        p = subprocess.Popen(args, env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        if update:
            update(started)
        _, stderr = p.communicate(timeout=seconds + 240)
        if p.returncode:
            raise RuntimeError('load generator failed: ' + stderr[-2000:])

    def case(self, n):
        self.wait_gone()
        a = self.a
        folder = self.out / f'apps-{n}'
        write_workload(folder, n, 'v1', a.image)
        write_workload(folder, n, 'v2', a.image)
        start = time.monotonic()
        p = self.deploy(folder, 'v1')
        (folder / 'deploy.log').write_text(p.stdout + p.stderr)
        urls = self.wait_apps(n, 'v1')
        seconds = time.monotonic() - start
        (folder / 'deploy.json').write_text(json.dumps({'seconds': seconds, 'urls': urls, 'n': n}))
        log(f'{self.trial} apps={n} deployed in {seconds:.2f}s')
        (folder / 'runtime-limits.json').write_text(json.dumps(self.check_limits(n), indent=2))
        time.sleep(5)
        self.collect(folder, 'idle', a.idle)
        self.collect(folder, 'load', a.steady, lambda: self.load(folder, 'load', a.steady, urls))

        def update(started):
            time.sleep(a.update_at)
            began = time.monotonic()
            u = self.deploy(folder, 'v2')
            (folder / 'update.log').write_text(u.stdout + u.stderr)
            self.wait_apps(n, 'v2')
            now = time.monotonic()
            (folder / 'update.json').write_text(json.dumps({
                'seconds': now - began, 'start_offset_s': began - started, 'ready_offset_s': now - started,
                'window_s': a.update}))
            if now - started > a.update - 2:
                raise RuntimeError('rollout did not finish inside the load window; coverage would be incomplete')

        self.collect(folder, 'update', a.update, lambda: self.load(folder, 'update', a.update, urls, update))
        if not self.http_check(urls, 'v2'):
            raise RuntimeError('final version is not v2')
        p = self.remove(folder, 'v2')
        (folder / 'cleanup.log').write_text(p.stdout + p.stderr)
        (folder / 'completed.json').write_text(json.dumps({'final_version': 'v2', 'time': time.time()}))
        log(f'{self.trial} apps={n} complete')
        time.sleep(5)


def service_range(cluster_ip):
    """The Service range minikube uses for a given kubernetes Service IP."""
    first = cluster_ip.split('.')
    if first[0] == '10' and 96 <= int(first[1]) <= 111:
        return '10.96.0.0/12'
    return '.'.join(first[:3]) + '.0/24'


def log(msg):
    print(time.strftime('%H:%M:%S ') + msg, flush=True)


# ---------------------------------------------------------------- guardian
# Runs inside the private network namespace, a new mount namespace and a new
# cgroup namespace whose root is the trial cgroup. Everything it starts is
# therefore charged to the trial.

def guardian(tool, root, out, cgroup, sw, tool_dir):
    root, out = Path(root), Path(out)

    def tool_path(name):
        return find_tool(name, tool_dir or None)

    sh(['mount', '--make-rprivate', '/'])
    # The cgroup namespace already starts at the trial cgroup; make the
    # cgroup filesystem show the same subtree so runtimes create app cgroups
    # inside it.
    if not cgroup.startswith('/sys/fs/cgroup/' + CGROUP_PREFIX):
        raise SystemExit(f'unexpected cgroup {cgroup}')
    sh(['mount', '--bind', cgroup, '/sys/fs/cgroup'])
    local = Path('/sys/fs/cgroup')
    (local / 'supervisor').mkdir()
    (local / 'supervisor' / 'cgroup.procs').write_text(str(os.getpid()))
    controllers = (local / 'cgroup.controllers').read_text().split()
    (local / 'cgroup.subtree_control').write_text(' '.join('+' + c for c in controllers))
    resolv = (root / 'resolv.conf').read_text()
    resolv_target = os.path.realpath('/etc/resolv.conf')
    sh(['mount', '-t', 'tmpfs', '-o', 'mode=755,size=256m', 'swbench-run', '/run'])
    if resolv_target.startswith('/run/'):
        Path(resolv_target).parent.mkdir(parents=True, exist_ok=True)
        Path(resolv_target).write_text(resolv)
    else:
        sh(['mount', '--bind', root / 'resolv.conf', '/etc/resolv.conf'])
    # Private copies of the conventional Kubernetes paths, in this mount
    # namespace only; the host's directories are not read or changed.
    for target in ('/var/lib/kubelet', '/var/lib/cni', '/var/log/pods', '/var/log/containers',
                   '/etc/cni/net.d', '/etc/rancher', '/var/lib/rancher'):
        source = root / 'mounts' / target.lstrip('/')
        source.mkdir(parents=True)
        Path(target).mkdir(parents=True, exist_ok=True)
        sh(['mount', '--bind', source, target])
    Path(RUN).mkdir(parents=True)
    Path('/run/user/0').mkdir(parents=True, mode=0o700)
    (root / 'empty.json').write_text('{}\n')
    (root / 'empty.yaml').write_text('{}\n')
    children = []

    def spawn(args, name):
        f = (out / f'{name}.log').open('w')
        children.append(subprocess.Popen([str(a) for a in args], stdout=f, stderr=subprocess.STDOUT))

    if tool in ('compose', 'minikube'):
        spawn([tool_path('dockerd'), f'--config-file={root / "empty.json"}', f'--data-root={root / "docker"}',
               f'--exec-root={RUN}/docker', f'--pidfile={RUN}/docker.pid', f'--host=unix://{RUN}/docker.sock',
               '--group=root', '--exec-opt=native.cgroupdriver=cgroupfs', '--cgroup-parent=/containers',
               '--iptables=true', '--ip6tables=false', '--log-level=warn'], 'engine')
    elif tool == 'shearwater':
        socket = f'{RUN}/containerd.sock'
        spawn([tool_path('containerd'), '--config', '/dev/null', '--root', root / 'containerd',
               '--state', f'{RUN}/containerd', '--address', socket], 'containerd')
        end = time.monotonic() + 30
        while not Path(socket).exists() and time.monotonic() < end:
            time.sleep(.25)
        spawn([sw, 'server', f'--listen=127.0.0.1:{API_PORT}', '--node-name=swbench',
               '--runtime=containerd', f'--data-dir={root / "data"}', f'--config-dir={root / "etc"}',
               f'--log-dir={root / "logs"}', f'--run-dir={RUN}/sw', f'--containerd-socket={socket}'], 'engine')
    elif tool == 'k3s':
        # Default bundled components stay on. Only addresses, storage and the
        # cgroup driver change so the fixture stays off the host; kube-proxy is
        # told not to rewrite shared conntrack sysctls.
        spawn([tool_path('k3s'), 'server', '--config', root / 'empty.yaml', '--data-dir', root / 'k3s',
               '--write-kubeconfig', root / 'k3s' / 'kubeconfig', '--node-name=swbench', f'--node-ip={NS_IP}',
               '--bind-address=0.0.0.0', f'--advertise-address={NS_IP}', f'--https-listen-port={API_PORT}',
               '--flannel-iface=eth0', '--cluster-cidr=10.42.0.0/16', '--service-cidr=10.43.0.0/16',
               '--kubelet-arg=cgroup-driver=cgroupfs',
               '--kube-proxy-arg=conntrack-max-per-core=0',
               '--kube-proxy-arg=conntrack-tcp-timeout-established=0s',
               '--kube-proxy-arg=conntrack-tcp-timeout-close-wait=0s',
               '--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<5%',
               '--private-registry', root / 'empty.yaml', '--default-local-storage-path', root / 'local-storage',
               '--prefer-bundled-bin'], 'engine')
    else:
        raise SystemExit(f'unknown tool {tool}')
    (out / 'guardian-ready.json').write_text(json.dumps({'pid': os.getpid(), 'children': [p.pid for p in children]}))
    stopping = []
    signal.signal(signal.SIGTERM, lambda *_: stopping.append(1))
    while not stopping:
        if any(p.poll() is not None for p in children):
            (out / 'engine-exit.json').write_text(json.dumps([p.poll() for p in children]))
            break
        time.sleep(.5)
    for p in reversed(children):
        if p.poll() is None:
            p.terminate()
    for p in children:
        try:
            p.wait(timeout=20)
        except subprocess.TimeoutExpired:
            p.kill()


# ---------------------------------------------------------------- run

def tool_version(args, tool):
    try:
        if tool == 'compose':
            return sh([find_tool('docker-compose', args.tool_dir), 'version', '--short'], capture_output=True).stdout.strip() + \
                ' / dockerd ' + sh([find_tool('dockerd', args.tool_dir), '--version'], capture_output=True).stdout.strip()
        if tool == 'k3s':
            return sh([find_tool('k3s', args.tool_dir), '--version'], capture_output=True).stdout.splitlines()[0]
        if tool == 'minikube':
            return sh([find_tool('minikube', args.tool_dir), 'version', '--short'], capture_output=True).stdout.strip() + \
                f' / kubernetes {args.minikube_kubernetes}'
        return sh([args.sw, 'version'], capture_output=True).stdout.strip().splitlines()[0]
    except (subprocess.CalledProcessError, IndexError, SystemExit) as e:
        return f'unknown ({e})'


def preflight(args, tools):
    if os.geteuid() != 0:
        raise SystemExit('run needs root (namespaces, cgroups, iptables)')
    if not args.yes:
        raise SystemExit('run changes iptables, kernel values and cgroups on this host while it runs; '
                         'use a disposable host and pass --yes')
    if not Path('/sys/fs/cgroup/cgroup.controllers').exists():
        raise SystemExit('cgroup v2 is required')
    enabled = Path('/sys/fs/cgroup/cgroup.subtree_control').read_text().split()
    missing = {'cpu', 'memory', 'pids'} - set(enabled)
    if missing:
        raise SystemExit(f'enable cgroup controllers {sorted(missing)} in /sys/fs/cgroup/cgroup.subtree_control')
    for name in ('ip', 'iptables', 'nsenter', 'unshare', 'mount'):
        find_tool(name, None)
    need = {'compose': ['dockerd', 'containerd', 'docker', 'docker-compose'], 'k3s': ['k3s', 'kubectl'],
            'minikube': ['dockerd', 'containerd', 'docker', 'minikube', 'kubectl'],
            'shearwater': ['containerd', 'ctr', 'runc', 'kubectl']}
    for tool in tools:
        for name in need[tool]:
            find_tool(name, args.tool_dir)
    args.sw = str(Path(args.sw).resolve())
    if 'shearwater' in tools and not Path(args.sw).is_file():
        raise SystemExit('--sw must point at the sw binary to measure')
    args.loadgen = str(Path(args.loadgen).resolve())
    if not Path(args.loadgen).is_file():
        raise SystemExit('--loadgen must point at the built hack/benchload binary')


def run(args):
    tools = [t for t in args.tools.split(',') if t]
    unknown = set(tools) - set(TOOLS)
    if unknown:
        raise SystemExit(f'unknown tools: {sorted(unknown)}')
    apps = [int(x) for x in args.apps.split(',')]
    if args.tool_dir:
        args.tool_dir = str(Path(args.tool_dir).resolve())
    preflight(args, tools)
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    if any(out.iterdir()):
        raise SystemExit(f'{out} is not empty')
    Path(args.work).mkdir(parents=True, exist_ok=True)
    meta = {'host': os.uname().nodename, 'kernel': os.uname().release, 'cpus_available': os.cpu_count(),
            'cap': f'{args.cpus} CPU, {args.memory_mib} MiB, no swap', 'image': args.image,
            'phases': f'idle {args.idle}s, steady {args.steady}s at {args.rate} rps, update {args.update}s (change at {args.update_at}s)',
            'repeats': args.repeats}
    meta.update({f'version_{t}': tool_version(args, t) for t in tools})
    for key in ('GITHUB_SHA', 'GITHUB_RUN_ID', 'RUNNER_NAME'):
        if os.environ.get(key):
            meta[key.lower()] = os.environ[key]
    (out / 'run.json').write_text(json.dumps({'meta': meta, 'tools': tools, 'apps': apps}, indent=2))
    if args.no_kernel_tuning:
        if {'k3s', 'minikube'} & set(tools):
            raise SystemExit('--no-kernel-tuning: kubelet would change these kernel values itself mid-run; '
                             'measure only compose and shearwater')
        if Path('/proc/sys/net/ipv4/ip_forward').read_text().strip() != '1':
            raise SystemExit('--no-kernel-tuning needs net.ipv4.ip_forward=1 already set')
        before = {}
    else:
        before = kernel_apply()
    (out / 'kernel-before.json').write_text(json.dumps(before, indent=2))
    meta['kernel_tuning'] = 'off' if args.no_kernel_tuning else 'kubelet defaults applied to every tool'
    (out / 'run.json').write_text(json.dumps({'meta': meta, 'tools': tools, 'apps': apps}, indent=2))
    network_up()
    failed = []
    try:
        for r in range(1, args.repeats + 1):
            order = tools[(r - 1) % len(tools):] + tools[:(r - 1) % len(tools)]
            for tool in order:
                trial = f'r{r}-{tool}'
                log(f'{trial} start')
                engine = Engine(args, tool, trial, out)
                try:
                    engine.start()
                    engine.prepare()
                    engine.warmup()
                    for n in apps:
                        engine.case(n)
                except Exception:
                    failed.append(trial)
                    text = traceback.format_exc()
                    (out / trial).mkdir(parents=True, exist_ok=True)
                    (out / trial / f'failure-{int(time.time())}.txt').write_text(text)
                    log(f'{trial} FAILED\n{text}')
                    if not args.keep_going:
                        raise
                finally:
                    engine.stop()
                    time.sleep(5)
    finally:
        network_down()
        kernel_restore(before)
        rows, failures = summarize(out)
        print((out / 'summary.md').read_text())
    if failed:
        raise SystemExit(f'failed trials: {", ".join(failed)}')


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    sub = p.add_subparsers(dest='command', required=True)
    r = sub.add_parser('run', help='measure (root, disposable host)')
    r.add_argument('--yes', action='store_true', help='confirm this host may be changed while the run lasts')
    r.add_argument('--out', required=True, help='empty result directory')
    r.add_argument('--work', default='/var/lib/swbench', help='engine state directory (local disk, not 9p)')
    r.add_argument('--tools', default=','.join(TOOLS))
    r.add_argument('--apps', default='1,10')
    r.add_argument('--repeats', type=int, default=1)
    r.add_argument('--sw', default='dist/sw')
    r.add_argument('--loadgen', default='dist/benchload')
    r.add_argument('--tool-dir', default='')
    r.add_argument('--image', default=DEFAULT_IMAGE)
    r.add_argument('--cpus', type=int, default=4)
    r.add_argument('--memory-mib', type=int, default=3072)
    r.add_argument('--minikube-memory-mib', type=int, default=2600)
    r.add_argument('--minikube-kubernetes', default='v1.37.0')
    r.add_argument('--idle', type=int, default=15)
    r.add_argument('--steady', type=int, default=30)
    r.add_argument('--update', type=int, default=45)
    r.add_argument('--update-at', type=int, default=10)
    r.add_argument('--rate', type=int, default=200)
    r.add_argument('--keep-going', action='store_true', help='record a failed tool and continue with the rest')
    r.add_argument('--keep-work', action='store_true', help='keep engine state directories afterwards')
    r.add_argument('--no-kernel-tuning', action='store_true',
                   help='leave kernel values alone (a host whose kernel is shared, e.g. WSL); compose/shearwater only')
    s = sub.add_parser('summarize', help='rebuild summary.csv/json/md from raw results')
    s.add_argument('out')
    g = sub.add_parser('_guardian')
    g.add_argument('tool')
    g.add_argument('root')
    g.add_argument('out')
    g.add_argument('cgroup')
    g.add_argument('sw')
    g.add_argument('tool_dir', nargs='?', default='')
    args = p.parse_args(argv)
    if args.command == 'run':
        run(args)
    elif args.command == 'summarize':
        rows, failures = summarize(args.out)
        print(Path(args.out, 'summary.md').read_text())
    else:
        guardian(args.tool, args.root, args.out, args.cgroup, args.sw, args.tool_dir)


if __name__ == '__main__':
    main()
