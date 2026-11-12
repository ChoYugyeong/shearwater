#!/usr/bin/env bash
# A first-user smoke test, in a disposable Linux/WSL runtime environment.
# Usage (root): bash hack/alpha-check.sh /absolute/sw /absolute/kubectl
# Optional: SW_CONTAINERD_SOCKET, SW_ALPHA_PORT, SW_ALPHA_KEEP=1.
# It refuses existing shearwater containers. It never selects another context.
# KEEP=1 leaves a successful fixture available for a separate real reboot test.
set -euo pipefail
umask 077
export LC_ALL=C
die() { printf '%s\n' "$*" >&2; exit 1; }
[[ $# == 2 && $EUID == 0 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] ||
  die 'usage (root): bash hack/alpha-check.sh /absolute/sw /absolute/kubectl'
sw=$1 kubectl=$2
socket=${SW_CONTAINERD_SOCKET:-/run/containerd/containerd.sock}
port=${SW_ALPHA_PORT:-16450}
keep=${SW_ALPHA_KEEP:-0}
[[ $socket == /* && $port =~ ^[0-9]+$ && $port -gt 0 && $port -le 65535 && $keep =~ ^[01]$ ]] || die 'Invalid test options'
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
for tool in curl ctr python3 timeout findmnt realpath ip iptables; do command -v "$tool" >/dev/null || die "Missing: $tool"; done
ctr_sw() { timeout --kill-after=3s 20s ctr --address "$socket" --namespace shearwater "$@"; }
containers=$(ctr_sw containers list -q) || die 'Cannot inspect the shearwater runtime'
tasks=$(ctr_sw tasks list -q) || die 'Cannot inspect shearwater tasks'
[[ -z $containers && -z $tasks ]] || die 'The shearwater runtime must be empty'
# A different containerd socket (or WSL filesystem) is not network isolation.
# Refuse an existing sw network even if our chosen runtime happens to be empty.
for link in sw-br0 sw-svc0; do
  if ip link show dev "$link" >/dev/null 2>&1; then die "Existing Shearwater network: $link; use an isolated environment"; fi
done
if iptables -w 2 -S SHEARWATER-FORWARD >/dev/null 2>&1 ||
   iptables -w 2 -t nat -S SHEARWATER-POSTROUTING >/dev/null 2>&1; then
  die 'Existing Shearwater iptables chains; use an isolated environment'
fi
python3 - "$port" <<'PY'
import socket,sys
with socket.socket() as s:
    s.bind(('127.0.0.1',int(sys.argv[1])))
PY
mkdir -p /var/lib/shearwater/check /run/shearwater
[[ $(realpath /var/lib/shearwater/check) == /var/lib/shearwater/check &&
   $(realpath /run/shearwater) == /run/shearwater &&
   $(findmnt -n -o FSTYPE --target /var/lib/shearwater/check) == ext4 ]] || die 'Fixture roots must be real directories on Linux ext4'
fixture=$(mktemp -d /var/lib/shearwater/check/alpha.XXXXXXXX)
run_dir=$(mktemp -d /run/shearwater/alpha.XXXXXXXX)
flags=(--listen="127.0.0.1:$port" --node-name=alpha-check --data-dir="$fixture/data"
       --config-dir="$fixture/etc" --run-dir="$run_dir" --log-dir="$fixture/log"
       --containerd-socket="$socket")
kubeconfig="$fixture/etc/kubeconfig"
k() { timeout --kill-after=3s 140s "$kubectl" --kubeconfig="$kubeconfig" --cache-dir="$fixture/discovery" -n default "$@"; }
sw_cmd() { timeout --kill-after=5s 90s "$sw" "$@"; }
owned=0
cleanup() {
  local rc=$?
  local cleanup_state=not-started
  if (( owned )) && { (( rc != 0 )) || [[ $keep == 0 ]]; }; then
    if sw_cmd down --all "${flags[@]}" >"$fixture/cleanup.txt" 2>&1; then
      cleanup_state=stopped
    else
      cleanup_state=failed
      rc=1
    fi
  elif (( owned )); then
    cleanup_state=kept
  fi
  # Passing HTTP/restart checks alone is not overall success: default cleanup
  # must finish too. Publish the final result only after that decision.
  python3 - "$fixture" "$rc" "$cleanup_state" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]); source=p/'functional.json'
result=json.loads(source.read_text()) if source.exists() else {'checksPassed':False}
result.update(passed=bool(result.get('checksPassed')) and int(sys.argv[2])==0,
              exitCode=int(sys.argv[2]), cleanup=sys.argv[3])
with (p/'result.json').open('x') as f:
    json.dump(result,f,indent=2)
print(json.dumps(result,indent=2))
PY
  if (( rc == 0 )); then printf 'PASS alpha check (KEEP=%s)\n' "$keep"; fi
  printf 'Evidence: %s\n' "$fixture"
  if (( rc != 0 )); then echo 'FAIL (evidence preserved)' >&2; fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
retry() {
  local deadline=$((SECONDS + $1)); shift
  until "$@"; do (( SECONDS < deadline )) || return 1; sleep 0.5; done
}
snapshot() {
  k get pods -l app=web -o json >"$fixture/$1-pods.json"
  k get service web -o json >"$fixture/$1-service.json"
}
owned=1
sw_cmd up "${flags[@]}" >"$fixture/up.txt" 2>&1
[[ $(k config view --minify -o jsonpath='{.clusters[0].cluster.server}') == "https://127.0.0.1:$port" ]]
k wait --for=condition=Ready node/alpha-check --timeout=30s >"$fixture/node.txt"
k apply -f "$repo/examples/web.yaml" >"$fixture/apply.txt"
k rollout status deploy/web --timeout=120s >"$fixture/rollout.txt"
snapshot initial
ip=$(k get service web -o jsonpath='{.spec.clusterIP}')
http() { curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 5 "http://$ip/" >"$fixture/response.html" 2>"$fixture/http.err"; }
retry 30 http
grep -q 'Welcome to nginx!' "$fixture/response.html"
k logs deploy/web --tail=100 >"$fixture/logs.txt"
grep -q 'GET / HTTP' "$fixture/logs.txt"
k get endpointslices -l kubernetes.io/service-name=web -o json >"$fixture/endpointslices.json"
# Read-only commands need only the API address and credentials, not runtime flags.
sw_cmd status --listen="127.0.0.1:$port" --config-dir="$fixture/etc" >"$fixture/status.txt"
sw_cmd tree --listen="127.0.0.1:$port" --config-dir="$fixture/etc" >"$fixture/tree.txt"
sw_cmd doctor "${flags[@]}" >"$fixture/doctor.txt"

# A server restart should adopt the existing task, not count as an app restart.
sw_cmd down "${flags[@]}" >"$fixture/down.txt"
sw_cmd up "${flags[@]}" >"$fixture/up-again.txt"
retry 30 http
snapshot server-restart

# Killing only the selected container tests the node agent's restart policy.
task=$(k get pods -l app=web -o jsonpath='{.items[0].status.containerStatuses[0].containerID}')
[[ $task == containerd://* && ${task#containerd://} != '' ]] || die 'Missing test task ID'
ctr_sw tasks kill --signal SIGKILL "${task#containerd://}"
restarted() {
  snapshot container-restart
  python3 - "$fixture" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
a=json.loads((p/'initial-pods.json').read_text())['items'][0]['status']['containerStatuses'][0]
b=json.loads((p/'container-restart-pods.json').read_text())['items'][0]['status']['containerStatuses'][0]
sys.exit(0 if b['restartCount'] > a['restartCount'] and b.get('ready') and 'running' in b.get('state',{}) else 1)
PY
}
retry 60 restarted
retry 30 http
k logs deploy/web --previous >"$fixture/previous-logs.txt"
python3 - "$fixture" "$run_dir" "$port" "$socket" <<'PY' >"$fixture/functional.json"
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
read=lambda n:json.loads((p/n).read_text())
a=read('initial-pods.json')['items']
assert len(a)==1
a=a[0]; sa=a['status']['containerStatuses'][0]
service=read('initial-service.json')
for label in ('server-restart','container-restart'):
    b=read(label+'-pods.json')['items']; assert len(b)==1
    b=b[0]; sb=b['status']['containerStatuses'][0]
    assert b['metadata']['uid']==a['metadata']['uid'] and b['status']['podIP']==a['status']['podIP']
    assert read(label+'-service.json')['spec']['clusterIP']==service['spec']['clusterIP']
    if label=='server-restart':
        assert sb['containerID']==sa['containerID'] and sb['restartCount']==sa['restartCount']
    else:
        assert sb['containerID']!=sa['containerID'] and sb['restartCount']>sa['restartCount']
endpoints=[e for s in read('endpointslices.json')['items'] for e in s.get('endpoints',[])]
assert any(e.get('targetRef',{}).get('uid')==a['metadata']['uid'] and a['status']['podIP'] in e['addresses'] for e in endpoints)
print(json.dumps(dict(checksPassed=True,fixture=str(p),runDir=sys.argv[2],port=int(sys.argv[3]),containerdSocket=sys.argv[4],
    podUID=a['metadata']['uid'],podIP=a['status']['podIP'],clusterIP=service['spec']['clusterIP'],
    checks=['deploy','HTTP','logs','EndpointSlice','status','tree','doctor','server restart adoption','container restart','previous logs']),indent=2))
PY
