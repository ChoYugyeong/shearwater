#!/usr/bin/env bash
# Run in WSL as root with an unprivileged build of sw and the Linux kubectl.
set -euo pipefail
[[ $(id -u) == 0 && $# == 2 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] || {
  echo "usage (root): bash hack/m3-check.sh /absolute/sw /absolute/kubectl" >&2
  exit 2
}
sw=$1 kubectl=$2
port=${SW_M3_PORT:-16446}
[[ -z $(ctr --namespace shearwater containers list -q) ]] || {
  echo "shearwater already has containers; stop the test here" >&2
  exit 1
}
mkdir -p /var/lib/shearwater/check /run/shearwater
run=$(mktemp -d /var/lib/shearwater/check/m3.XXXXXXXX)
net=$(mktemp -d /run/shearwater/m3.XXXXXXXX)
echo "Evidence: $run"
k() { KUBECONFIG="$run/kubeconfig" "$kubectl" --request-timeout=90s "$@"; }
"$sw" server --runtime=containerd --node-name=m3-check --listen="127.0.0.1:$port" \
  --data-dir="$run/data" --config-dir="$run/etc" --run-dir="$net" --log-dir="$run/log" \
  >"$run/server.log" 2>&1 &
srv=$!
cleanup() {
  local status=$?
  if [[ -s $run/kubeconfig ]] && kill -0 "$srv" 2>/dev/null; then
    k delete deployment,replicaset,pod --all --grace-period=1 --wait=false >/dev/null 2>&1 || true
    for _ in {1..10}; do
      if [[ -z $(ctr --namespace shearwater containers list -q) ]]; then
        break
      fi
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
node_exists() { k get node m3-check >/dev/null 2>&1; }
until_ok 15 node_exists
k wait --for=condition=Ready node/m3-check --timeout=25s

deployment() {
  k create deployment "$1" --image=busybox:1.36 -- sh -c 'trap "exit 0" TERM; while :; do sleep 1; done'
  k wait --for=condition=Available "deployment/$1" --timeout=60s
}
deployment web
k get deployment,rs,pods -o wide
pod=$(k get pods -l app=web -o jsonpath='{.items[0].metadata.name}')
before=$(k get "pod/$pod" -o jsonpath='{.metadata.uid}')
[[ $(k get "pod/$pod" -o jsonpath='{.spec.nodeName}') == m3-check ]]
[[ $(k get "pod/$pod" -o jsonpath='{.status.podIP}') == 10.244.0.* ]]
k delete "pod/$pod" --grace-period=1 --wait=true --timeout=25s
replaced() {
  local u
  u=$(k get pods -l app=web --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null) &&
    [[ -n $u && $u != "$before" ]]
}
until_ok 45 replaced
echo "Verified scheduling and Pod replacement"

gone() {
  local remaining
  remaining=$(k get deployment,rs,pods -l "app=$1" -o name) && [[ -z $remaining ]]
}
k delete deployment web --cascade=background --wait=true --timeout=15s
until_ok 45 gone web
echo "Verified background cascade"

deployment foreground
k delete deployment foreground --cascade=foreground --wait=true --timeout=55s
gone foreground
echo "Verified foreground cascade"

deployment orphan
orphan_rs=$(k get rs -l app=orphan -o name)
k delete deployment orphan --cascade=orphan --wait=true --timeout=15s
[[ -z $(k get "$orphan_rs" -o jsonpath='{.metadata.ownerReferences}') ]]
[[ $(k get pods -l app=orphan -o jsonpath='{.items[0].status.phase}') == Running ]]
k delete "$orphan_rs" --cascade=foreground --wait=true --timeout=55s
gone orphan
[[ -z $(ctr --namespace shearwater containers list -q) ]]
echo "Verified orphan retention and explicit cleanup"
echo "M3 integration passed"
