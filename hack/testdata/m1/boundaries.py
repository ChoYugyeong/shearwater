#!/usr/bin/env python3
"""Four M0 compatibility boundaries, checked against an isolated real sw server.

In WSL from the repository root:
  . hack/env.sh
  python3 hack/testdata/m1/boundaries.py

A nonzero exit means at least one expected API behavior is missing.
This does not modify server code, require root, or run any containers.
"""
import copy
import datetime
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

root = Path(__file__).resolve().parents[3]
os.chdir(root)
run = Path.home() / ".cache/shearwater-dev" / (
    "m1-boundaries-" + datetime.datetime.now().strftime("%Y%m%d-%H%M%S") + "-" + str(os.getpid()))
run.mkdir(parents=True)
port = int(os.environ.get("SW_PORT", "16443"))
with socket.socket() as probe:
    probe.bind(("127.0.0.1", port))
binary = run / "sw"
subprocess.run(["go", "build", "-o", str(binary), "./cmd/sw"], check=True)
(run / "source-commit.txt").write_bytes(subprocess.check_output(["git", "rev-parse", "HEAD"]))
context = ssl._create_unverified_context()  # isolated loopback server's generated certificate
base = f"https://127.0.0.1:{port}"
results = []
log = (run / "server.log").open("w")
server = subprocess.Popen([str(binary), "server", "--listen", f"127.0.0.1:{port}",
                           "--data-dir", str(run / "data"), "--config-dir", str(run / "etc"),
                           "--trace", str(run / "trace.jsonl")], stderr=log)
try:
    for attempt in range(80):
        if server.poll() is not None:
            raise RuntimeError("server exited; see " + str(run / "server.log"))
        try:
            with urllib.request.urlopen(base + "/readyz", context=context, timeout=1) as r:
                if r.read().strip() == b"ok":
                    break
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.1)
    else:
        raise RuntimeError("server not ready")
    config = run / "kubeconfig"
    config.write_bytes(subprocess.check_output(
        [str(binary), "kubeconfig", "--config-dir", str(run / "etc"), "--server", base]))
    kc = json.loads(subprocess.check_output(
        ["kubectl", "--kubeconfig", str(config), "config", "view", "--raw", "-o", "json"]))
    token = kc["users"][0]["user"]["token"]

    def request(method, path, body):
        raw = body if isinstance(body, str) else json.dumps(body, separators=(",", ":"))
        req = urllib.request.Request(base + path, data=raw.encode(), method=method,
                                     headers={"Authorization": "Bearer " + token,
                                              "Content-Type": "application/json"})
        try:
            response = urllib.request.urlopen(req, context=context, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            value = json.loads(response.read())
            return response.status, value

    def record(name, expected, observed, passed):
        results.append(dict(name=name, expected=expected, observed=observed, passed=passed))
        print(("PASS " if passed else "FAIL ") + name + ": " + json.dumps(observed))

    deploy_path = "/apis/apps/v1/namespaces/default/deployments"
    pod_path = "/api/v1/namespaces/default/pods"
    deploy = json.loads((root / "hack/testdata/m1/apply-00.json").read_text())
    deploy["metadata"]["name"] = "boundary-generation"
    status, current = request("POST", deploy_path, deploy)
    assert status == 201, current
    old_generation = current["metadata"]["generation"]
    current["metadata"]["annotations"] = {"review": "annotation-only"}
    status, updated = request("PUT", deploy_path + "/boundary-generation", current)
    record("Deployment annotation-only generation", old_generation + 1,
           {"http": status, "generation": updated.get("metadata", {}).get("generation")},
           status == 200 and updated["metadata"]["generation"] == old_generation + 1)

    pod = json.loads((root / "hack/testdata/m1/pod.json").read_text())
    pod["metadata"] = {"name": "boundary-finalizer", "finalizers": ["example.com/hold"]}
    status, current = request("POST", pod_path, pod)
    assert status == 201, current
    status, current = request("DELETE", pod_path + "/boundary-finalizer",
                              {"propagationPolicy": "Background"})
    assert status == 200 and current["metadata"].get("deletionTimestamp"), current
    current["metadata"]["finalizers"].append("example.com/new")
    status, updated = request("PUT", pod_path + "/boundary-finalizer", current)
    record("New finalizer during deletion", "422 Invalid",
           {"http": status, "reason": updated.get("reason"),
            "finalizers": updated.get("metadata", {}).get("finalizers")},
           status == 422 and updated.get("reason") == "Invalid")

    duplicate = copy.deepcopy(deploy)
    duplicate["metadata"]["name"] = "boundary-duplicate"
    raw = json.dumps(duplicate, separators=(",", ":"))
    assert raw.count('"replicas":2') == 1
    raw = raw.replace('"replicas":2', '"replicas":1,"replicas":2', 1)
    status, updated = request("POST", deploy_path + "?fieldValidation=Strict", raw)
    record("Strict duplicate JSON key", "400 BadRequest",
           {"http": status, "reason": updated.get("reason"),
            "replicas": updated.get("spec", {}).get("replicas")},
           status == 400 and updated.get("reason") == "BadRequest")

    valid = copy.deepcopy(pod)
    valid["metadata"] = {"name": "boundary-invalid-mode"}
    status, updated = request("POST", pod_path + "?fieldValidation=Maybe", valid)
    record("Invalid fieldValidation with otherwise valid body", "400 or 422",
           {"http": status, "reason": updated.get("reason")},
           status in (400, 422))
finally:
    server.terminate()
    try:
        server.wait(timeout=5)
    except subprocess.TimeoutExpired:
        server.kill()
        server.wait()
    log.close()
    (run / "report.json").write_text(json.dumps(results, indent=2) + "\n")
    print("run dir:", run)
passed = sum(r["passed"] for r in results)
print(f"{passed} passed, {len(results)-passed} failed")
sys.exit(0 if len(results) == 4 and passed == 4 else 1)
