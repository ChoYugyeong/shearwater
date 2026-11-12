#!/usr/bin/env bash
# M0 check: build sw, run it without root on a spare port with throwaway state,
# and drive it with the real kubectl. No containers are involved.
#
#   bash hack/m0-check.sh            run the checks, stop the server at the end
#   KEEP=1 bash hack/m0-check.sh     leave the server running (prints how to reach it)
#
# State goes to ~/.cache/shearwater-dev/m0-<time> (ext4; SQLite must not sit on /mnt/c).
# Nothing is deleted afterwards; the directory is small and its path is printed.
set -u
cd "$(dirname "$0")/.."
. hack/env.sh

RUN="$HOME/.cache/shearwater-dev/m0-$(date +%Y%m%d-%H%M%S)"
PORT="${SW_PORT:-16443}"
T=hack/testdata/m0
mkdir -p "$RUN"
go build -o "$RUN/sw" ./cmd/sw || { echo "build failed"; exit 1; }

start_server() {
  "$RUN/sw" server --runtime=none --listen "127.0.0.1:$PORT" --data-dir "$RUN/data" --config-dir "$RUN/etc" --trace "$RUN/trace.jsonl" 2>>"$RUN/server.log" &
  SRV=$!
  for _ in $(seq 50); do
    curl -sk "https://127.0.0.1:$PORT/readyz" 2>/dev/null | grep -q ok && return 0
    sleep 0.1
  done
  echo "server did not become ready; see $RUN/server.log"; exit 1
}
stop_server() { kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }

start_server
[ "${KEEP:-}" = 1 ] || trap stop_server EXIT
"$RUN/sw" kubeconfig --config-dir "$RUN/etc" --server "https://127.0.0.1:$PORT" > "$RUN/kubeconfig"
export KUBECONFIG="$RUN/kubeconfig"

PASS=0; FAIL=0
# ok NAME CMD...        the command must succeed
# no NAME CMD...        the command must fail
# has NAME TEXT CMD...  the command's output (stdout+stderr) must contain TEXT
ok()  { local n="$1"; shift; if out=$("$@" 2>&1); then PASS=$((PASS+1)); echo "PASS $n"; else FAIL=$((FAIL+1)); echo "FAIL $n"; echo "$out" | sed 's/^/     /'; fi; }
no()  { local n="$1"; shift; if out=$("$@" 2>&1); then FAIL=$((FAIL+1)); echo "FAIL $n (succeeded, expected failure)"; echo "$out" | sed 's/^/     /'; else PASS=$((PASS+1)); echo "PASS $n"; fi; }
has() { local n="$1" want="$2"; shift 2; out=$("$@" 2>&1); if echo "$out" | grep -qF -- "$want"; then PASS=$((PASS+1)); echo "PASS $n"; else FAIL=$((FAIL+1)); echo "FAIL $n (no \"$want\")"; echo "$out" | sed 's/^/     /'; fi; }
rev() { kubectl get --raw /api/v1/namespaces | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["resourceVersion"])'; }
same_rev() { local n="$1" want="$2" got; got=$(rev); if [ -n "$want" ] && [ "$got" = "$want" ]; then PASS=$((PASS+1)); echo "PASS $n (revision $got)"; else FAIL=$((FAIL+1)); echo "FAIL $n (revision $want -> $got)"; fi; }

echo "== basics"
has "version shows the server"            "Server Version: v1.36.0+shearwater" kubectl version
ok  "cluster-info"                        kubectl cluster-info
has "api-resources lists deployments"     "deployments" kubectl api-resources
has "namespaces exist from the start"     "kube-system" kubectl get namespaces
has "wrong token is refused"              "UNAUTHORIZED" kubectl --token=wrong get pods

echo "== create -f / get"
ok  "create -f (namespace, pod, deployment, service)" kubectl create -f $T/objects.yaml
has "get pods"                            "solo" kubectl get pods
has "get deploy,svc,rs"                   "deployment.apps/web" kubectl get deploy,svc,rs -o name
has "get all"                             "service/web" kubectl get all -o name
has "defaults are filled in"              "restartPolicy: Always" kubectl get pod solo -o yaml
has "deployment defaults"                 "maxSurge: 25%" kubectl get deploy web -o yaml
has "label selector"                      "solo" kubectl get pods -l app=solo -o name
has "label selector excludes"             "No resources found" kubectl get pods -l app=nope
has "field selector"                      "solo" kubectl get pods --field-selector metadata.name=solo -o name
has "all namespaces"                      "default" kubectl get pods -A
has "describe pod"                        "Image:" kubectl describe pod solo
no  "create twice fails"                  kubectl create -f $T/objects.yaml
has "create twice says AlreadyExists"     "AlreadyExists" kubectl create -f $T/objects.yaml
no  "create in a missing namespace fails" kubectl create -n nowhere -f $T/unknown-field.yaml --validate=ignore

echo "== validation"
has "unknown field is refused (strict)"   'unknown field "spec.containers[0].imagePullPolcy"' kubectl create -f $T/unknown-field.yaml
has "unknown field warns with --validate=warn" "imagePullPolcy" kubectl create -f $T/unknown-field.yaml --validate=warn
ok  "...and the object was created"       kubectl get pod typo
has "rejected field explains itself"      "Not supported by Shearwater" kubectl create -f $T/rejected.yaml
has "missing image is invalid"            "spec.containers[0].image: Required value" kubectl create -f $T/rejected.yaml

