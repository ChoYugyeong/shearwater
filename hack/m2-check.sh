#!/usr/bin/env bash
# Build sw unprivileged after sourcing hack/env.sh, then run this in WSL:
# wsl.exe -d Ubuntu-24.04 -u root -- bash /path/to/hack/m2-check.sh /absolute/sw /absolute/kubectl
# Test evidence and SQLite stay under /var/lib/shearwater/check.
set -euo pipefail
[[ $(id -u) == 0 && $# == 2 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] || {
  echo "usage (root): bash hack/m2-check.sh /absolute/sw /absolute/kubectl" >&2
  exit 2
}
sw=$1 kubectl=$2
socket=${SW_CONTAINERD_SOCKET:-/run/containerd/containerd.sock}
[[ $socket == /* ]] || { echo 'SW_CONTAINERD_SOCKET must be absolute' >&2; exit 2; }
port=${SW_M2_PORT:-16445}
ctr_sw() { ctr --namespace shearwater --address "$socket" "$@"; }
[[ -z $(ctr_sw containers list -q) ]] || {
  echo "shearwater already has containers; stop the test here" >&2
  exit 1
}
mkdir -p /var/lib/shearwater/check /run/shearwater
run=$(mktemp -d /var/lib/shearwater/check/m2.XXXXXXXX)
net=$(mktemp -d /run/shearwater/m2.XXXXXXXX)
echo "Evidence: $run"
srv=
k() { KUBECONFIG="$run/kubeconfig" "$kubectl" --request-timeout=10s "$@"; }
stop() {
  if [[ -n $srv ]]; then
    kill "$srv" 2>/dev/null || true
    wait "$srv" || true
    srv=
  fi
}
cleanup() {
  if [[ -n $srv ]]; then
    k delete pods m2-a m2-b --ignore-not-found --grace-period=1 --wait=true --timeout=15s || true
  fi
  stop
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
until_ok() {
  local seconds=$1
  shift
  local deadline=$((SECONDS + seconds))
  until "$@"; do
    if (( SECONDS >= deadline )); then
      echo "Timed out: $*" >&2
      tail -40 "$run/server.log" >&2
      return 1
    fi
    sleep 1
  done
}
serving() { kill -0 "$srv" && k get node m2-check >/dev/null 2>&1; }
start() {
  "$sw" server --runtime=containerd --containerd-socket="$socket" --node-name=m2-check --listen="127.0.0.1:$port" \
    --data-dir="$run/data" --config-dir="$run/etc" --run-dir="$net" --log-dir="$run/log" \
    >>"$run/server.log" 2>&1 &
  srv=$!
  until_ok 10 test -s "$run/etc/admin.token"
  "$sw" kubeconfig --config-dir="$run/etc" --server="https://127.0.0.1:$port" >"$run/kubeconfig"
  until_ok 15 serving
}
orphan="$(cat /proc/sys/kernel/random/uuid)-orphan-0"
ctr_sw snapshots prepare "$orphan" >/dev/null
start
k wait --for=condition=Ready node/m2-check --timeout=20s
snapshot_gone() {
  ctr_sw snapshots list | awk -v name="$orphan" '$1 == name { found=1 } END { exit found }'
}
until_ok 10 snapshot_gone
echo "Verified orphan snapshot sweep"
cat <<EOF | k create -f -
apiVersion: v1
kind: Pod
metadata:
  name: m2-a
spec:
  nodeName: m2-check
  terminationGracePeriodSeconds: 8
  volumes:
  - name: host
    hostPath:
      path: $run/host/created
      type: DirectoryOrCreate
  containers:
  - name: main
    image: busybox:1.36
    command: [sh, -ec, 'trap "" TERM; echo m2-start; touch /host/mounted; while :; do sleep 1; done']
    volumeMounts:
    - name: host
      mountPath: /host
---
apiVersion: v1
kind: Pod
metadata:
  name: m2-b
spec:
  nodeName: m2-check
  containers:
  - name: main
    image: busybox:1.36
    command: [httpd, -f, -p, "8080"]
EOF
k wait --for=condition=Ready pod/m2-a pod/m2-b --timeout=90s
k get pods -o wide
pod_value() { k get pod "$1" -o "jsonpath=$2"; }
ip_a=$(pod_value m2-a '{.status.podIP}')
ip_b=$(pod_value m2-b '{.status.podIP}')
id() { pod_value m2-a '{.status.containerStatuses[0].containerID}'; }
before=$(id)
[[ $ip_a == 10.244.0.* && $ip_b == 10.244.0.* && $ip_a != "$ip_b" && $before == containerd://* ]]
k logs m2-a | grep -Fx m2-start
until_ok 10 test -f "$run/host/created/mounted"
ctr_sw tasks exec --exec-id=m2-peer "${before#containerd://}" ping -c 1 -W 3 "$ip_b"
ctr_sw tasks exec --exec-id=m2-egress "${before#containerd://}" ping -c 1 -W 3 1.1.1.1
ctr_sw tasks exec --exec-id=m2-tcp "${before#containerd://}" nc -z -w 5 1.1.1.1 80
ctr_sw tasks kill --signal SIGKILL "${before#containerd://}"
restarted() {
  [[ $(pod_value m2-a '{.status.containerStatuses[0].restartCount}') == "$1" &&
     $(pod_value m2-a '{.status.containerStatuses[0].ready}') == true &&
     $(id) != "$before" ]]
}
until_ok 35 restarted 1
[[ $(pod_value m2-a '{.status.podIP}') == "$ip_a" ]]
k logs m2-a --previous | grep -Fx m2-start
after=$(id)
stop
start
[[ $(id) == "$after" && $(pod_value m2-a '{.status.podIP}') == "$ip_a" ]]
k logs m2-a | grep -Fx m2-start
echo "Verified restart after SIGKILL and adoption after sw server restart"
stop
ctr_sw tasks kill --signal SIGKILL "${after#containerd://}"
before=$after
start
until_ok 40 restarted 2
[[ $(pod_value m2-a '{.status.podIP}') == "$ip_a" ]]
[[ -n $(pod_value m2-a '{.status.containerStatuses[0].lastState.terminated.finishedAt}') ]]
echo "Verified catch-up after the container exited while sw server was down"
terminating=$(id)
k delete pod m2-a --wait=false
[[ -n $(pod_value m2-a '{.metadata.deletionTimestamp}') ]]
sleep 2
ctr_sw tasks list | awk -v id="${terminating#containerd://}" '$1 == id && $3 == "RUNNING" { running=1 } END { exit !running }'
k wait --for=delete pod/m2-a --timeout=20s
k delete pod m2-b --grace-period=1 --wait=true --timeout=20s
[[ -z $(ctr_sw containers list -q) && -z $(ctr_sw tasks list -q) ]]
[[ -z $(find "$net/netns" -mindepth 1 -print -quit) && -f "$run/host/created/mounted" ]]
python3 - "$run/data/state.db" <<'PY'
import sqlite3, sys, time
with sqlite3.connect(sys.argv[1]) as db:
    for _ in range(20):
        if not db.execute("SELECT 1 FROM ip_alloc WHERE kind = 'pod'").fetchone():
            break
        time.sleep(.25)
    else:
        raise SystemExit("Pod IP allocation leaked")
PY
echo "M2 integration passed"
