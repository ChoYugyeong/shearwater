#!/usr/bin/env bash
# M1: real kubectl, isolated API state, no root and no containers.
# bash hack/m1-check.sh
# WIN_KUBECTL=/mnt/c/ProgramData/chocolatey/bin/kubectl.exe bash hack/m1-check.sh
# M0 regression is separate: WIN_KUBECTL=... bash hack/m0-check.sh
# Evidence is retained under ~/.cache/shearwater-dev/m1-<time>-<pid>.
set -uo pipefail
cd "$(dirname "$0")/.."
. hack/env.sh
RUN="$HOME/.cache/shearwater-dev/m1-$(date +%Y%m%d-%H%M%S)-$$"
PORT="${SW_PORT:-16443}"
T=hack/testdata/m1
A="$PWD/$T/assert.py"
PASS=0 FAIL=0 N=0 SRV=""
BG=()
mkdir -p "$RUN"
# Refuse a busy port rather than testing another agent's server.
python3 - "$PORT" <<'PY' || exit 1
import socket, sys
s = socket.socket()
s.bind(("127.0.0.1", int(sys.argv[1])))
s.close()
PY
cleanup() {
  local p
  for p in "${BG[@]}"; do
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  done
  if [ -n "$SRV" ]; then
    kill "$SRV" 2>/dev/null || true
    wait "$SRV" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
go build -o "$RUN/sw" ./cmd/sw || exit 1
git rev-parse HEAD > "$RUN/source-commit.txt"
git status --short > "$RUN/source-status.txt"
"$RUN/sw" server --runtime=none --listen "127.0.0.1:$PORT" --data-dir "$RUN/data" \
  --config-dir "$RUN/etc" --trace "$RUN/trace.jsonl" 2>"$RUN/server.log" &
SRV=$!
ready=0
for _ in $(seq 80); do
  kill -0 "$SRV" 2>/dev/null || break
  if curl -sk --max-time 1 "https://127.0.0.1:$PORT/readyz" | grep -q ok; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" = 1 ] || { cat "$RUN/server.log"; exit 1; }
"$RUN/sw" kubeconfig --config-dir "$RUN/etc" --server "https://127.0.0.1:$PORT" > "$RUN/kubeconfig"
export KUBECONFIG="$RUN/kubeconfig"
k() { timeout --kill-after=3s 35s kubectl --cache-dir="$RUN/discovery" "$@"; }
WK() { timeout --kill-after=3s 35s "$WIN_KUBECTL" --kubeconfig "$(wslpath -w "$RUN/kubeconfig")" --cache-dir "$(wslpath -w "$RUN/win-cache")" "$@"; }
trace_count() { if [ -f "$RUN/trace.jsonl" ]; then wc -l < "$RUN/trace.jsonl"; else echo 0; fi; }
check() {
  local mode="$1" name="$2" want="$3"; shift 3
  local code=0 good=0 first last command output
  N=$((N+1))
  first=$(trace_count)
  printf -v command '%q ' "$@"
  printf -v output '%s/check-%03d.log' "$RUN" "$N"
  "$@" >"$output" 2>&1 || code=$?
  last=$(trace_count)
  case "$mode" in
    ok) [ "$code" = 0 ] && good=1 ;;
    no) [ "$code" != 0 ] && good=1 ;;
    has) [ "$code" = 0 ] && grep -qF -- "$want" "$output" && good=1 ;;
    reject) [ "$code" != 0 ] && grep -qF -- "$want" "$output" && good=1 ;;
    eq) [ "$code" = 0 ] && [ "$(tr -d '\r' < "$output")" = "$want" ] && good=1 ;;
  esac
  python3 "$A" record "$RUN" "$N" "$name" "$good" "$code" "$first" "$last" "$command"
  if [ "$good" = 1 ]; then PASS=$((PASS+1)); echo "PASS $name"
  else FAIL=$((FAIL+1)); echo "FAIL $name (exit $code, $mode '$want')"; sed 's/^/     /' "$output"; fi
  return 0
}
ok() { local name="$1"; shift; check ok "$name" "" "$@"; }
no() { local name="$1"; shift; check no "$name" "" "$@"; }
has() { local name="$1" want="$2"; shift 2; check has "$name" "$want" "$@"; }
reject() { local name="$1" want="$2"; shift 2; check reject "$name" "$want" "$@"; }
eq() { local name="$1" want="$2"; shift 2; check eq "$name" "$want" "$@"; }
columns() {
  local file="$RUN/table-$1.txt"; shift
  local resource="$1"; shift
  k get "$resource" > "$file" && python3 "$A" columns "$file" "$@"
}
until_text() {
  local file="$1" text="$2"
  for _ in $(seq 100); do
    grep -qF -- "$text" "$file" 2>/dev/null && return 0
    sleep 0.1
  done
  cat "$file"; return 1
}
stop_bg() {
  local target="$1" p
  kill "$target" 2>/dev/null || true
  wait "$target" 2>/dev/null || true
  local remaining=()
  for p in "${BG[@]}"; do [ "$p" = "$target" ] || remaining+=("$p"); done
  BG=("${remaining[@]}")
}
wait_bg() {
  local target="$1" code=0 p
  wait "$target" || code=$?
  local remaining=()
  for p in "${BG[@]}"; do [ "$p" = "$target" ] || remaining+=("$p"); done
  BG=("${remaining[@]}")
  return "$code"
}
rev() { k get ns -o jsonpath='{.metadata.resourceVersion}'; }
apply_step() {
  local file="$1" desc="$2" field="$3" want="$4" before
  ok "apply $desc" k apply -f "$T/$file.json"
  eq "$desc: final object" "$want" k get deploy m1-web -o "jsonpath=$field"
  before=$(rev)
  has "$desc: repeated apply unchanged" unchanged k apply -f "$T/$file.json"
  eq "$desc: unchanged preserves revision" "$before" rev
}
k version --client -o json > "$RUN/linux-version.json"
echo "run dir: $RUN"
echo "== fixtures and Table"
ok "create pod" k create -f "$T/pod.json"
ok "create service" k create -f "$T/service.json"
ok "create namespace" k create -f "$T/namespace.json"
apply_step apply-00 initial '{.spec.template.spec.containers[*].name}' 'main side'
ok "pod columns" columns pods pods NAME READY STATUS RESTARTS AGE
ok "deployment columns" columns deploy deploy NAME READY UP-TO-DATE AVAILABLE AGE
ok "service columns" columns svc svc NAME TYPE CLUSTER-IP 'PORT(S)' AGE
ok "namespace columns" columns ns ns NAME STATUS AGE
has "pods wide includes NODE" NODE k get pods -o wide
has "deployments wide includes CONTAINERS" CONTAINERS k get deploy -o wide
has "services wide includes SELECTOR" SELECTOR k get svc -o wide
has "namespaces wide" m1-ns k get ns -o wide