echo "== dry run"
R0=$(rev)
has "create --dry-run=server prints the object" "created (server dry run)" bash -c "kubectl get pod solo -o yaml | sed 's/name: solo/name: dry/' | kubectl create --dry-run=server -f -"
no  "dry-run stored nothing"              kubectl get pod dry
has "delete --dry-run=server"             "(server dry run)" kubectl delete pod solo --dry-run=server
ok  "...and the pod is still there"       kubectl get pod solo
same_rev "dry runs did not advance the revision" "$R0"

echo "== replace (PUT)"
ok  "replace with a new image"            bash -c "kubectl get deploy web -o yaml | sed 's/nginx:1.27/nginx:1.28/' | kubectl replace -f -"
has "generation went up"                  "2" kubectl get deploy web -o jsonpath='{.metadata.generation}'
has "stale resourceVersion is a conflict" "Conflict" bash -c "kubectl get deploy web -o yaml | sed 's/resourceVersion: .*/resourceVersion: \"1\"/' | kubectl replace -f -"
has "selector cannot change"              "spec.selector" bash -c "kubectl get deploy web -o yaml | sed 's/app: web/app: other/' | kubectl replace -f -"

echo "== delete"
ok  "delete pod (waits for it to be gone)" kubectl delete pod solo --timeout=20s
no  "pod is gone"                         kubectl get pod solo
has "namespace with objects is kept"      "still contains objects" bash -c "kubectl get pod typo -o yaml | sed 's/namespace: default/namespace: team-a/; /resourceVersion/d; /uid:/d' | kubectl create -f - && kubectl delete ns team-a --timeout=20s"
ok  "empty the namespace"                 kubectl delete pod typo -n team-a --timeout=20s
ok  "delete the empty namespace"          kubectl delete ns team-a --timeout=20s
has "default cannot be deleted"           "cannot be deleted" kubectl delete ns default --timeout=20s
ok  "delete -f the rest"                  kubectl delete --ignore-not-found --timeout=20s deploy/web svc/web pod/typo

echo "== bodies kubectl sends for 'create <kind>' (recorded for docs/kubectl-trace.md; the three create commands failed with 415 in M0 and succeed since M1)"
for cmd in "create deployment cap-deploy --image=nginx:1.27" "create service clusterip cap-svc --tcp=80:80" \
           "create namespace cap-ns" "run cap-run --image=nginx:1.27" "expose pod cap-run --port=80"; do
  seen=$(wc -l < "$RUN/trace.jsonl")
  kubectl $cmd >/dev/null 2>&1; code=$?
  tail -n +$((seen+1)) "$RUN/trace.jsonl" | CMD="$cmd" CODE=$code python3 -c '
import json, os, sys
for line in sys.stdin:
    e = json.loads(line)
    if e["method"] != "POST": continue
    body = "protobuf, %d bytes" % (len(e["bodyBase64"]) * 3 // 4) if "bodyBase64" in e else "json"
    print("     kubectl %-48s exit %s: POST %s -> %s, %s (%s)" % (os.environ["CMD"], os.environ["CODE"], e["path"], e["status"], e["contentType"], body))'
done
kubectl delete --ignore-not-found --timeout=20s pod/cap-run svc/cap-run >/dev/null 2>&1

if [ -n "${WIN_KUBECTL:-}" ]; then
  echo "== Windows kubectl through WSL localhost forwarding ($WIN_KUBECTL)"
  WK() { "$WIN_KUBECTL" --kubeconfig "$(wslpath -w "$RUN/kubeconfig")" "$@"; }
  has "windows: version"                  "Server Version: v1.36.0+shearwater" WK version
  ok  "windows: create -f"                WK create -f "$(wslpath -w $T/objects.yaml)"
  has "windows: get"                      "deployment.apps/web" WK get deploy,pods,svc -o name
  ok  "windows: delete"                   WK delete --timeout=20s deploy/web svc/web pod/solo ns/team-a
  for cmd in "create deployment win-deploy --image=nginx:1.27" "create service clusterip win-svc --tcp=80:80" "create namespace win-ns"; do
    seen=$(wc -l < "$RUN/trace.jsonl")
    WK $cmd >/dev/null 2>&1; code=$?
    tail -n +$((seen+1)) "$RUN/trace.jsonl" | CMD="$cmd" CODE=$code python3 -c '
import json, os, sys
for line in sys.stdin:
    e = json.loads(line)
    if e["method"] != "POST": continue
    body = "protobuf, %d bytes" % (len(e["bodyBase64"]) * 3 // 4) if "bodyBase64" in e else "json"
    print("     kubectl.exe %-44s exit %s: POST %s -> %s, %s (%s) %s" % (os.environ["CMD"], os.environ["CODE"], e["path"], e["status"], e["contentType"], body, e["userAgent"].split(" ")[0]))'
  done
fi

echo "== restart keeps state"
ok  "create before restart"               kubectl create -f $T/objects.yaml
R1=$(rev)
stop_server; start_server
has "objects survive a restart"           "deployment.apps/web" kubectl get deploy,pods,svc -o name
same_rev "restart did not advance the revision" "$R1"

echo
echo "$PASS passed, $FAIL failed   (kubectl $(kubectl version --client -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["clientVersion"]["gitVersion"])'))"
echo "run dir: $RUN   (trace.jsonl, server.log, kubeconfig)"
[ "${KEEP:-}" = 1 ] && echo "server left running: export KUBECONFIG=$RUN/kubeconfig   stop with: kill $SRV"
[ "$FAIL" = 0 ]
