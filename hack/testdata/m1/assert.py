#!/usr/bin/env python3
"""Assertions and evidence for m1-check.sh; standard library only."""
import base64
import json
import pathlib
import sys
from urllib.parse import parse_qs

def stream(path):
    raw = pathlib.Path(path).read_text(errors="replace")
    dec = json.JSONDecoder()
    pos = 0
    while pos < len(raw):
        while pos < len(raw) and raw[pos].isspace():
            pos += 1
        if pos == len(raw):
            break
        obj, pos = dec.raw_decode(raw, pos)
        yield obj

def trace(path):
    p = pathlib.Path(path)
    return [json.loads(s) for s in p.read_text().splitlines() if s] if p.exists() else []

mode, *args = sys.argv[1:]
if mode == "record":
    run, number, name, outcome, code, first, last, command = args
    entry = dict(number=int(number), name=name, passed=outcome == "1",
                 exit_code=int(code), trace_start=int(first), trace_end=int(last),
                 command=command, output=f"check-{int(number):03d}.log")
    with open(pathlib.Path(run) / "checks.jsonl", "a") as f:
        f.write(json.dumps(entry) + "\n")
elif mode == "columns":
    header = pathlib.Path(args[0]).read_text().splitlines()[0].split()
    assert all(c in header for c in args[1:]), (header, args[1:])
elif mode == "events":
    selected = [e for e in stream(args[0]) if e.get("object", {}).get("metadata", {}).get("name") == "m1-watch"]
    types = [e["type"] for e in selected]
    assert types == ["ADDED", "DELETED", "ADDED", "DELETED"], types
    rv = [int(e["object"]["metadata"]["resourceVersion"]) for e in selected]
    assert rv == sorted(set(rv)), rv
    assert selected[1]["object"]["metadata"]["labels"]["track"] == "yes", selected[1]
    print("ADDED -> selector DELETED -> ADDED -> actual DELETED; increasing RV, old labels retained")
elif mode == "bookmark":
    events = list(stream(args[0]))
    positions = [i for i, e in enumerate(events) if e.get("type") == "BOOKMARK"
                 and e["object"]["metadata"].get("annotations", {}).get("k8s.io/initial-events-end") == "true"]
    assert len(positions) == 1, events
    assert positions[0] > 0 and all(e["type"] == "ADDED" for e in events[:positions[0]]), events
    print("initial ADDED objects precede exactly one initial-events-end BOOKMARK")
elif mode == "request":
    path, method, suffix, *query = args
    candidates = [e for e in trace(path) if e["method"] == method and e["path"].endswith(suffix)]
    for term in query:
        key, value = term.split("=", 1)
        candidates = [e for e in candidates if value in parse_qs(e.get("query", "")).get(key, [])]
    assert candidates, (method, suffix, query)
    print(json.dumps(candidates[-1], ensure_ascii=False))
elif mode == "protobuf":
    path, name = args
    events = [e for e in trace(path) if e["method"] == "POST" and e["status"] == 201
              and e.get("contentType") == "application/vnd.kubernetes.protobuf"
              and name in (e.get("body", "") or base64.b64decode(e.get("bodyBase64", "")).decode("utf8", "replace"))]
    assert events, name
    print(json.dumps(events[-1], ensure_ascii=False))
elif mode == "report":
    run = pathlib.Path(args[0])
    checks = trace(run / "checks.jsonl")
    requests = trace(run / "trace.jsonl")
    fence = chr(96) * 3
    lines = ["# M1 kubectl check evidence", "", f"Run: {run}", "",
             f"{sum(e['passed'] for e in checks)} passed, {sum(not e['passed'] for e in checks)} failed", ""]
    for c in checks:
        if c["passed"]:
            continue
        lines += [f"## FAIL {c['number']}: {c['name']}", "", "Command:", fence+"sh", c["command"], fence,
                  f"Exit: {c['exit_code']}", "", "Output:", fence, (run/c["output"]).read_text(errors="replace"), fence,
                  "", "Requests completed during this check:", fence+"jsonl"]
        lines += [json.dumps(e, ensure_ascii=False) for e in requests[c["trace_start"]:c["trace_end"]]]
        lines += [fence, ""]
    (run / "report.md").write_text("\n".join(lines) + "\n")
    print(run / "report.md")
else:
    raise SystemExit("unknown mode: " + mode)