echo "== apply strategic merge"
apply_step apply-01-add 'add container' '{.spec.template.spec.containers[*].name}' 'main side extra'
apply_step apply-02-delete 'delete container' '{.spec.template.spec.containers[*].name}' 'main extra'
apply_step apply-03-order 'reorder containers' '{.spec.template.spec.containers[*].name}' 'extra main'
apply_step apply-04-env 'delete env' '{.spec.template.spec.containers[?(@.name=="main")].env[*].name}' KEEP
apply_step apply-05-strategy 'RollingUpdate to Recreate' '{.spec.strategy.type}' Recreate
eq "rollingUpdate removed" '' k get deploy m1-web -o jsonpath='{.spec.strategy.rollingUpdate}'
apply_step apply-06-volume 'emptyDir to hostPath' '{.spec.template.spec.volumes[0].hostPath.path}' /tmp/shearwater-m1
eq "emptyDir removed" '' k get deploy m1-web -o jsonpath='{.spec.template.spec.volumes[0].emptyDir}'

echo "== edit / patch / label / annotate / set image"
edit_deployment() {
  # In this WSL setup edit stalls before PATCH under a non-interactive Windows
  # pipe. Give only edit a PTY; sed remains the editor.
  local command
  printf -v command '%q ' kubectl --cache-dir="$RUN/discovery" edit deploy m1-web
  env KUBE_EDITOR="sed -i s/nginx:1.27/nginx:1.28/g" timeout --kill-after=3s 25s \
    script --quiet --return --command "$command" "$RUN/edit.typescript" </dev/null
}
ok "edit via sed" edit_deployment
eq "edit changed image" nginx:1.28 k get deploy m1-web -o jsonpath='{.spec.template.spec.containers[?(@.name=="main")].image}'
ok "strategic patch" k patch deploy m1-web --type=strategic -p '{"spec":{"template":{"spec":{"containers":[{"name":"main","image":"nginx:1.29"}]}}}}'
eq "strategic keeps extra container" 'extra main' k get deploy m1-web -o jsonpath='{.spec.template.spec.containers[*].name}'
ok "merge patch" k patch deploy m1-web --type=merge -p '{"spec":{"replicas":3}}'
eq "merge changed replicas" 3 k get deploy m1-web -o jsonpath='{.spec.replicas}'
ok "json patch" k patch deploy m1-web --type=json -p '[{"op":"test","path":"/spec/replicas","value":3},{"op":"replace","path":"/spec/replicas","value":4}]'
eq "json changed replicas" 4 k get deploy m1-web -o jsonpath='{.spec.replicas}'
ok "label" k label pod m1-pod stage=m1
eq "label persisted" m1 k get pod m1-pod -o jsonpath='{.metadata.labels.stage}'
ok "annotate" k annotate pod m1-pod note=m1
eq "annotation persisted" m1 k get pod m1-pod -o jsonpath='{.metadata.annotations.note}'
ok "set image" k set image deploy/m1-web main=nginx:1.30
eq "set image persisted" nginx:1.30 k get deploy m1-web -o jsonpath='{.spec.template.spec.containers[?(@.name=="main")].image}'

