#!/usr/bin/env bash
# Capture the Protobuf bodies the real kubectl sends, as test data for the
# request decoder (design 3.6). Runs sw without root on a spare port with
# --trace and writes each POST body to internal/protobuf/testdata/<client>/<name>.bin.
#
#   bash hack/m1-capture.sh
#   WIN_KUBECTL=/mnt/c/ProgramData/chocolatey/bin/kubectl.exe bash hack/m1-capture.sh
#
# The server's answer does not matter here: the trace records the body whether
# the request is accepted or not. Re-run only to add a client version or a case;
# the files are fixtures and are committed.
set -u
cd "$(dirname "$0")/.."
. hack/env.sh

RUN="$HOME/.cache/shearwater-dev/m1-capture-$(date +%Y%m%d-%H%M%S)"
PORT="${SW_PORT:-16443}"
OUT=internal/protobuf/testdata
mkdir -p "$RUN"
go build -o "$RUN/sw" ./cmd/sw || { echo "build failed"; exit 1; }
"$RUN/sw" server --listen "127.0.0.1:$PORT" --data-dir "$RUN/data" --config-dir "$RUN/etc" --trace "$RUN/trace.jsonl" 2>>"$RUN/server.log" &
SRV=$!
trap 'kill $SRV 2>/dev/null; wait $SRV 2>/dev/null' EXIT
for _ in $(seq 50); do curl -sk "https://127.0.0.1:$PORT/readyz" 2>/dev/null | grep -q ok && break; sleep 0.1; done
"$RUN/sw" kubeconfig --config-dir "$RUN/etc" --server "https://127.0.0.1:$PORT" > "$RUN/kubeconfig"

# name|kubectl arguments
CASES='
create-deployment|create deployment web --image=nginx:1.27
create-deployment-zero-replicas|create deployment zero --image=nginx:1.27 --replicas=0 --port=8080
create-deployment-command|create deployment job --image=busybox:1.36 -- sleep 3600
create-deployment-two-images|create deployment pair --image=nginx:1.27 --image=busybox:1.36 --replicas=2
create-service-clusterip|create service clusterip web --tcp=80:8080
create-service-clusterip-two-ports|create service clusterip two --tcp=80:8080 --tcp=443:https
create-service-clusterip-headless|create service clusterip headless --clusterip=None --tcp=80:80
create-service-nodeport|create service nodeport np --tcp=80:80
create-namespace|create namespace team-b
debug-copy-pod|debug solo --copy-to=solo-copy --set-image=main=nginx:1.28 --profile=baseline --share-processes=false
'

capture() { # capture LABEL KUBECTL...
  local label="$1"; shift
  local dir="$OUT/$label"
  mkdir -p "$dir"
  "$@" delete --ignore-not-found --timeout=10s deploy/web deploy/zero deploy/job deploy/pair svc/web svc/two svc/headless svc/np ns/team-b pod/solo pod/solo-copy >/dev/null 2>&1
  "$@" create -f - >/dev/null 2>&1 <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: solo
  labels:
    app: solo
spec:
  containers:
  - name: main
    image: nginx:1.27
    command: ["nginx", "-g", "daemon off;"]
    ports:
    - containerPort: 80
    env:
    - name: MODE
      value: test
    - name: EMPTY
    resources:
      limits:
        cpu: 500m
        memory: 64Mi
    volumeMounts:
    - name: scratch
      mountPath: /scratch
  volumes:
  - name: scratch
    emptyDir: {}
EOF
  echo "$CASES" | while IFS='|' read -r name args; do
    [ -n "$name" ] || continue
    local seen; seen=$(wc -l < "$RUN/trace.jsonl")
    # shellcheck disable=SC2086
    "$@" $args >"$RUN/out.txt" 2>&1 </dev/null; local code=$?   # kubectl.exe would eat the case list on stdin
    tail -n +$((seen+1)) "$RUN/trace.jsonl" | NAME="$name" DIR="$dir" CODE=$code python3 -c '
import base64, json, os, sys
n = 0
for line in sys.stdin:
    e = json.loads(line)
    if e["method"] != "POST" or "bodyBase64" not in e: continue
    body = base64.b64decode(e["bodyBase64"])
    open(os.path.join(os.environ["DIR"], os.environ["NAME"] + ".bin"), "wb").write(body)
    print("  %-36s %4d bytes  POST %s -> %s  (%s, exit %s)" % (os.environ["NAME"], len(body), e["path"], e["status"], e["userAgent"].split(" ")[0], os.environ["CODE"]))
    n += 1
if n != 1:
    print("  %-36s expected one protobuf POST, saw %d (exit %s)" % (os.environ["NAME"], n, os.environ["CODE"]))'
    [ -s "$dir/$name.bin" ] || sed 's/^/      /' "$RUN/out.txt"
  done
}

echo "== $(kubectl version --client 2>/dev/null | head -1)"
capture "kubectl-$(kubectl version --client -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["clientVersion"]["gitVersion"])')" kubectl --kubeconfig "$RUN/kubeconfig"
if [ -n "${WIN_KUBECTL:-}" ]; then
  WV=$("$WIN_KUBECTL" version --client -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["clientVersion"]["gitVersion"])')
  echo "== Windows kubectl $WV"
  capture "kubectl-$WV-windows" "$WIN_KUBECTL" --kubeconfig "$(wslpath -w "$RUN/kubeconfig")"
fi
echo "run dir: $RUN"
