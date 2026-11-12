"""Measure one fresh Shearwater instance without adding a tracing daemon."""

import ctypes
import http.client
import json
import os
import select
import ssl
import statistics
import struct
import subprocess
import sys
import tempfile
import time
from pathlib import Path


# inotify gives us a file-specific view while /proc/<pid>/io attributes
# storage writes to the sw process. Inotify can merge identical notifications;
# we report the count as notifications and require *zero* for the idle gate.
IN_MODIFY = 0x00000002
IN_MOVED_FROM = 0x00000040
IN_MOVED_TO = 0x00000080
IN_CREATE = 0x00000100
IN_DELETE = 0x00000200
IN_Q_OVERFLOW = 0x00004000
EVENT = struct.Struct("iIII")


def run(args, **kwargs):
    return subprocess.run(args, text=True, check=True, **kwargs)


def ctr_ids(kind):
    return set(run(["ctr", "--namespace", "shearwater", kind, "list", "-q"],
                   capture_output=True).stdout.split())


def proc_sample(pid):
    base = Path(f"/proc/{pid}")
    # The command name in stat can have spaces or parentheses, so split only
    # after the last closing parenthesis. The remaining fields start at 3.
    stat = (base / "stat").read_text().rsplit(")", 1)[1].split()
    status = (base / "status").read_text().splitlines()
    rss_kib = next(int(row.split()[1]) for row in status if row.startswith("VmRSS:"))
    io = dict(line.split(":", 1) for line in (base / "io").read_text().splitlines())
    return {"ticks": int(stat[11]) + int(stat[12]), "rss_kib": rss_kib,
            "write_bytes": int(io["write_bytes"]), "syscw": int(io["syscw"])}


def add_watch(lib, fd, path):
    mask = IN_MODIFY | IN_MOVED_FROM | IN_MOVED_TO | IN_CREATE | IN_DELETE
    wd = lib.inotify_add_watch(fd, os.fsencode(path), mask)
    if wd < 0:
        raise OSError(ctypes.get_errno(), f"watch {path}")
    return wd


def sample_idle(pid, seconds, data, log):
    lib = ctypes.CDLL(None, use_errno=True)
    fd = lib.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
    if fd < 0:
        raise OSError(ctypes.get_errno(), "inotify_init1")
    try:
        watches = {
            add_watch(lib, fd, data): {"state.db", "state.db-wal", "state.db-journal"},
            add_watch(lib, fd, log): {"server.log"},
        }
        sizes_before = file_stamps(data, log)
        t0 = time.monotonic()
        initial = proc_sample(pid)
        samples = [{"elapsed_s": 0, **initial}]
        notifications = []
        tick = t0 + 1
        while time.monotonic() < t0 + seconds:
            now = time.monotonic()
            delay = max(0, min(tick, t0 + seconds) - now)
            if select.select([fd], [], [], delay)[0]:
                read_events(fd, watches, notifications)
            now = time.monotonic()
            if now >= tick:
                samples.append({"elapsed_s": round(now - t0, 3), **proc_sample(pid)})
                tick = now + 1
        end = proc_sample(pid)
        elapsed = time.monotonic() - t0
        read_events(fd, watches, notifications)
        sizes_after = file_stamps(data, log)
    finally:
        os.close(fd)
    ticks = end["ticks"] - initial["ticks"]
    rss = [s["rss_kib"] for s in samples] + [end["rss_kib"]]
    return {
        "elapsed_s": round(elapsed, 3),
        "cpu_seconds": ticks / os.sysconf("SC_CLK_TCK"),
        "cpu_percent_of_one_core": round(100 * ticks / os.sysconf("SC_CLK_TCK") / elapsed, 4),
        "rss_peak_bytes": max(rss) * 1024,
        "rss_median_bytes": round(statistics.median(rss) * 1024),
        "sw_write_bytes": end["write_bytes"] - initial["write_bytes"],
        "sw_write_syscalls_including_sockets": end["syscw"] - initial["syscw"],
        "file_notifications": notifications,
        "file_stamps_before": sizes_before, "file_stamps_after": sizes_after,
        "samples": samples,
    }


def read_events(fd, watches, notifications):
    while True:
        try:
            buf = os.read(fd, 65536)
        except BlockingIOError:
            return
        offset = 0
        while offset < len(buf):
            wd, mask, _, length = EVENT.unpack_from(buf, offset)
            offset += EVENT.size
            name = os.fsdecode(buf[offset:offset + length].rstrip(b"\x00"))
            offset += length
            if mask & IN_Q_OVERFLOW:
                raise RuntimeError("inotify overflow; cannot report idle writes")
            if name in watches.get(wd, set()):
                notifications.append({"file": name, "mask": hex(mask)})


def file_stamps(data, log):
    result = {}
    for path in (data / "state.db", data / "state.db-wal",
                 data / "state.db-journal", log / "server.log"):
        try:
            s = path.stat()
            result[path.name] = [s.st_size, s.st_mtime_ns]
        except FileNotFoundError:
            result[path.name] = None
    return result