echo "== typed Protobuf creates"
ok "create deployment" k create deployment m1-proto-deploy --image=nginx:1.27
ok "create service clusterip" k create service clusterip m1-proto-svc --tcp=80:8080
ok "create namespace typed" k create namespace m1-proto-ns
ok "debug copy Pod" k debug m1-pod --copy-to=m1-copy --set-image=main=nginx:1.28 --profile=baseline --share-processes=false
eq "debug copy image" nginx:1.28 k get pod m1-copy -o jsonpath='{.spec.containers[0].image}'
for name in m1-proto-deploy m1-proto-svc m1-proto-ns m1-copy; do
  ok "$name really sent Protobuf" python3 "$A" protobuf "$RUN/trace.jsonl" "$name"
done

echo "== warning and unsupported SSA"
create_warning() {
  local rc=0
  k create -f "$T/warning.json" > "$RUN/warning.txt" 2>&1 || rc=$?
  cat "$RUN/warning.txt"; return "$rc"
}
has "stored-only field Warning" 'Warning:' create_warning
has "warning names livenessProbe" livenessProbe cat "$RUN/warning.txt"
reject "server-side apply rejected with guidance" 'apply' k apply --server-side -f "$T/apply-06-volume.json"
ok "SSA trace is 415" python3 - "$RUN/trace.jsonl" <<'PY'
import json,sys
e=[json.loads(s) for s in open(sys.argv[1]) if 'application/apply-patch+yaml' in s]
assert e and e[-1]["status"] == 415, e
print(json.dumps(e[-1]))
PY

echo "== Table watch and selector events"
timeout --kill-after=3s 25s kubectl --cache-dir="$RUN/discovery" get pods -w > "$RUN/watch-table.txt" 2>"$RUN/watch-table.err" &
TABLE_PID=$!; BG+=("$TABLE_PID")
ok "Table watch starts with initial rows" until_text "$RUN/watch-table.txt" m1-pod
ok "create watched object" k create -f "$T/watch.json"
ok "Table watch observes creation" until_text "$RUN/watch-table.txt" m1-watch
timeout --kill-after=3s 25s kubectl --cache-dir="$RUN/discovery" get pods -w -l track=yes --output-watch-events -o json > "$RUN/watch-events.json" 2>"$RUN/watch-events.err" &
EVENT_PID=$!; BG+=("$EVENT_PID")
ok "selector watch starts with matching object" until_text "$RUN/watch-events.json" m1-watch
ok "remove selector label" k label pod m1-watch track-
ok "selector watch observes leaving selector" until_text "$RUN/watch-events.json" DELETED
ok "re-enter selector" k label pod m1-watch track=yes
ok "delete watched object" k delete pod m1-watch --timeout=10s
events_ready() {
  for _ in $(seq 100); do
    python3 "$A" events "$RUN/watch-events.json" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  cat "$RUN/watch-events.json"; cat "$RUN/watch-events.err"; return 1
}
ok "selector event sequence and old object/new RV" events_ready
stop_bg "$EVENT_PID"
stop_bg "$TABLE_PID"
ok "Table watch prints creation, update and deletion" python3 - "$RUN/watch-table.txt" <<'PY'
import pathlib,sys
rows=[s for s in pathlib.Path(sys.argv[1]).read_text().splitlines() if s.split() and s.split()[0]=="m1-watch"]
assert len(rows)>=4, rows
print("\n".join(rows))
PY

