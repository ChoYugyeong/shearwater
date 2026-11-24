#!/usr/bin/env python3
"""Portable checks for hack/bench_runtime.py: workload and summaries only.

Nothing here needs root, namespaces or a container runtime.
"""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('bench_runtime', HERE / 'bench_runtime.py')
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)


def resources(folder, phase, rows):
    with (folder / f'{phase}-resources.jsonl').open('w') as f:
        for i, (mem, pss, usec) in enumerate(rows):
            f.write(json.dumps({
                'monotonic': float(i), 'memory_current': mem, 'pss': pss,
                'memory_stat': {'anon': 1, 'file': 2, 'kernel': 3},
                'cpu': {'usage_usec': usec, 'throttled_usec': 0},
                'memory_events': {'oom_kill': 0, 'max': 0},
            }) + '\n')


def requests(folder, phase, rows):
    with (folder / f'{phase}-requests.jsonl').open('w') as f:
        for i, (latency, error, version) in enumerate(rows):
            row = {'index': i, 'offset_s': i / 200, 'dispatch_lag_ms': 0.1, 'latency_ms': latency,
                   'scheduled_latency_ms': latency, 'status': 200, 'bytes': 1024, 'version': version}
            if error:
                row['error'] = error
            f.write(json.dumps(row) + '\n')


def case(out, trial, apps, deploy, pss_mib, errors=0):
    folder = out / trial / f'apps-{apps}'
    folder.mkdir(parents=True)
    (folder / 'deploy.json').write_text(json.dumps({'seconds': deploy}))
    (folder / 'update.json').write_text(json.dumps({'seconds': 1.5}))
    mib = 2**20
    for phase in ('idle', 'load', 'update'):
        # 1 second of CPU over a 2 second span is 50% of one core.
        resources(folder, phase, [(200 * mib, pss_mib * mib, 0), (210 * mib, pss_mib * mib, 500000),
                                  (220 * mib, pss_mib * mib, 1000000)])
    requests(folder, 'load', [(1.0, None, 'v1'), (2.0, None, 'v1'), (3.0, None, 'v1')])
    requests(folder, 'update', [(1.0, None, 'v1'), (1.0, None, 'v2')] +
             [(2000.0, 'Get "http://x": context deadline exceeded (Client.Timeout exceeded)', '')] * errors)
    (folder / 'completed.json').write_text('{}')


class WorkloadTest(unittest.TestCase):
    def test_ten_apps_are_independent_and_limited(self):
        compose, kube = bench.workload(10, 'v2', 'img@sha256:abc')
        self.assertEqual(sorted(compose['services']), [f'web{i:02d}' for i in range(10)])
        deployments = [o for o in kube['items'] if o['kind'] == 'Deployment']
        services = [o for o in kube['items'] if o['kind'] == 'Service']
        self.assertEqual((len(deployments), len(services)), (10, 10))
        for d in deployments:
            self.assertEqual(d['spec']['replicas'], 1)
            self.assertEqual(d['spec']['strategy']['rollingUpdate'], {'maxUnavailable': 0, 'maxSurge': 1})
            c = d['spec']['template']['spec']['containers'][0]
            self.assertEqual(c['imagePullPolicy'], 'Never')
            self.assertEqual(c['resources']['limits'], {'cpu': '500m', 'memory': '64Mi'})
        web = compose['services']['web03']
        self.assertEqual((web['pull_policy'], web['cpus'], web['mem_limit']), ('never', 0.5, '64m'))
        self.assertEqual(web['ports'], ['127.0.0.1:18003:8080'])

    def test_body_is_exactly_the_checked_size(self):
        compose, _ = bench.workload(1, 'v1', 'img')
        command = compose['services']['web00']['command'][0]
        body = command.split("printf '%s' '", 1)[1].split("'", 1)[0]
        self.assertEqual(len(body), bench.BODY_SIZE)
        self.assertTrue(body.startswith('v1 x'))


class SummaryTest(unittest.TestCase):
    def test_summary_tables_and_csv(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            (out / 'run.json').write_text(json.dumps({'meta': {'repeats': 2}}))
            case(out, 'r1-shearwater', 1, 0.8, 100)
            case(out, 'r2-shearwater', 1, 1.0, 110)
            case(out, 'r1-minikube', 1, 3.0, 900, errors=2)
            (out / 'r1-k3s').mkdir()
            (out / 'r1-k3s' / 'failure-1.txt').write_text('Traceback\nRuntimeError: engine did not become ready\n')
            rows, failures = bench.summarize(out)

            self.assertEqual(len(rows), 3)
            sw = [r for r in rows if r['tool'] == 'shearwater' and r['trial'] == 'r1-shearwater'][0]
            self.assertAlmostEqual(sw['idle']['cpu_one_core_percent'], 50.0)
            self.assertEqual(sw['idle']['memory_mib'], 210)
            self.assertEqual(sw['load']['p50_ms'], 2.0)
            mk = [r for r in rows if r['tool'] == 'minikube'][0]
            self.assertEqual(mk['update']['errors'], 2)
            self.assertEqual(mk['update']['error_types'], {'timeout': 2})
            self.assertEqual(failures, ['r1-k3s: RuntimeError: engine did not become ready'])

            md = (out / 'summary.md').read_text()
            self.assertIn('| Shearwater | 2 | 105.00 [100.00–110.00] |', md)
            self.assertIn('| minikube | 1 | 900.00 |', md)
            self.assertIn('2/4 |', md)
            self.assertLess(md.index('| minikube'), md.index('| Shearwater'))
            self.assertIn('engine did not become ready', md)
            header = (out / 'summary.csv').read_text().splitlines()[0].split(',')
            self.assertEqual(header[:5], ['tool', 'trial', 'apps', 'deploy_s', 'update_s'])
            self.assertIn('update_errors', header)

    def test_quantile_and_service_range(self):
        self.assertEqual(bench.quantile([1, 2, 3, 4], .5), 2.5)
        self.assertIsNone(bench.quantile([], .5))
        self.assertEqual(bench.service_range('10.96.0.1'), '10.96.0.0/12')
        self.assertEqual(bench.service_range('172.30.0.1'), '172.30.0.0/24')


if __name__ == '__main__':
    unittest.main()
