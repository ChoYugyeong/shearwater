#!/usr/bin/env bash
# Run as WSL root with sw built unprivileged after sourcing hack/env.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
[[ $(id -u) == 0 && $# == 2 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] || {
  echo "usage (root): bash hack/m4-check.sh /absolute/sw /absolute/kubectl" >&2
  exit 2
}
sw=$1 kubectl=$2
port=${SW_M4_PORT:-16447}
socket=${SW_CONTAINERD_SOCKET:-/run/containerd/containerd.sock}
empty_runtime() {
  local ids
  ids=$(ctr --address "$socket" --namespace shearwater containers list -q) && [[ -z $ids ]]
}
empty_runtime || {
  echo "shearwater containerd namespace is busy or unavailable" >&2
  exit 1
}
mkdir -p /var/lib/shearwater/check /run/shearwater
run=$(mktemp -d /var/lib/shearwater/check/m4.XXXXXXXX)
net=$(mktemp -d /run/shearwater/m4.XXXXXXXX)
echo "Evidence: $run"
watcher=
k() { KUBECONFIG="$run/kubeconfig" "$kubectl" --request-timeout=90s "$@"; }
"$sw" server --runtime=containerd --node-name=m4-check --listen="127.0.0.1:$port" \
  --containerd-socket="$socket" \
  --data-dir="$run/data" --config-dir="$run/etc" --run-dir="$net" --log-dir="$run/log" \
  --trace="$run/trace.jsonl" >"$run/server.log" 2>&1 &
srv=$!
stop_watch() {
  if [[ -n $watcher ]]; then
    kill "$watcher" 2>/dev/null || true
    wait "$watcher" 2>/dev/null || true
    watcher=
  fi
}
cleanup() {
  local status=$?
  stop_watch
  if [[ -s $run/kubeconfig ]] && kill -0 "$srv" 2>/dev/null; then
    k delete deployment m4-web --ignore-not-found --cascade=foreground --wait=false >/dev/null 2>&1 || true
    k delete rs m4-standalone --ignore-not-found --cascade=foreground --wait=false >/dev/null 2>&1 || true
    for _ in {1..15}; do
      if empty_runtime; then break; fi
      sleep 1
    done
  fi
  kill "$srv" 2>/dev/null || true
  wait "$srv" || true
  if (( status != 0 )); then tail -40 "$run/server.log" >&2; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
until_ok() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    if (( SECONDS >= deadline )); then
      echo "Timed out: $*" >&2
      return 1
    fi
    sleep 1
  done
}
until_ok 10 test -s "$run/etc/admin.token"
kill -0 "$srv"
"$sw" kubeconfig --config-dir="$run/etc" --server="https://127.0.0.1:$port" >"$run/kubeconfig"
node_exists() { k get node m4-check >/dev/null 2>&1; }
until_ok 15 node_exists
k wait --for=condition=Ready node/m4-check --timeout=25s

start_watch() {
  local name=$1 cursor
  k get --raw '/api/v1/namespaces/default/pods?labelSelector=app%3Dm4-web' >"$run/$name-initial.json"
  cursor=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["metadata"]["resourceVersion"])' "$run/$name-initial.json")
  KUBECONFIG="$run/kubeconfig" "$kubectl" --request-timeout=90s get --raw \
    "/api/v1/namespaces/default/pods?watch=true&resourceVersion=$cursor&labelSelector=app%3Dm4-web&timeoutSeconds=80" \
    >"$run/$name-watch.jsonl" 2>"$run/$name-watch.err" &
  watcher=$!
}
verify_watch() {
  local name=$1
  shift
  python3 hack/testdata/m4/watch-rollout.py "$run/$name-initial.json" "$run/$name-watch.jsonl" "$@"
  stop_watch
}

k create -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: m4-web
  annotations:
    kubernetes.io/change-cause: initial
spec:
  replicas: 3
  revisionHistoryLimit: 2
  selector:
    matchLabels:
      app: m4-web
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 0
  template:
    metadata:
      labels:
        app: m4-web
    spec:
      terminationGracePeriodSeconds: 3
      containers:
      - name: app
        image: busybox:1.36
        command: [sh, -ec, "trap '' TERM; while :; do sleep 1; done"]
        env:
        - name: RELEASE
          value: v1
YAML
k rollout status deployment/m4-web --timeout=55s
initial_rs=$(k get rs -l app=m4-web -o jsonpath='{.items[0].metadata.uid}')
start_watch rolling
k patch deployment/m4-web --type=strategic -p \
  '{"metadata":{"annotations":{"kubernetes.io/change-cause":"updated"}},"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":"RELEASE","value":"v2"}]}]}}}}'
k rollout status deployment/m4-web --timeout=55s
verify_watch rolling v2 3 1 0
k rollout status deployment/m4-web --revision=2 --watch=false
k rollout history deployment/m4-web | tee "$run/history-v2.txt"
grep -Eq '^1[[:space:]]+initial$' "$run/history-v2.txt"
grep -Eq '^2[[:space:]]+updated$' "$run/history-v2.txt"
k rollout history deployment/m4-web --revision=1 >"$run/revision-1.txt"
grep -Eq 'RELEASE:[[:space:]]+v1' "$run/revision-1.txt"