echo "== wait and finalizer deletion"
ok "wait --for=jsonpath" k wait --for=jsonpath='{.status.phase}'=Pending pod/m1-pod --timeout=10s
ok "create held Pod" k create -f "$T/finalizer.json"
timeout --kill-after=3s 20s kubectl --cache-dir="$RUN/discovery" delete pod m1-held --timeout=15s > "$RUN/delete-held.log" 2>&1 &
DELETE_PID=$!; BG+=("$DELETE_PID")
held_marked() {
  for _ in $(seq 100); do
    local stamp
    stamp=$(k get pod m1-held -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) || return 1
    [ -n "$stamp" ] && return 0
    sleep 0.1
  done
  return 1
}
ok "delete marks object before finalization" held_marked
timeout --kill-after=3s 20s kubectl --cache-dir="$RUN/discovery" wait --for=delete pod/m1-held --timeout=15s > "$RUN/wait-delete.log" 2>&1 &
WAIT_PID=$!; BG+=("$WAIT_PID")
sleep 1
ok "delete is still waiting" kill -0 "$DELETE_PID"
ok "wait is still waiting" kill -0 "$WAIT_PID"
ok "clear finalizer by patch" k patch pod m1-held --type=merge -p '{"metadata":{"finalizers":[]}}'
finish_delete() { local rc=0; wait_bg "$DELETE_PID" || rc=$?; cat "$RUN/delete-held.log"; return "$rc"; }
finish_wait() { local rc=0; wait_bg "$WAIT_PID" || rc=$?; cat "$RUN/wait-delete.log"; return "$rc"; }
ok "kubectl delete finishes after finalizer removal" finish_delete
ok "kubectl wait delete finishes" finish_wait
reject "held Pod is gone" NotFound k get pod m1-held
ok "delete/wait established a watch" python3 "$A" request "$RUN/trace.jsonl" GET /pods watch=true fieldSelector=metadata.name=m1-held

echo "== watch-list initial bookmark"
RAW='/apis/apps/v1/namespaces/default/deployments?watch=true&sendInitialEvents=true&resourceVersionMatch=NotOlderThan&allowWatchBookmarks=true&fieldSelector=metadata.name%3Dm1-web&timeoutSeconds=2'
raw_watch() { k get --raw "$RAW" > "$RUN/watch-list.json" && cat "$RUN/watch-list.json"; }
ok "watch-list request finishes normally" raw_watch
ok "initial-events-end bookmark after ADDED" python3 "$A" bookmark "$RUN/watch-list.json"
ok "watch-list request trace" python3 "$A" request "$RUN/trace.jsonl" GET /deployments sendInitialEvents=true resourceVersionMatch=NotOlderThan

if [ -n "${WIN_KUBECTL:-}" ]; then
  echo "== Windows kubectl"
  WK version --client -o json > "$RUN/windows-version.json"
  ok "windows: create deployment" WK create deployment m1-win-deploy --image=nginx:1.27
  ok "windows: create service" WK create service clusterip m1-win-svc --tcp=80:8080
  ok "windows: create namespace" WK create namespace m1-win-ns
  ok "windows: debug copy" WK debug m1-pod --copy-to=m1-win-copy --set-image=main=nginx:1.28 --profile=baseline --share-processes=false
  has "windows: Table" READY WK get pods
  has "windows: wide" NODE WK get pods -o wide
  ok "windows: apply" WK apply -f "$(wslpath -w "$PWD/$T/apply-00.json")"
  has "windows: repeated apply unchanged" unchanged WK apply -f "$(wslpath -w "$PWD/$T/apply-00.json")"
  ok "windows: apply container addition" WK apply -f "$(wslpath -w "$PWD/$T/apply-01-add.json")"
  eq "windows: apply result" 'main side extra' WK get deploy m1-web -o jsonpath='{.spec.template.spec.containers[*].name}'
  ok "windows: wait jsonpath" WK wait --for=jsonpath='{.status.phase}'=Pending pod/m1-pod --timeout=10s
  ok "windows: delete waits" WK delete pod m1-win-copy --timeout=10s
fi
python3 "$A" report "$RUN"
echo
echo "$PASS passed, $FAIL failed"
echo "run dir: $RUN"
[ "$FAIL" = 0 ]
