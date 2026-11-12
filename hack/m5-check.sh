#!/usr/bin/env bash
# Run as WSL root with sw built unprivileged after sourcing hack/env.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
[[ $(id -u) == 0 && $# == 2 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] || {
  echo "usage (root): bash hack/m5-check.sh /absolute/sw /absolute/kubectl" >&2
  exit 2
}
sw=$1 kubectl=$2
port=${SW_M5_PORT:-16448}
ctr_sw() { ctr --namespace shearwater "$@"; }
empty_runtime() {
  local ids
  ids=$(ctr_sw containers list -q) && [[ -z $ids ]]
}
empty_runtime || { echo "shearwater runtime is busy or unavailable" >&2; exit 1; }
mkdir -p /var/lib/shearwater/check /run/shearwater
run=$(mktemp -d /var/lib/shearwater/check/m5.XXXXXXXX)
net=$(mktemp -d /run/shearwater/m5.XXXXXXXX)
echo "Evidence: $run"
srv=
k() { KUBECONFIG="$run/kubeconfig" "$kubectl" --request-timeout=15s "$@"; }
http() { curl --noproxy '*' --connect-timeout 1 --max-time 3 -fsS "$1"; }
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
serving() { kill -0 "$srv" && k get node m5-check >/dev/null 2>&1; }
start() {
  "$sw" server --runtime=containerd --node-name=m5-check --listen="127.0.0.1:$port" \
    --data-dir="$run/data" --config-dir="$run/etc" --run-dir="$net" --log-dir="$run/log" \
    --trace="$run/trace.jsonl" >>"$run/server.log" 2>&1 &
  srv=$!
  until_ok 10 test -s "$run/etc/admin.token"
  "$sw" kubeconfig --config-dir="$run/etc" --server="https://127.0.0.1:$port" >"$run/kubeconfig"
  until_ok 15 serving
}
stop() {
  if [[ -n $srv ]]; then
    kill "$srv" 2>/dev/null || true
    wait "$srv" || true
    srv=
  fi
}
cleanup() {
  local status=$?
  if [[ -n $srv && -s $run/kubeconfig ]] && kill -0 "$srv" 2>/dev/null; then
    k patch pod m5-green --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    k delete deployment m5-expose --ignore-not-found --wait=false >/dev/null 2>&1 || true
    k delete service m5-web m5-fixed m5-headless m5-no-selector m5-reused m5-expose m5-conflict \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
    k delete pod m5-blue m5-green --ignore-not-found --grace-period=1 --wait=false >/dev/null 2>&1 || true
    k delete pod -l app=m5-expose --ignore-not-found --grace-period=1 --wait=false >/dev/null 2>&1 || true
    until_ok 35 empty_runtime || true
  fi
  stop
  if (( status != 0 )); then tail -40 "$run/server.log" >&2; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
start
k wait --for=condition=Ready node/m5-check --timeout=25s

k create -f - <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: m5-web
spec:
  selector: {app: m5-web}
  ports:
  - {name: http, port: 80, targetPort: web}
---
apiVersion: v1
kind: Service
metadata:
  name: m5-fixed
spec:
  clusterIP: 10.96.0.40
  selector: {app: m5-web, tier: blue}
  ports:
  - {port: 81, targetPort: 8080}
---
apiVersion: v1
kind: Service
metadata:
  name: m5-headless
spec:
  clusterIP: None
  selector: {app: m5-web}
  ports:
  - {port: 80, targetPort: web}
---
apiVersion: v1
kind: Service
metadata:
  name: m5-no-selector
spec:
  ports:
  - {port: 80}
YAML
ip_of() { k get service "$1" -o jsonpath='{.spec.clusterIP}'; }
web_ip=$(ip_of m5-web)
fixed_ip=$(ip_of m5-fixed)
empty_ip=$(ip_of m5-no-selector)
[[ $web_ip == 10.96.* && $fixed_ip == 10.96.0.40 && $empty_ip == 10.96.* &&
   $web_ip != "$fixed_ip" && $web_ip != "$empty_ip" && $(ip_of m5-headless) == None ]]
if k create -f - >"$run/conflict.txt" 2>&1 <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: m5-conflict
spec:
  clusterIP: 10.96.0.40
  ports: [{port: 80}]
YAML
then
  echo "Duplicate ClusterIP was accepted" >&2
  exit 1
fi
grep -Eqi 'clusterIP|already.*allocated' "$run/conflict.txt"

# Both Pods match m5-web, but their port called "web" has different numbers.
# A finalizer lets us observe green after DELETE while checking its removal
# from routing. Green also overrides one automatically supplied env value.
for item in blue:8080 green:8081; do
  color=${item%:*}
  target=${item#*:}
  extra=
  override=
  if [[ $color == green ]]; then
    extra='  finalizers: [shearwater.io/m5-check]'
    override='    env: [{name: M5_WEB_SERVICE_PORT, value: "9999"}]'
  fi
  k create -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: m5-$color
  labels: {app: m5-web, tier: $color}
$extra
spec:
  terminationGracePeriodSeconds: 2
  containers:
  - name: app
    image: busybox:1.36
    ports: [{name: web, containerPort: $target}]
$override
    command: [sh, -ec, 'mkdir -p /www; echo $color > /www/index.html; echo "service=\${M5_WEB_SERVICE_HOST:-missing}:\${M5_WEB_SERVICE_PORT:-missing}"; exec httpd -f -p $target -h /www']
YAML
done
k wait --for=condition=Ready pod/m5-blue pod/m5-green --timeout=60s
green_ip=$(k get pod m5-green -o jsonpath='{.status.podIP}')
logs_contain() { k logs "m5-$1" | grep -Fx "service=$web_ip:$2"; }
until_ok 10 logs_contain blue 80
until_ok 10 logs_contain green 9999

slice_ports() {
  k get endpointslices -l "kubernetes.io/service-name=$1" \
    -o jsonpath='{range .items[*]}{.ports[0].port}{"\n"}{end}' | sort -n
}
has_ports() {
  local ports
  ports=$(slice_ports "$1") && [[ $ports == "$2" ]]
}
no_slices() {
  local names
  names=$(k get endpointslices -l "kubernetes.io/service-name=$1" -o name) && [[ -z $names ]]
}
until_ok 15 has_ports m5-web $'8080\n8081'
k get endpointslices -l kubernetes.io/service-name=m5-web -o json >"$run/endpointslices.json"
no_slices m5-headless
no_slices m5-no-selector
reply_is() {
  local body
  body=$(http "$1" 2>/dev/null) && [[ $body == "$2" ]]
}
until_ok 15 reply_is "http://$fixed_ip:81/" blue
web_url="http://$web_ip/"
reachable() { http "$1" >/dev/null 2>&1; }
until_ok 15 reachable "$web_url"
first=$(http "$web_url")
second=$(http "$web_url")
[[ ( $first == blue && $second == green ) || ( $first == green && $second == blue ) ]]
[[ $(http "$web_url") == "$first" && $(http "$web_url") == "$second" ]]
if reachable "http://$empty_ip/"; then
  echo "Selectorless Service unexpectedly returned a response" >&2
  exit 1
fi
echo "Verified automatic and explicit IPs, two named-port slices, round robin, and env"

container=$(k get pod m5-blue -o jsonpath='{.status.containerStatuses[0].containerID}')
[[ $(ctr_sw tasks exec --exec-id m5-hairpin "${container#containerd://}" wget -qO- -T 5 "http://$fixed_ip:81/") == blue ]]
echo "Verified Pod -> its own Service -> the same Pod"

k patch service m5-web --type=merge -p '{"spec":{"selector":{"tier":"blue"}}}'
until_ok 15 has_ports m5-web 8080
until_ok 15 reply_is "$web_url" blue
k patch service m5-web --type=merge -p '{"spec":{"selector":{"tier":null}}}'
until_ok 15 has_ports m5-web $'8080\n8081'
k delete pod m5-green --wait=false
until_ok 15 has_ports m5-web 8080
[[ -n $(k get pod m5-green -o jsonpath='{.metadata.deletionTimestamp}') ]]
for _ in {1..3}; do until_ok 10 reply_is "$web_url" blue; done
k patch pod m5-green --type=merge -p '{"metadata":{"finalizers":[]}}'
k wait --for=delete pod/m5-green --timeout=25s
pod_ip_released() {
  python3 -c 'import sqlite3,sys; db=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro",uri=True); sys.exit(db.execute("SELECT 1 FROM ip_alloc WHERE ip=? AND kind=?", (sys.argv[2],"pod")).fetchone() is not None)' "$run/data/state.db" "$green_ip"
}
until_ok 15 pod_ip_released
echo "Verified selector updates and withdrawal of a terminating Pod"

has_addr() {
  ip -o -4 addr show dev sw-svc0 | awk -v addr="$1/32" '$4==addr {found=1} END {exit !found}'
}
has_addr "$web_ip"
stop
has_addr "$web_ip"
if reachable "$web_url"; then
  echo "TCP listener survived server stop" >&2
  exit 1
fi
start
[[ $(ip_of m5-web) == "$web_ip" ]]
until_ok 15 reply_is "$web_url" blue
echo "Verified stable ClusterIP and listener recovery after server restart"

k delete service m5-web --wait=true --timeout=20s
absent_addr() { ! has_addr "$1"; }
until_ok 15 absent_addr "$web_ip"
until_ok 15 no_slices m5-web
if reachable "$web_url"; then
  echo "Deleted Service still responds" >&2
  exit 1
fi
k create -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: m5-reused
spec:
  clusterIP: $web_ip
  selector: {app: m5-web, tier: blue}
  ports: [{port: 80, targetPort: 8080}]
YAML
until_ok 15 reply_is "$web_url" blue
echo "Verified deletion detaches the IP and a new Service can reuse it"

k create deployment m5-expose --image=busybox:1.36 --port=8080 -- \
  sh -ec 'mkdir -p /www; echo exposed > /www/index.html; exec httpd -f -p 8080 -h /www'
k rollout status deployment/m5-expose --timeout=60s
k expose deployment m5-expose --port=82 --target-port=8080
expose_ip=$(ip_of m5-expose)
direct=$(k get pods -l app=m5-expose -o jsonpath='{.items[0].status.podIP}')
[[ $direct == "$green_ip" ]]
if ! until_ok 15 reply_is "http://$expose_ip:82/" exposed; then
  # Keep enough evidence to distinguish an application, EndpointSlice,
  # listener or Pod network failure before the EXIT trap removes fixtures.
  k get pods -l app=m5-expose -o wide >&2
  k get endpointslices -l kubernetes.io/service-name=m5-expose -o yaml >&2
  ip -o -4 addr show dev sw-svc0 >&2
  ip neigh show dev sw-br0 >&2
  http "http://$direct:8080/" >&2 || true
  http "http://$expose_ip:82/" >&2 || true
  exit 1
fi
echo "Verified kubectl expose Deployment -> curl ClusterIP with recycled Pod IP $direct"

k delete service m5-fixed m5-headless m5-no-selector m5-reused m5-expose --wait=true --timeout=20s
until_ok 15 absent_addr "$web_ip"
until_ok 15 absent_addr "$fixed_ip"
until_ok 15 absent_addr "$expose_ip"
k delete deployment m5-expose --cascade=foreground --wait=true --timeout=50s
k delete pod m5-blue --grace-period=1 --wait=true --timeout=20s
until_ok 15 empty_runtime
echo "M5 integration passed"
