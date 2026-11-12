"""Check the JSON and runtime evidence saved by hack/e2e-stage1.sh."""

import datetime
import json
import os
from pathlib import Path
import re
import select
import signal
import sqlite3
import subprocess
import sys


def read(path):
    return json.loads(Path(path).read_text())


def write(path, obj):
    path.write_text(json.dumps(obj, indent=2) + "\n")


def manifests(deployment, directory):
    """Keep the first apply's Pod template identical to kubectl create's."""
    directory = Path(directory)
    obj = read(deployment)
    obj = {key: obj[key] for key in ("apiVersion", "kind", "metadata", "spec")}
    obj["metadata"] = {key: obj["metadata"][key] for key in ("name", "namespace", "labels") if key in obj["metadata"]}
    containers = obj["spec"]["template"]["spec"]["containers"]
    (nginx,) = containers
    assert nginx["image"] == "nginx:1.27", nginx
    write(directory / "web-original.json", obj)
    nginx["image"] = "nginx:1.28"
    write(directory / "web-image.json", obj)

    keep = {"name": "STAGE1_KEEP", "value": "keep"}
    nginx["env"] = [{"name": "STAGE1_DROP", "value": "drop"}, keep]
    sidecar = {"name": "sidecar", "image": "nginx:1.28", "command": ["sleep", "3600"]}
    containers.append(sidecar)
    write(directory / "web-env.json", obj)

    nginx["env"] = [keep, {"name": "STAGE1_ADD", "value": "added"}]
    containers[:] = [sidecar, nginx]
    write(directory / "web-reordered.json", obj)
    obj["spec"]["replicas"] += 1
    write(directory / "web-dry-run.json", obj)


def items(path, kind):
    return [obj for obj in read(path)["items"] if obj["kind"] == kind]


def ready_pod(pod):
    status = pod["status"]
    assert status["phase"] == "Running" and status["podIP"], pod
    assert not pod["metadata"].get("deletionTimestamp"), pod
    assert pod["spec"]["nodeName"] == "stage1-check", pod
    statuses = status["containerStatuses"]
    assert {cs["name"] for cs in statuses} == {c["name"] for c in pod["spec"]["containers"]}, pod
    assert all(cs["ready"] and "running" in cs["state"] for cs in statuses), statuses
    return status


def workload(path, replicas, main, image):
    replicas = int(replicas)
    (deployment,) = items(path, "Deployment")
    assert deployment["metadata"]["name"] == "web", deployment
    assert deployment["spec"]["replicas"] == deployment["status"]["readyReplicas"] == replicas, deployment
    pods = items(path, "Pod")
    assert len(pods) == replicas, pods
    assert len({ready_pod(pod)["podIP"] for pod in pods}) == replicas, pods
    sets = items(path, "ReplicaSet")
    (active,) = [rs for rs in sets if rs["spec"]["replicas"] > 0]
    assert active["spec"]["replicas"] == active["status"]["readyReplicas"] == replicas, active
    assert deployment["metadata"]["uid"] in {ref["uid"] for ref in active["metadata"]["ownerReferences"]}, active
    uid = active["metadata"]["uid"]
    for pod in pods:
        assert uid in {ref["uid"] for ref in pod["metadata"]["ownerReferences"]}, pod
        assert {c["name"]: c["image"] for c in pod["spec"]["containers"]}[main] == image, pod
    print(uid)


def replacement(before, after, removed):
    old = {pod["metadata"]["uid"] for pod in items(before, "Pod")}
    new = {pod["metadata"]["uid"] for pod in items(after, "Pod")}
    assert old - new == {removed} and len(new - old) == 1, (old, new)


def strategic(manifest, live):
    expected = read(manifest)["spec"]["template"]["spec"]["containers"]
    (deployment,) = items(live, "Deployment")
    templates = [deployment["spec"]["template"]["spec"]] + [pod["spec"] for pod in items(live, "Pod")]
    for spec in templates:
        # Defaults can add fields, so compare the order, images and complete env lists.
        actual = spec["containers"]
        assert [(c["name"], c["image"], c.get("env", [])) for c in actual] == [
            (c["name"], c["image"], c.get("env", [])) for c in expected
        ], actual


def restarted(before, after, name, *outage):
    old, new = read(before), read(after)
    assert old["metadata"]["uid"] == new["metadata"]["uid"], new
    assert old["status"]["podIP"] == ready_pod(new)["podIP"], new
    (prev,) = [cs for cs in old["status"]["containerStatuses"] if cs["name"] == name]
    (curr,) = [cs for cs in new["status"]["containerStatuses"] if cs["name"] == name]
    assert curr["restartCount"] == prev["restartCount"] + 1, curr
    assert curr["containerID"].startswith("containerd://") and curr["containerID"] != prev["containerID"], curr
    terminated = curr["lastState"]["terminated"]
    assert terminated["containerID"] == prev["containerID"] and terminated["exitCode"] == 137, terminated
    finished = datetime.datetime.fromisoformat(terminated["finishedAt"].replace("Z", "+00:00"))
    if outage:
        killed, resumed = map(int, outage)
        assert killed <= int(finished.timestamp()) <= resumed, (killed, finished, resumed)