def start(sw, flags, evidence, config, port):
    # Start timing before the launcher checks the host. No Pod exists yet,
    # so image pulls cannot lengthen the startup measurement.
    t0 = time.monotonic()
    with (evidence / "up.txt").open("w") as output:
        up = subprocess.Popen([sw, "up", *flags], stdout=output, stderr=output)
        first_ready = None
        while time.monotonic() - t0 < 21:
            if up.poll() not in (None, 0):
                raise RuntimeError(f"sw up failed; see {evidence / 'up.txt'}")
            try:
                tls = ssl.create_default_context(cafile=config / "ca.crt")
                conn = http.client.HTTPSConnection("127.0.0.1", port, timeout=0.2, context=tls)
                try:
                    conn.request("GET", "/readyz")
                    with conn.getresponse() as response:
                        if response.status == 200 and response.read() == b"ok":
                            first_ready = time.monotonic() - t0
                            break
                finally:
                    conn.close()
            except (OSError, http.client.HTTPException):
                pass
            time.sleep(0.01)
        if up.wait(timeout=4) != 0 or first_ready is None:
            raise RuntimeError("could not measure /readyz; see up.txt")
        return round(first_ready, 4)


def measure(sw, kubectl):
    if ctr_ids("containers") or ctr_ids("tasks"):
        raise RuntimeError("the shearwater namespace is in use; test only on an idle Shearwater node")
    work = Path("/var/lib/shearwater/check")
    run_root = Path("/run/shearwater")
    if work.resolve() != work or run_root.resolve() != run_root:
        raise RuntimeError("measurement directories must not redirect through symlinks")
    work.mkdir(parents=True, exist_ok=True)
    evidence = Path(tempfile.mkdtemp(prefix="measure.", dir=work))
    run_root.mkdir(parents=True, exist_ok=True)
    tmp = Path(tempfile.mkdtemp(prefix="measure.", dir=run_root))
    data, config, log = (evidence / name for name in ("data", "etc", "log"))
    port = int(os.environ.get("SW_MEASURE_PORT", "16450"))
    seconds = max(60, int(os.environ.get("SW_MEASURE_SECONDS", "60")))
    flags = [f"--listen=127.0.0.1:{port}", "--node-name=measure-check",
             f"--data-dir={data}", f"--config-dir={config}", f"--log-dir={log}", f"--run-dir={tmp}"]
    print(f"Evidence: {evidence}", flush=True)
    k = [kubectl, "--kubeconfig", str(config / "kubeconfig"), "--request-timeout=15s"]
    try:
        startup = start(sw, flags, evidence, config, port)
        pid = json.loads((tmp / "server.json").read_text())["pid"]
        run(k + ["wait", "--for=condition=Ready", "node/measure-check", "--timeout=25s"])
        run(k + ["create", "deployment", "idle", "--image=busybox:1.36", "--replicas=10", "--", "sleep", "600"])
        run(k + ["rollout", "status", "deployment/idle", "--timeout=120s"])
        time.sleep(5)  # let the last status and Event writes settle
        # kubectl's formatted output may strip List metadata. The raw
        # response retains the revision, so we can compare before and after.
        request = k + ["get", "--raw", "/api/v1/namespaces/default/pods?labelSelector=app%3Didle"]
        before = json.loads(run(request, capture_output=True).stdout)
        if len(before["items"]) != 10 or not all(any(
                c["type"] == "Ready" and c["status"] == "True"
                for c in p["status"]["conditions"]) for p in before["items"]):
            raise RuntimeError("ten Ready Pods are required for the idle sample")
        expected = {c["containerID"].removeprefix("containerd://") for p in before["items"]
                    for c in p["status"]["containerStatuses"]}
        if len(expected) != 10 or ctr_ids("containers") != expected or ctr_ids("tasks") != expected:
            raise RuntimeError("unexpected extra container or task in the measurement namespace")
        print(f"Sampling PID {pid} with 10 Ready Pods for {seconds}s...", flush=True)
        metrics = sample_idle(pid, seconds, data, log)
        after = json.loads(run(request, capture_output=True).stdout)
        metrics.update({
            "readyz_seconds": startup,
            "binary_bytes": Path(sw).stat().st_size,
            "kernel": os.uname().release,
            "server_version": run([sw, "version"], capture_output=True).stdout.strip(),
            "ready_pods": len(before["items"]),
            "containerd_tasks": len(expected),
            "extra_pod_tasks": len(ctr_ids("tasks") - expected),
            "resource_version_before": before["metadata"]["resourceVersion"],
            "resource_version_after": after["metadata"]["resourceVersion"],
        })
        if metrics["extra_pod_tasks"] or ctr_ids("tasks") != expected:
            raise RuntimeError("Pod tasks changed during the sample")
        if not metrics["resource_version_before"] or metrics["resource_version_before"] != metrics["resource_version_after"]:
            raise RuntimeError("cluster state changed during the idle sample")
        metrics["targets_met"] = {
            "idle_writes_zero": metrics["sw_write_bytes"] == 0 and not metrics["file_notifications"]
                and metrics["file_stamps_before"] == metrics["file_stamps_after"],
            "rss_le_50_MB": metrics["rss_peak_bytes"] <= 50_000_000,
            "binary_le_40_MB": metrics["binary_bytes"] <= 40_000_000,
            "readyz_le_1_s": startup <= 1,
        }
        (evidence / "measure.json").write_text(json.dumps(metrics, indent=2) + "\n")
        print(json.dumps({k: v for k, v in metrics.items() if k not in ("samples",)}, indent=2))
    finally:
        # The fixture is the only instance we are allowed to stop. Keep all
        # evidence and credentials in its private directory.
        run([sw, "down", "--all", *flags])


if __name__ == "__main__":
    args = sys.argv[1:]
    if (os.geteuid() != 0 or len(args) != 2 or
            not all(Path(p).is_absolute() and os.access(p, os.X_OK) for p in args)):
        sys.exit("usage (WSL root): bash hack/measure.sh /absolute/sw /absolute/kubectl")
    measure(*args)
