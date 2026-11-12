"""Check Pod counts at every change after an initial consistent list."""

import json
from pathlib import Path
import sys
import time


def release(pod):
    for container in pod["spec"]["containers"]:
        for var in container.get("env", []):
            if var["name"] == "RELEASE":
                return var["value"]
    return None


def ready(pod):
    return not pod["metadata"].get("deletionTimestamp") and any(
        c["type"] == "Ready" and c["status"] == "True"
        for c in pod.get("status", {}).get("conditions", [])
    )


def check(initial_path, watch_path, release_name, replicas, surge, unavailable):
    initial = json.loads(initial_path.read_text(encoding="utf-8"))
    pods = {p["metadata"]["uid"]: p for p in initial["items"]}
    initial_uids = set(pods)
    if len(pods) != replicas or not all(ready(p) for p in pods.values()):
        raise SystemExit("watch started without the full initial Ready replica count")
    revision = int(initial["metadata"]["resourceVersion"])
    deadline = time.monotonic() + 15
    max_live = min_ready = replicas
    terminating_seen = False
    events = 0
    with watch_path.open(encoding="utf-8") as watch:
        while time.monotonic() < deadline:
            pos = watch.tell()
            line = watch.readline()
            if not line.endswith("\n"):
                watch.seek(pos)
                time.sleep(0.1)
                continue
            event = json.loads(line)
            kind, pod = event["type"], event["object"]
            if kind == "ERROR":
                raise SystemExit(f"watch returned {pod}")
            if kind == "BOOKMARK":
                continue
            uid = pod["metadata"]["uid"]
            current = int(pod["metadata"]["resourceVersion"])
            if current <= revision:
                raise SystemExit(f"watch resourceVersion moved from {revision} to {current}")
            revision = current
            if kind == "DELETED":
                pods.pop(uid, None)
            elif kind in ("ADDED", "MODIFIED"):
                pods[uid] = pod
            else:
                raise SystemExit(f"unexpected watch event type {kind}")
            events += 1
            live = [p for p in pods.values() if p.get("status", {}).get("phase") not in ("Failed", "Succeeded")]
            available = sum(ready(p) for p in live)
            max_live = max(max_live, len(live))
            min_ready = min(min_ready, available)
            terminating_seen |= any(p["metadata"].get("deletionTimestamp") for p in live)
            if len(live) > replicas + surge or available < replicas - unavailable:
                raise SystemExit(f"budget exceeded at resourceVersion={revision}: live={len(live)}, Ready={available}")
            if (len(live) == available == replicas and terminating_seen and not initial_uids.issuperset(pods)
                    and all(release(p) == release_name for p in live)):
                if surge > 0 and max_live <= replicas:
                    raise SystemExit("watch did not observe the expected rollout overlap")
                print(f"Rollout watch verified {events} events: max live={max_live}, min Ready={min_ready}")
                return
    raise SystemExit("watch never reached the fully updated replica set")


if __name__ == "__main__":
    initial, watch, target, replicas, surge, unavailable = sys.argv[1:]
    check(Path(initial), Path(watch), target, int(replicas), int(surge), int(unavailable))