def events(path, description, name, uid):
    events = read(path)["items"]
    for event in events:
        ref = event["involvedObject"]
        assert (ref["kind"], ref["namespace"], ref["name"], ref["uid"]) == ("Pod", "default", name, uid), event
    reasons = {ev["reason"] for ev in events if ev.get("message")}
    expected = {"Scheduled", "Started"}
    assert expected <= reasons, f"Missing {expected - reasons} Events for Pod {name} ({uid})"
    table = Path(description).read_text().split("Events:", 1)[1]
    assert all(re.search(rf"(?m)^\s*Normal\s+{reason}\s", table) for reason in expected), table


def endpoints(pods, slices):
    ready_ips = {ready_pod(pod)["podIP"] for pod in items(pods, "Pod")}
    assert ready_ips, pods
    endpoints = [ep for sl in items(slices, "EndpointSlice") for ep in sl["endpoints"]]
    addresses = [ip for ep in endpoints if ep["conditions"]["ready"] for ip in ep["addresses"]]
    assert len(addresses) == len(ready_ips) and set(addresses) == ready_ips, (ready_ips, endpoints)


def identity(path, tasks):
    """API state plus live task PIDs distinguishes adoption from replacement."""
    state = {}
    container_ids = set()
    for obj in read(path)["items"]:
        meta = obj["metadata"]
        record = {"uid": meta["uid"], "spec": obj["spec"]}
        state[obj["kind"] + "/" + meta["name"]] = record
        if obj["kind"] == "Pod":
            status = ready_pod(obj)
            record["ip"] = status["podIP"]
            record["containers"] = {}
            for cs in status["containerStatuses"]:
                cid = cs["containerID"]
                assert cid.startswith("containerd://"), cs
                container_ids.add(cid.removeprefix("containerd://"))
                record["containers"][cs["name"]] = [cid, cs["restartCount"]]
    assert "Deployment/web" in state and "Service/web" in state and container_ids, state
    running = dict(line.split() for line in Path(tasks).read_text().splitlines())
    assert running.keys() == container_ids, (running, container_ids)
    print(json.dumps({"objects": state, "taskPIDs": running}, sort_keys=True, indent=2))


def kill_server(metadata, binary, run_dir):
    """A pidfd keeps the signal tied to this process even if its PID is reused."""
    pid = read(metadata)["pid"]
    assert type(pid) is int and pid > 1, pid
    fd = os.pidfd_open(pid)
    try:
        assert os.path.samefile(f"/proc/{pid}/exe", binary), pid
        args = Path(f"/proc/{pid}/cmdline").read_bytes().decode().rstrip("\0").split("\0")
        assert args[1] == "server", args
        separate = any(args[i:i + 2] == ["--run-dir", run_dir] for i in range(2, len(args)))
        assert separate or f"--run-dir={run_dir}" in args, args
        signal.pidfd_send_signal(fd, signal.SIGKILL)
        poll = select.poll()
        poll.register(fd, select.POLLIN)
        assert poll.poll(10000), f"Server {pid} did not exit after SIGKILL"
        print(f"Sent SIGKILL to fixture server PID {pid} and observed exit")
    finally:
        os.close(fd)


def allocation_rows(db):
    with sqlite3.connect(Path(db).as_uri() + "?mode=ro", uri=True) as conn:
        for row in conn.execute("SELECT kind, owner_uid, ip FROM ip_alloc ORDER BY kind, owner_uid"):
            print(*row, sep="\t")


def pod_network(directory, count):
    directory = Path(directory)
    netns = sorted(path.name for path in directory.iterdir()) if directory.is_dir() else []
    # sysfs belongs to the namespace where it was mounted. With nsenter --net,
    # /sys/class/net can still describe the outer host, including unrelated Pods.
    # ip uses netlink in our current network namespace, just as sw itself does.
    links = json.loads(subprocess.check_output(["ip", "-j", "link", "show"], text=True, timeout=10))
    veth = sorted(link["ifname"] for link in links if re.fullmatch(r"sw[0-9a-f]{8}", link["ifname"]))
    print(json.dumps({"netns": netns, "veth": veth}, indent=2))
    assert len(netns) == int(count) and veth == sorted("sw" + uid[:8] for uid in netns), (netns, veth)


def watch_event(path, event_type, uid, app):
    # kubectl prints successive JSON objects, sometimes leaving a partial last one.
    decoder = json.JSONDecoder()
    rest = Path(path).read_text().lstrip()
    while rest:
        try:
            event, end = decoder.raw_decode(rest)
        except json.JSONDecodeError:
            break
        meta = event.get("object", {}).get("metadata", {})
        if event.get("type") == event_type and meta.get("uid") == uid and meta.get("labels", {}).get("app") == app:
            return
        rest = rest[end:].lstrip()
    raise SystemExit(f"Missing {event_type} watch event for {uid} with app={app}")


checks = {
    "manifests": manifests,
    "workload": workload,
    "replacement": replacement,
    "strategic": strategic,
    "restarted": restarted,
    "events": events,
    "endpoints": endpoints,
    "identity": identity,
    "kill-server": kill_server,
    "allocations": allocation_rows,
    "pod-network": pod_network,
    "watch-event": watch_event,
}

if __name__ == "__main__":
    checks[sys.argv[1]](*sys.argv[2:])