scale_path=/apis/apps/v1/namespaces/default/deployments/m4-web/scale
k get --raw "$scale_path" >"$run/scale-before.json"
python3 - "$run/scale-before.json" <<'PY'
import json, sys
scale = json.load(open(sys.argv[1]))
assert (scale["apiVersion"], scale["kind"]) == ("autoscaling/v1", "Scale")
assert scale["spec"]["replicas"] == scale["status"]["replicas"] == 3
assert scale["status"]["selector"] == "app=m4-web"
PY
k scale deployment/m4-web --replicas=4 --timeout=35s
k rollout status deployment/m4-web --timeout=40s
k scale deployment/m4-web --replicas=5 --dry-run=server
[[ $(k get deployment/m4-web -o jsonpath='{.spec.replicas}') == 4 ]]
k scale deployment/m4-web --current-replicas=4 --replicas=2 --timeout=35s
if k scale deployment/m4-web --current-replicas=4 --replicas=1 >"$run/failed-precondition.txt" 2>&1; then
  echo "scale accepted the wrong current replica count" >&2
  exit 1
fi
[[ $(k get deployment/m4-web -o jsonpath='{.spec.replicas}') == 2 ]]
k rollout status deployment/m4-web --revision=2 --watch=false
echo "Verified Deployment /scale and kubectl preconditions"

k rollout undo deployment/m4-web --to-revision=1
k rollout status deployment/m4-web --timeout=55s
k rollout status deployment/m4-web --revision=3 --watch=false
k rollout history deployment/m4-web | tee "$run/history-v3.txt"
grep -Eq '^3[[:space:]]+initial$' "$run/history-v3.txt"
reused_rs=$(k get rs -l app=m4-web -o json | python3 -c \
  'import json,sys; print(next(r["metadata"]["uid"] for r in json.load(sys.stdin)["items"] if r["metadata"]["annotations"]["deployment.kubernetes.io/revision"]=="3"))')
[[ $initial_rs == "$reused_rs" ]]
k patch deployment/m4-web --type=merge -p '{"spec":{"revisionHistoryLimit":0}}'
only_current_revision() {
  local revs
  revs=$(k get rs -l app=m4-web -o jsonpath='{range .items[*]}{.metadata.annotations.deployment\.kubernetes\.io/revision}{"\n"}{end}') &&
    [[ $revs == 3 ]]
}
until_ok 25 only_current_revision
echo "Verified revision history, reuse, and history limit zero"

k annotate deployment/m4-web deployment.kubernetes.io/revision=9223372036854775807 --overwrite
k rollout status deployment/m4-web --revision=3 --watch=false
k patch deployment/m4-web --type=merge -p '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":0,"maxUnavailable":1}}}}'
k rollout pause deployment/m4-web
start_watch paused
old_pod=$(k get pods -l app=m4-web -o jsonpath='{.items[0].metadata.name}')
k delete "pod/$old_pod" --wait=false
verify_watch paused v1 2 0 1
k rollout resume deployment/m4-web
start_watch background
old_set=$(k get rs -l app=m4-web -o name)
k delete "$old_set" --cascade=background --wait=true --timeout=20s
verify_watch background v1 2 0 2
k rollout status deployment/m4-web --revision=4 --watch=false
echo "Verified zero-surge replacements and the revision counter after ReplicaSet deletion"

k create -f - <<'YAML'
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: m4-standalone
spec:
  replicas: 0
  selector:
    matchLabels:
      app: m4-standalone
  template:
    metadata:
      labels:
        app: m4-standalone
    spec:
      terminationGracePeriodSeconds: 1
      containers:
      - name: app
        image: busybox:1.36
        command: [sh, -ec, 'trap "exit 0" TERM; while :; do sleep 1; done']
YAML
k scale rs/m4-standalone --current-replicas=0 --replicas=1 --timeout=35s
k wait pod -l app=m4-standalone --for=condition=Ready --timeout=30s
k scale rs/m4-standalone --replicas=0 --timeout=25s
python3 - "$run/trace.jsonl" <<'PY'
import json, sys
with open(sys.argv[1]) as trace:
    scale = [e for line in trace if (e := json.loads(line))["path"].endswith("/scale") and e["status"] == 200]
for resource in ("deployments/m4-web", "replicasets/m4-standalone"):
    requests = [e for e in scale if e["path"].endswith(f"/{resource}/scale")]
    assert {"GET", "PUT", "PATCH"} <= {e["method"] for e in requests}, requests
    print(f"Verified {resource}/scale GET, PUT and PATCH in the request trace")
PY
k delete rs/m4-standalone --cascade=foreground --wait=true --timeout=30s
k delete deployment/m4-web --cascade=foreground --wait=true --timeout=40s
until_ok 15 empty_runtime
echo "M4 integration passed"
