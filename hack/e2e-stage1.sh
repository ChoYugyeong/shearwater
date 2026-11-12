#!/usr/bin/env bash
# Run in WSL as root: bash hack/e2e-stage1.sh /absolute/sw /absolute/kubectl
# Keep the SQLite database, manifests, command output and logs in the fixture.
# Run M0/M1 and Go tests separately as a regular user. Measurement is separate.
set -euo pipefail
umask 077
export LC_ALL=C

die() { printf '%s\n' "$*" >&2; exit 1; }
[[ $# == 2 && $EUID == 0 && $1 == /* && $2 == /* && -x $1 && -x $2 ]] ||
  die "usage (WSL root): bash hack/e2e-stage1.sh /absolute/sw /absolute/kubectl"
sw=$1
kubectl=$2
socket=${SW_CONTAINERD_SOCKET:-/run/containerd/containerd.sock}
[[ $socket == /* ]] || die "SW_CONTAINERD_SOCKET must be absolute"
checks="$(cd -- "$(dirname -- "$0")" && pwd)/testdata/stage1/assertions.py"
for tool in curl ctr findmnt ip iptables python3 timeout; do
  command -v "$tool" >/dev/null || die "Missing command: $tool"
done

check_dir=/var/lib/shearwater/check
run_root=/run/shearwater
mkdir -p -- "$check_dir" "$run_root"
[[ $(realpath -e -- "$check_dir") == "$check_dir" && $(realpath -e -- "$run_root") == "$run_root" ]] ||
  die "Fixture roots must resolve to $check_dir and $run_root"
[[ $(findmnt -n -o FSTYPE --target "$check_dir") == ext4 ]] ||
  die "Keep the SQLite fixture on WSL ext4 at $check_dir"
fixture=$(mktemp -d "$check_dir/stage1.XXXXXXXX")
run_dir=$(mktemp -d "$run_root/stage1.XXXXXXXX")
config_dir="$fixture/etc"
log_dir="$fixture/log"
db="$fixture/data/state.db"
kubeconfig="$config_dir/kubeconfig"
listen=127.0.0.1:16449
api="https://$listen"
node=stage1-check
flags=(--listen="$listen" --node-name="$node" --data-dir="$fixture/data" --containerd-socket="$socket"
       --config-dir="$config_dir" --run-dir="$run_dir" --log-dir="$log_dir")
kube=("$kubectl" --kubeconfig="$kubeconfig" --cache-dir="$fixture/discovery" --namespace=default)

sw_cmd() { timeout --kill-after=5s 90s "$sw" "$@"; }
k() { timeout --kill-after=5s 135s "${kube[@]}" "$@"; }
verify() { python3 -I "$checks" "$@"; }
# Every runtime observation and signal goes to this namespace on the socket used by sw up.
ctr_sw() { timeout --kill-after=3s 15s ctr --namespace shearwater --address "$socket" "$@"; }
note() { printf '%s\n' "$*" | tee -a "$fixture/results.txt"; }
step() { section=$1; prefix="$fixture/$section"; note "$section $2"; }
pass() { note "PASS $section"; }
retry() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    (( SECONDS < deadline )) || { printf 'Timed out waiting for %s\n' "$1" >&2; return 1; }
    sleep 0.5
  done
}

section=preflight
owned=0
starting=0
bad_owned=0
bad_run=
job_pid=
stop_job() {
  if [[ -n $job_pid ]]; then
    kill -TERM "$job_pid" 2>/dev/null || true
    kill -CONT "$job_pid" 2>/dev/null || true
    wait "$job_pid" 2>/dev/null || true
    job_pid=
  fi
}
cleanup() {
  local rc=$?
  stop_job
  # Use the same flags as up so sw can check which instance owns the runtime.
  if (( owned || starting )); then
    sw_cmd down --all "${flags[@]}" >"$fixture/cleanup.txt" 2>&1 ||
      { note "Cleanup failed. See $fixture/cleanup.txt"; rc=1; }
  fi
  if (( bad_owned )) || [[ -n $bad_run && -s $bad_run/server.json ]]; then
    sw_cmd down --all "${bad_flags[@]}" >"$fixture/cleanup-missing-socket.txt" 2>&1 ||
      { note "Missing-socket fixture cleanup failed. See $fixture/cleanup-missing-socket.txt"; rc=1; }
  fi
  if (( rc != 0 )); then note "FAILED at $section. Evidence kept at $fixture"; fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

runtime_empty() {
  ctr_sw containers list -q >"$1.containers" &&
    ctr_sw tasks list -q >"$1.tasks" &&
    [[ -z $(<"$1.containers") && -z $(<"$1.tasks") ]]
}
api_down() {
  local rc=0
  curl --noproxy '*' --connect-timeout 1 --max-time 3 -ksS "$1/readyz" \
    >"$2" 2>&1 || rc=$?
  [[ $rc == 7 ]]  # curl could not connect to this loopback listener.
}
api_ready() { [[ $(k --request-timeout=4s get --raw /readyz) == ok ]]; }
start() {
  starting=1
  sw_cmd up "${flags[@]}" >"$fixture/up-$1.txt" 2>&1
  owned=1
  starting=0
  [[ -s $kubeconfig && -s $db && -s $run_dir/server.json ]]
  # Check the destination before allowing kubectl to make changes.
  [[ $(k config view --minify -o jsonpath='{.clusters[0].cluster.server}') == "$api" ]]
  retry 15 api_ready
  k wait --for=condition=Ready "node/$node" --timeout=30s >"$fixture/node-$1.txt" 2>&1
}

note "Evidence: $fixture (runtime directory $run_dir)"
runtime_empty "$fixture/runtime-before" || die "The shearwater runtime is busy or unavailable"
api_down "$api" "$fixture/listener-before.txt" || die "The test listener $listen is already in use"

# Test 18 starts with a missing socket on its own port and in its own directories.
# Do this before the positive startup so the shared node has no running test owner.
section=18-preflight
bad_fixture=$(mktemp -d "$check_dir/stage1.XXXXXXXX")
bad_run=$(mktemp -d "$run_root/stage1.XXXXXXXX")
bad_port=$(python3 -I - <<'PY'
import socket
with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
)
[[ $bad_port != 16449 ]]
bad_listen="127.0.0.1:$bad_port"
bad_socket="$bad_run/missing-containerd.sock"
bad_flags=(--listen="$bad_listen" --node-name="$node" --data-dir="$bad_fixture/data"
           --config-dir="$bad_fixture/etc" --run-dir="$bad_run" --log-dir="$bad_fixture/log"
           --containerd-socket="$bad_socket")
printf '%s\n' "$bad_fixture" "$bad_run" "$bad_listen" >"$fixture/18-missing-paths.txt"
if sw_cmd up "${bad_flags[@]}" >"$fixture/18-missing-socket.txt" 2>&1; then
  bad_owned=1
  die "sw up succeeded with a missing containerd socket"
fi
grep -Fq -- "$bad_socket" "$fixture/18-missing-socket.txt"
grep -Eiq 'hint:.*(start.*containerd|--containerd-socket)' "$fixture/18-missing-socket.txt"
api_down "https://$bad_listen" "$fixture/18-missing-listener.txt"
[[ ! -e $bad_run/server.json && ! -e $bad_fixture/etc/kubeconfig && ! -e $bad_fixture/data/state.db ]]

section=18-startup
start initial
# An ordinary repeated up should reuse the process and the existing config.
sha256sum "$run_dir/server.json" "$kubeconfig" >"$fixture/18-start.sha256"
sw_cmd up "${flags[@]}" >"$fixture/18-idempotent-up.txt" 2>&1
grep -Fq 'already running' "$fixture/18-idempotent-up.txt"
sha256sum -c "$fixture/18-start.sha256" >"$fixture/18-start-check.txt"

rollout() { k rollout status deploy/web --timeout=120s >"$prefix-rollout.txt" 2>&1; }
web_ready() {
  k --request-timeout=5s get deploy,rs,pods -o json >"$prefix-workload.json" &&
    verify workload "$prefix-workload.json" "$1" "$main" "$2" >"$prefix-rs-uid.txt" 2>"$prefix-workload.err"
}
pod_value() { k --request-timeout=5s get "pod/$1" -o "jsonpath=$2"; }
web_pod() { k get pods -l app=web -o jsonpath='{.items[0].metadata.name}'; }
container_id() {
  pod_value "$1" '{.status.containerStatuses[?(@.name=="'"$2"'")].containerID}'
}
nginx_ok() {
  local code
  code=$(curl --noproxy '*' --connect-timeout 1 --max-time 4 -fsS -w '%{http_code}' \
    -o "$2" "$1") && [[ $code == 200 ]] && grep -Fq 'Welcome to nginx!' "$2"
}
restarted() {
  k --request-timeout=5s get "pod/$1" -o json >"$prefix-after.json" &&
    verify restarted "$prefix-before.json" "$prefix-after.json" "${@:2}" 2>"$prefix-restart.err"
}

step 01 "Discover the live Kubernetes API"
k version >"$prefix-version.txt" 2>&1
grep -Fq 'Server Version:' "$prefix-version.txt"
k cluster-info >"$prefix-cluster-info.txt" 2>&1
grep -Fq -- "$api" "$prefix-cluster-info.txt"
k api-resources >"$prefix-api-resources.txt" 2>&1
for resource in pods deployments services endpointslices events; do
  grep -Eq "^$resource[[:space:]]" "$prefix-api-resources.txt"
done
pass

step 02 "Report exactly one Ready node"
k get nodes >"$prefix-nodes.txt"
k get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
  >"$prefix-ready.txt"
[[ $(<"$prefix-ready.txt") == "$node True" ]]
pass

step 03 "Create and roll out two nginx replicas"
k create deployment web --image=nginx:1.27 --replicas=2 >"$prefix-create.txt"
rollout
main=$(k get deploy web -o jsonpath='{.spec.template.spec.containers[0].name}')
[[ -n $main ]]
pass

step 04 "Check the wide table and the two Running Pod IPs"
k get deploy,rs,pods -o wide >"$prefix-wide.txt"
awk '$1 == "deployment.apps/web" && $2 == "2/2" {found=1} END {exit !found}' "$prefix-wide.txt"
retry 25 web_ready 2 nginx:1.27
pass

step 05 "Fetch the nginx page directly by Pod IP"
pod=$(web_pod)
pod_ip=$(pod_value "$pod" '{.status.podIP}')
[[ -n $pod_ip ]]
retry 15 nginx_ok "http://$pod_ip/" "$prefix-nginx.html"
pass

step 06 "Kill nginx and observe one restart in the same Pod"
k get "pod/$pod" -o json >"$prefix-before.json"
cid=$(container_id "$pod" "$main")
[[ $cid == containerd://?* ]]
ctr_sw tasks kill --signal SIGKILL "${cid#containerd://}"
retry 75 restarted "$pod" "$main"
retry 15 nginx_ok "http://$pod_ip/" "$prefix-nginx.html"
pass

step 07 "Delete one Pod and verify a new UID replaces it"
old_uid=$(pod_value "$pod" '{.metadata.uid}')
k get pods -o json >"$prefix-before.json"
k delete "pod/$pod" --grace-period=1 --wait=true --timeout=40s >"$prefix-delete.txt"
retry 90 web_ready 2 nginx:1.27
verify replacement "$prefix-before.json" "$prefix-workload.json" "$old_uid"
pass

step 08 "Scale the Deployment to four Ready Pods"
k scale deploy/web --replicas=4 >"$prefix-scale.txt"
rollout
retry 25 web_ready 4 nginx:1.27
pass

step 09 "Apply an image change and check the new ReplicaSet and two revisions"
old_rs=$(<"$fixture/08-rs-uid.txt")
k get deploy web -o json >"$prefix-before.json"
verify manifests "$prefix-before.json" "$fixture"
k apply -f "$fixture/web-original.json" >"$prefix-apply-original.txt" 2>&1
k apply -f "$fixture/web-image.json" >"$prefix-apply-image.txt" 2>&1
rollout
retry 50 web_ready 4 nginx:1.28
[[ $(<"$prefix-rs-uid.txt") != "$old_rs" ]]
k rollout history deploy/web >"$prefix-history.txt"
[[ $(awk '$1 ~ /^[0-9]+$/ {print $1}' "$prefix-history.txt" | sort -n) == $'1\n2' ]]
pass

step 10 "Use strategic apply to add and drop env entries and reorder containers"
k apply -f "$fixture/web-env.json" >"$prefix-apply-env.txt" 2>&1
rollout
retry 50 web_ready 4 nginx:1.28
verify strategic "$fixture/web-env.json" "$prefix-workload.json"
cp -- "$prefix-workload.json" "$prefix-before.json"
# Keep the patch Content-Type as evidence. Omit authorization lines from the debug output.
k --v=8 apply -f "$fixture/web-reordered.json" 2>&1 |
  awk 'tolower($0) !~ /authorization/' >"$prefix-apply-reordered.txt"
grep -Fiq 'application/strategic-merge-patch+json' "$prefix-apply-reordered.txt"
rollout
retry 50 web_ready 4 nginx:1.28
verify strategic "$fixture/web-reordered.json" "$prefix-workload.json"
pass

step 11 "Read an nginx access log, observe a live watch, and describe real Pod Events"
pod=$(web_pod)
pod_ip=$(pod_value "$pod" '{.status.podIP}')
uid=$(pod_value "$pod" '{.metadata.uid}')
retry 15 nginx_ok "http://$pod_ip/?stage1-access-log" "$prefix-nginx.html"
has_access_log() {
  k --request-timeout=5s logs "$pod" -c "$main" >"$prefix-logs.txt" &&
    grep -Fq 'GET /?stage1-access-log ' "$prefix-logs.txt"
}
retry 15 has_access_log
"${kube[@]}" --request-timeout=45s get pods -w -L stage1-watch >"$prefix-watch.txt" 2>"$prefix-watch.err" &
job_pid=$!
watch_row() {
  awk -v pod="$pod" -v label="$1" '$1 == pod && (label == "" || $NF == label) {found=1} END {exit !found}' "$prefix-watch.txt"
}
retry 15 watch_row ""
k label "pod/$pod" stage1-watch=observed >"$prefix-label.txt"
retry 15 watch_row observed
stop_job
pod_events() {
  k --request-timeout=5s describe "pod/$pod" >"$prefix-describe.txt" &&
    k --request-timeout=5s get events \
      --field-selector="involvedObject.kind=Pod,involvedObject.namespace=default,involvedObject.name=$pod,involvedObject.uid=$uid" \
      -o json >"$prefix-events.json" &&
    verify events "$prefix-events.json" "$prefix-describe.txt" "$pod" "$uid" 2>"$prefix-events.err"
}
event_failed=0
if retry 12 pod_events; then
  pass
else
  # The remaining cases can still provide evidence if Event emission regresses.
  note "FAIL 11 Pod Events missing or absent from describe. See $prefix-events.err"
  event_failed=1
fi

step 12 "Expose nginx, match ready EndpointSlice IPs and curl the ClusterIP"
k expose deploy web --port=80 >"$prefix-expose.txt"
cluster_ip=$(k get svc web -o jsonpath='{.spec.clusterIP}')
[[ -n $cluster_ip && $cluster_ip != None ]]
k get pods -l app=web -o json >"$prefix-pods.json"
endpoints_ready() {
  k --request-timeout=5s get endpointslices -l kubernetes.io/service-name=web -o json >"$prefix-slices.json" &&
    verify endpoints "$prefix-pods.json" "$prefix-slices.json" 2>"$prefix-slices.err"
}
retry 25 endpoints_ready
retry 25 nginx_ok "http://$cluster_ip/" "$prefix-nginx.html"
pass

step 13 "SIGKILL sw server and verify SQLite recovery and live task adoption"
running_tasks() {
  ctr_sw tasks list >"$1" &&
    awk '$3 == "RUNNING" {print $1, $2}' "$1" | sort >"$1.running"
}
identity() {
  k --request-timeout=5s get deploy,rs,pods,svc -o json >"$1.objects.json" &&
    running_tasks "$1.tasks" &&
    verify identity "$1.objects.json" "$1.tasks.running" >"$1.identity.json" 2>"$1.identity.err"
}
identity "$prefix-before"
verify kill-server "$run_dir/server.json" "$sw" "$run_dir" >"$prefix-kill.txt"
retry 10 api_down "$api" "$prefix-listener-down.txt"
running_tasks "$prefix-offline.tasks"
cmp "$prefix-before.tasks.running" "$prefix-offline.tasks.running"
retry 10 nginx_ok "http://$pod_ip/" "$prefix-direct-offline.html"
start after-sigkill
retry 25 nginx_ok "http://$cluster_ip/" "$prefix-service-restored.html"
retry 25 identity "$prefix-after"
cmp "$prefix-before.identity.json" "$prefix-after.identity.json"
sw_cmd status --listen="$listen" --config-dir="$config_dir" >"$prefix-status.txt" 2>&1
sw_cmd tree --listen="$listen" --config-dir="$config_dir" >"$prefix-tree.txt" 2>&1
[[ -s $prefix-status.txt ]]
grep -Fq -- "$pod" "$prefix-tree.txt"
pass

step 14 "Dry-run both an apply update and a create without storing either"
k get deploy web -o json >"$prefix-before.json"
k apply --dry-run=server -f "$fixture/web-dry-run.json" -o json >"$prefix-dry-update.json"
grep -Eq '"replicas":[[:space:]]*5[,[:space:]]*$' "$prefix-dry-update.json"
printf '%s\n' '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"stage1-dry"}}' >"$prefix-namespace.json"
k apply --dry-run=server -f "$prefix-namespace.json" -o name >"$prefix-dry-create.txt"
grep -Fxq 'namespace/stage1-dry' "$prefix-dry-create.txt"
k get deploy web -o json >"$prefix-after.json"
cmp "$prefix-before.json" "$prefix-after.json"
k get ns stage1-dry --ignore-not-found -o name >"$prefix-namespace-check.txt"
[[ ! -s $prefix-namespace-check.txt ]]
pass

step 15 "Check exact HTTP statuses and the stored-only Warning header"
api_write() {
  local name=$1 method=$2 path=$3 input=$4 expected=$5
  local code content_type=${6:-application/json}
  # Send the fixture token via stdin so it does not appear in curl's command line.
  code=$(curl --noproxy '*' --connect-timeout 2 --max-time 10 -sS --cacert "$config_dir/ca.crt" \
    --header @- --header "Content-Type: $content_type" --request "$method" --data-binary "@$input" \
    --dump-header "$prefix-$name.headers" --output "$prefix-$name.response.json" \
    --write-out '%{http_code}' "$api$path" <<<"Authorization: Bearer $(<"$config_dir/admin.token")")
  printf '%s\n' "$code" >"$prefix-$name.code"
  [[ $code == "$expected" ]] || die "$name returned HTTP $code, expected $expected"
}
deploy_path=/apis/apps/v1/namespaces/default/deployments/web
pods_path='/api/v1/namespaces/default/pods?fieldValidation=Strict'
k get deploy web -o json >"$prefix-stale.json"
k label deploy web stage1-rv=advanced >"$prefix-label.txt"
api_write conflict PUT "$deploy_path" "$prefix-stale.json" 409
grep -Fq 'resourceVersion' "$prefix-conflict.response.json"
printf '%s\n' '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"stage1-rejected"},"spec":{"hostNetwork":true,"containers":[{"name":"nginx","image":"nginx:1.27"}]}}' \
  >"$prefix-rejected.json"
api_write rejected POST "$pods_path" "$prefix-rejected.json" 422
grep -Fq 'spec.hostNetwork' "$prefix-rejected.response.json"
printf '%s\n' '{"spec":{"minReadySeconds":3}}' >"$prefix-stored.json"
api_write stored PATCH "$deploy_path?fieldValidation=Strict" "$prefix-stored.json" 200 application/merge-patch+json
grep -Eiq '^Warning: .*spec.minReadySeconds.*stored' "$prefix-stored.headers"
[[ $(k get deploy web -o jsonpath='{.spec.minReadySeconds}') == 3 ]]
printf '%s\n' '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"stage1-unknown"},"spec":{"unknownStage1":true,"containers":[{"name":"nginx","image":"nginx:1.27"}]}}' \
  >"$prefix-unknown.json"
api_write unknown POST "$pods_path" "$prefix-unknown.json" 400
grep -Fq 'spec.unknownStage1' "$prefix-unknown.response.json"
pass

step 16 "Delete the Deployment and Service and check for leaked test resources"
verify pod-network "$run_dir/netns" 4 >"$prefix-net-before.json"
verify allocations "$db" >"$prefix-ip-before.tsv"
k delete deploy web --cascade=foreground --wait=true --timeout=75s >"$prefix-delete-deploy.txt"
k delete svc web --wait=true --timeout=30s >"$prefix-delete-service.txt"
resources_gone() {
  k --request-timeout=5s get deploy,rs,pods,svc,endpointslices -o name >"$prefix-remaining.txt" &&
    [[ ! -s $prefix-remaining.txt ]] &&
    runtime_empty "$prefix-runtime-after" &&
    verify pod-network "$run_dir/netns" 0 >"$prefix-net-after.json" 2>"$prefix-net.err" &&
    verify allocations "$db" >"$prefix-ip-after.tsv" &&
    [[ ! -s $prefix-ip-after.tsv ]]
}
retry 40 resources_gone
note "SKIP 16 Direct comparison of moby and moby_history is prohibited"
pass

step 17 "Measure the footprint and record it in README"
note "DEFER 17 Run hack/measure.sh separately and retain its measurements"

step 18 "Check first-use kubeconfig instructions, diagnostics and doctor"
# Run the printed kubectl invocation with the supplied binary and private cache.
awk -v path="$kubeconfig" 'index($0, "kubectl") && index($0, path) {found=1} END {exit !found}' "$fixture/up-initial.txt"
k get nodes >"$prefix-kubeconfig-use.txt"
grep -Fq "$node" "$prefix-kubeconfig-use.txt"
sw_cmd doctor "${flags[@]}" >"$prefix-doctor.txt" 2>&1
pass

step 19 "List Pods across namespaces, watch a label leave app=web, and test scale preconditions"
other_ns=stage1-other
k create namespace "$other_ns" >"$prefix-namespace.txt"
k -n "$other_ns" create deployment web --image=nginx:1.27 --replicas=0 >"$prefix-deploy.txt"
k -n "$other_ns" scale deploy/web --current-replicas=0 --replicas=1 >"$prefix-scale.txt"
if k -n "$other_ns" scale deploy/web --current-replicas=0 --replicas=2 >"$prefix-scale-rejected.txt" 2>&1; then
  die "kubectl scale accepted the wrong current-replicas value"
fi
grep -Ei 'expected.*replicas|precondition' "$prefix-scale-rejected.txt"
[[ $(k -n "$other_ns" get deploy web -o jsonpath='{.spec.replicas}') == 1 ]]
k -n "$other_ns" rollout status deploy/web --timeout=120s >"$prefix-rollout.txt" 2>&1

cat >"$fixture/reuse.json" <<'JSON'
{
  "apiVersion": "v1",
  "kind": "Pod",
  "metadata": {"name": "stage1-reuse", "labels": {"app": "web"}},
  "spec": {"restartPolicy": "Always", "containers": [{"name": "nginx", "image": "nginx:1.27"}]}
}
JSON
reuse=stage1-reuse
k create -f "$fixture/reuse.json" >"$prefix-create.txt"
k wait --for=condition=Ready "pod/$reuse" --timeout=90s >"$prefix-ready.txt"
reuse_uid=$(pod_value "$reuse" '{.metadata.uid}')
k get pods -A >"$prefix-all-pods.txt"
awk -v ns="$other_ns" -v pod="$reuse" '$1 == ns {other=1} $1 == "default" && $2 == pod {default_pod=1} END {exit !(other && default_pod)}' \
  "$prefix-all-pods.txt"
"${kube[@]}" --request-timeout=45s get pods -l app=web -w --output-watch-events -o json \
  >"$prefix-watch.json" 2>"$prefix-watch.err" &
job_pid=$!
selector_event() {
  verify watch-event "$prefix-watch.json" "$1" "$reuse_uid" web 2>"$prefix-watch-assert.err"
}
retry 15 selector_event ADDED
k label "pod/$reuse" app- >"$prefix-label.txt"
retry 15 selector_event DELETED
stop_job
[[ $(pod_value "$reuse" '{.metadata.uid}{.metadata.labels.app}') == "$reuse_uid" ]]
pass

step 20 "Stop only sw server, kill a task offline and verify exit catch-up"
k get "pod/$reuse" -o json >"$prefix-before.json"
cid=$(container_id "$reuse" nginx)
[[ $cid == containerd://?* ]]
task_id=${cid#containerd://}
pod_ip=$(pod_value "$reuse" '{.status.podIP}')
running_tasks "$prefix-before.tasks"
sw_cmd down "${flags[@]}" >"$prefix-down.txt" 2>&1
retry 10 api_down "$api" "$prefix-listener-down.txt"
running_tasks "$prefix-offline.tasks"
cmp "$prefix-before.tasks.running" "$prefix-offline.tasks.running"
retry 10 nginx_ok "http://$pod_ip/" "$prefix-direct-offline.html"
killed=$(date +%s)
ctr_sw tasks kill --signal SIGKILL "$task_id"
task_stopped() {
  ctr_sw tasks list >"$prefix-exited.tasks" &&
    awk -v id="$task_id" '$1 == id && $3 == "STOPPED" {found=1} END {exit !found}' "$prefix-exited.tasks"
}
retry 15 task_stopped
resumed=$(date +%s)
api_down "$api" "$prefix-listener-before-up.txt"
printf 'killed_at=%s\nobserved_stopped_at=%s\n' "$killed" "$resumed" >"$prefix-outage.txt"
start after-offline-exit
retry 90 restarted "$reuse" nginx "$killed" "$resumed"
retry 15 nginx_ok "http://$pod_ip/" "$prefix-direct-restored.html"
pass

step 21 "Recreate the same Pod name while kubectl delete is still waiting"
k patch "pod/$reuse" --type=merge -p '{"metadata":{"finalizers":["stage1.shearwater.test/hold"]}}' >"$prefix-hold.txt"
"${kube[@]}" --request-timeout=40s delete "pod/$reuse" --grace-period=1 --wait=true --timeout=35s \
  >"$prefix-delete.txt" 2>&1 &
job_pid=$!
old_pod_stopped() {
  [[ $(pod_value "$reuse" '{.metadata.deletionGracePeriodSeconds}') == 0 ]] &&
    grep -Fq "pod \"$reuse\" deleted" "$prefix-delete.txt"
}
retry 15 old_pod_stopped
# The finalizer holds the old object until its task is gone. Pause this script's
# kubectl child so the replacement exists before the client resumes its wait.
kill -STOP "$job_pid"
k patch "pod/$reuse" --type=merge -p '{"metadata":{"finalizers":[]}}' >"$prefix-release.txt"
k create -f "$fixture/reuse.json" >"$prefix-recreate.txt"
new_uid=$(pod_value "$reuse" '{.metadata.uid}')
[[ -n $new_uid && $new_uid != "$reuse_uid" ]]
kill -CONT "$job_pid"
wait "$job_pid"
job_pid=
k wait --for=condition=Ready "pod/$reuse" --timeout=90s >"$prefix-ready.txt"
[[ $(pod_value "$reuse" '{.metadata.uid}') == "$new_uid" ]]
printf 'old_uid=%s\nnew_uid=%s\n' "$reuse_uid" "$new_uid" >"$prefix-uids.txt"
pass

step teardown "Run sw down --all with live resources and retain the fixture"
k -n "$other_ns" expose deploy web --port=80 >"$prefix-expose.txt"
cluster_ip=$(k -n "$other_ns" get svc web -o jsonpath='{.spec.clusterIP}')
[[ -n $cluster_ip && $cluster_ip != None ]]
pod_ip=$(pod_value "$reuse" '{.status.podIP}')
other_deploy_uid=$(k -n "$other_ns" get deploy web -o jsonpath='{.metadata.uid}')
[[ -n $pod_ip && -n $other_deploy_uid ]]
allocation_snapshot() {
  verify allocations "$db" >"$prefix-ip-before.tsv" &&
    [[ $(wc -l <"$prefix-ip-before.tsv") == 3 ]] &&
    grep -Fq -- "$new_uid" "$prefix-ip-before.tsv" &&
    grep -Eq '^service[[:space:]]' "$prefix-ip-before.tsv"
}
chain_absent() {
  if iptables -w 3 -t "$1" -S "$2" >"$3" 2>&1; then
    die "Owned iptables chain $2 is still present"
  fi
  grep -Eiq 'no chain|does not exist|not found' "$3"
}
down_all() {
  local attempt log_file
  retry 25 nginx_ok "http://$cluster_ip/" "$prefix-service.html"
  retry 15 nginx_ok "http://$pod_ip/?stage1-retained-log" "$prefix-pod.html"
  attempt=$(pod_value "$reuse" '{.status.containerStatuses[?(@.name=="nginx")].restartCount}')
  log_file="$log_dir/pods/default_${reuse}_${new_uid}/nginx/$attempt.log"
  retry 15 grep -Fq 'GET /?stage1-retained-log ' "$log_file"
  # Query the current network namespace, not the namespace of the sysfs mount.
  ip link show dev sw-br0 >"$prefix-bridge-before.txt"
  ip link show dev sw-svc0 >"$prefix-service-link-before.txt"
  iptables -w 3 -t filter -S SHEARWATER-FORWARD >"$prefix-filter-before.txt"
  iptables -w 3 -t nat -S SHEARWATER-POSTROUTING >"$prefix-nat-before.txt"
  retry 25 allocation_snapshot
  sha256sum "$kubeconfig" >"$prefix-config.sha256"
  sw_cmd down --all "${flags[@]}" >"$prefix-down-all.txt" 2>&1
  retry 10 api_down "$api" "$prefix-listener-down.txt"
  runtime_empty "$prefix-runtime-after"
  verify pod-network "$run_dir/netns" 0 >"$prefix-net-after.json"
  ! ip link show dev sw-br0 >"$prefix-bridge-after.txt" 2>&1
  ! ip link show dev sw-svc0 >"$prefix-service-link-after.txt" 2>&1
  chain_absent filter SHEARWATER-FORWARD "$prefix-filter-after.txt"
  chain_absent nat SHEARWATER-POSTROUTING "$prefix-nat-after.txt"
  verify allocations "$db" >"$prefix-ip-after.tsv"
  cmp "$prefix-ip-before.tsv" "$prefix-ip-after.tsv"
  sha256sum -c "$prefix-config.sha256" >"$prefix-config-check.txt"
  grep -Fq 'GET /?stage1-retained-log ' "$log_file"
  owned=0
}
down_all
pass

step cold-start "Rebuild the host network from saved state with an empty runtime directory"
# Keep persistent paths and the node name. Give sw an empty /run path to
# model lost volatile state without rebooting WSL or stopping Docker.
run_dir=$(mktemp -d "$run_root/stage1.XXXXXXXX")
flags=(--listen="$listen" --node-name="$node" --data-dir="$fixture/data"
       --config-dir="$config_dir" --run-dir="$run_dir" --log-dir="$log_dir" --containerd-socket="$socket")
printf '%s\n' "$run_dir" >"$prefix-run-dir.txt"
start after-network-loss
# Successful HTTP requests prove that the listeners and tasks came back, even
# if kubectl initially reads a Ready status carried over from the database.
retry 90 nginx_ok "http://$pod_ip/" "$prefix-direct.html"
retry 90 nginx_ok "http://$cluster_ip/" "$prefix-service.html"
k wait --for=condition=Ready "pod/$reuse" --timeout=30s >"$prefix-pod-ready.txt"
k -n "$other_ns" rollout status deploy/web --timeout=120s >"$prefix-deploy-ready.txt" 2>&1
[[ $(pod_value "$reuse" '{.metadata.uid} {.status.podIP}') == "$new_uid $pod_ip" ]]
[[ $(k -n "$other_ns" get deploy web -o jsonpath='{.metadata.uid}') == "$other_deploy_uid" ]]
[[ $(k -n "$other_ns" get svc web -o jsonpath='{.spec.clusterIP}') == "$cluster_ip" ]]
k get "pod/$reuse" -o json >"$prefix-pod.json"
k -n "$other_ns" get deploy,rs,pods,svc,endpointslices -o json >"$prefix-other.json"
verify pod-network "$run_dir/netns" 2 >"$prefix-net.json"
pass

step final-teardown "Stop the recovered fixture with sw down --all"
down_all
pass

(( event_failed == 0 )) || die "Stage 1 failed the Pod Events assertion"
note "Stage 1 integration passed for the in-scope checks. Evidence: $fixture"
