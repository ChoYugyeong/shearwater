#!/usr/bin/env bash
# M2 first checks (design 7): small experiments before the runtime is written.
#   1. does the shim write the container's output straight to a log file?
#   2. do rules behind DOCKER-USER let pods reach each other and the outside?
#   3. MTU
#   4. after a reboot, can a pod come back with the same IP? (simulated: the
#      namespaces, links and rules are removed by hand; WSL is not restarted)
#
# Run as root in WSL:  bash hack/m2-probe.sh all      (setup + checks + cleanup)
#                      bash hack/m2-probe.sh cleanup  (after an interrupted run)
#
# It uses `ctr`, `ip` and `iptables` because the question here is whether the
# approach works on this host, not whether our netlink code does. It touches only
# the containerd namespace "shearwater", /run/shearwater, /var/log/shearwater,
# the bridge sw-br0, veths named swprobe*, and the chains SHEARWATER-*.
set -u

NS=shearwater
IMG=docker.io/library/busybox:1.36
BR=sw-br0
NETNS_DIR=/run/shearwater/netns
LOG_DIR=/var/log/shearwater/probe
POD_CIDR=10.244.0.0/24
SVC_CIDR=10.96.0.0/16
GW=10.244.0.1
IP_A=10.244.0.2
IP_B=10.244.0.3
OUT_IP=1.1.1.1
MTU=$(cat /sys/class/net/eth0/mtu)

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "  ok    $*"; }
bad() { fail=$((fail + 1)); echo "  FAIL  $*"; }
# check NAME expect(0|1) CMD...: expect 0 means the command must succeed.
check() {
  local name=$1 want=$2
  shift 2
  if "$@" >/dev/null 2>&1; then got=0; else got=1; fi
  if [ "$got" = "$want" ]; then ok "$name"; else bad "$name (want exit $want, got $got)"; fi
}
in_a() { nsenter --net=$NETNS_DIR/probe-a "$@"; }
in_b() { nsenter --net=$NETNS_DIR/probe-b "$@"; }

need_root() {
  if [ "$(id -u)" != 0 ]; then
    echo "run as root" >&2
    exit 1
  fi
}

bridge_up() {
  if ! ip link show $BR >/dev/null 2>&1; then
    ip link add $BR mtu "$MTU" type bridge
    ip addr add $GW/24 dev $BR
  fi
  ip link set $BR up
}

# pod_net NAME IP: a namespace bound at $NETNS_DIR/NAME with eth0 = IP.
pod_net() {
  local name=$1 addr=$2 host=sw$1
  host=${host//-/}
  mkdir -p $NETNS_DIR
  touch $NETNS_DIR/"$name"
  unshare --net=$NETNS_DIR/"$name" true
  nsenter --net=$NETNS_DIR/"$name" ip link add eth0 mtu "$MTU" type veth peer name "$host" netns 1
  ip link set "$host" mtu "$MTU" master $BR up
  nsenter --net=$NETNS_DIR/"$name" sh -c "ip link set lo up && ip addr add $addr/24 dev eth0 && ip link set eth0 up && ip route add default via $GW"
}

pod_net_down() {
  local name=$1
  if [ -e $NETNS_DIR/"$name" ]; then
    umount $NETNS_DIR/"$name" 2>/dev/null
    rm -f $NETNS_DIR/"$name"
  fi
}

rules_up() {
  iptables -w -N SHEARWATER-FORWARD 2>/dev/null
  iptables -w -F SHEARWATER-FORWARD
  iptables -w -A SHEARWATER-FORWARD -i $BR -o $BR -j ACCEPT
  iptables -w -A SHEARWATER-FORWARD -i $BR ! -o $BR -j ACCEPT
  iptables -w -A SHEARWATER-FORWARD -o $BR -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  iptables -w -C DOCKER-USER -j SHEARWATER-FORWARD 2>/dev/null || iptables -w -I DOCKER-USER 1 -j SHEARWATER-FORWARD
  iptables -w -t nat -N SHEARWATER-POSTROUTING 2>/dev/null
  iptables -w -t nat -F SHEARWATER-POSTROUTING
  iptables -w -t nat -A SHEARWATER-POSTROUTING -d $POD_CIDR -j RETURN
  iptables -w -t nat -A SHEARWATER-POSTROUTING -d $SVC_CIDR -j RETURN
  iptables -w -t nat -A SHEARWATER-POSTROUTING ! -o $BR -j MASQUERADE
  iptables -w -t nat -C POSTROUTING -s $POD_CIDR -j SHEARWATER-POSTROUTING 2>/dev/null || iptables -w -t nat -A POSTROUTING -s $POD_CIDR -j SHEARWATER-POSTROUTING
}

rules_down() {
  iptables -w -D DOCKER-USER -j SHEARWATER-FORWARD 2>/dev/null
  iptables -w -F SHEARWATER-FORWARD 2>/dev/null
  iptables -w -X SHEARWATER-FORWARD 2>/dev/null
  iptables -w -t nat -D POSTROUTING -s $POD_CIDR -j SHEARWATER-POSTROUTING 2>/dev/null
  iptables -w -t nat -F SHEARWATER-POSTROUTING 2>/dev/null
  iptables -w -t nat -X SHEARWATER-POSTROUTING 2>/dev/null
}

container_down() {
  ctr -n $NS task kill -s KILL "$1" >/dev/null 2>&1
  sleep 1
  ctr -n $NS task rm "$1" >/dev/null 2>&1
  ctr -n $NS container rm "$1" >/dev/null 2>&1
}

cleanup() {
  container_down probe-c1
  container_down probe-c2
  pod_net_down probe-a
  pod_net_down probe-b
  ip link del swprobea 2>/dev/null
  ip link del swprobeb 2>/dev/null
  ip link del $BR 2>/dev/null
  rules_down
  rm -f $LOG_DIR/c1.log $LOG_DIR/c2.log
  rmdir $LOG_DIR 2>/dev/null
}

connectivity() {
  local label=$1
  check "$label: pod a -> pod b (ping)" 0 in_a ping -c 2 -W 2 $IP_B
  check "$label: pod b -> pod a (ping)" 0 in_b ping -c 2 -W 2 $IP_A
  check "$label: pod a -> gateway" 0 in_a ping -c 1 -W 2 $GW
  check "$label: host -> pod a" 0 ping -c 1 -W 2 $IP_A
  check "$label: pod a -> $OUT_IP (ping)" 0 in_a ping -c 2 -W 3 $OUT_IP
  check "$label: pod a -> http://$OUT_IP (TCP)" 0 in_a curl -sS -o /dev/null --max-time 15 http://$OUT_IP/
}

all() {
  need_root
  echo "host: $(uname -r), containerd $(containerd --version | awk '{print $3}'), eth0 mtu $MTU"
  echo "resolv.conf: $(grep -v '^#' /etc/resolv.conf | tr '\n' ' ')"
  cleanup >/dev/null 2>&1
  moby_before=$(ctr namespaces ls -q | sort | tr '\n' ' ')

  echo "== setup: bridge, two namespaces"
  bridge_up
  pod_net probe-a $IP_A
  pod_net probe-b $IP_B

  echo "== 2a. before our rules (FORWARD policy $(iptables -S FORWARD | head -1 | awk '{print $3}'), bridge-nf-call-iptables=$(cat /proc/sys/net/bridge/bridge-nf-call-iptables))"
  check "pod a -> pod b is dropped without rules" 1 in_a ping -c 1 -W 2 $IP_B
  check "pod a -> $OUT_IP is dropped without rules" 1 in_a ping -c 1 -W 2 $OUT_IP
  check "pod a -> gateway works without rules (INPUT, not FORWARD)" 0 in_a ping -c 1 -W 2 $GW

  echo "== 2b. with SHEARWATER-FORWARD behind DOCKER-USER and the nat chain"
  rules_up
  connectivity "rules"
  echo "  DOCKER-USER: $(iptables -S DOCKER-USER | tr '\n' ';')"
  echo "  FORWARD:     $(iptables -S FORWARD | tr '\n' ';')"
  src=$(in_a curl -sS --max-time 15 https://$OUT_IP/cdn-cgi/trace 2>/dev/null | grep '^ip=')
  echo "  outside sees pod a as: ${src:-unknown} (a public address means the masquerade applied)"
  check "pod a resolves a name with the host's resolv.conf" 0 in_a curl -sS -o /dev/null --max-time 15 http://example.com/

  echo "== 3. MTU $MTU"
  full=$((MTU - 28))
  check "host -> $OUT_IP, $MTU-byte packet, don't fragment" 0 ping -c 2 -W 3 -M do -s $full $OUT_IP
  check "pod a -> $OUT_IP, $MTU-byte packet, don't fragment" 0 in_a ping -c 2 -W 3 -M do -s $full $OUT_IP
  check "pod a -> pod b, $MTU-byte packet, don't fragment" 0 in_a ping -c 2 -W 3 -M do -s $full $IP_B
  check "pod a -> pod b, one byte more is refused locally" 1 in_a ping -c 1 -W 2 -M do -s $((full + 1)) $IP_B
  size=$(in_a curl -sS -o /dev/null --max-time 60 -w '%{size_download}' https://speed.cloudflare.com/__down?bytes=20000000 2>/dev/null)
  if [ "${size:-0}" = 20000000 ]; then ok "pod a downloads 20 MB over TLS (got $size bytes)"; else bad "pod a downloads 20 MB (got ${size:-0} bytes)"; fi

  echo "== 1. shim writes the log file"
  mkdir -p $LOG_DIR
  if ! ctr -n $NS images ls -q | grep -qx $IMG; then
    ctr -n $NS images pull $IMG >/dev/null 2>&1 || bad "pull $IMG"
  fi
  ctr -n $NS run -d --with-ns network:$NETNS_DIR/probe-a --log-uri file://$LOG_DIR/c1.log $IMG probe-c1 \
    sh -c 'i=0; while true; do echo out $i; echo err $i >&2; i=$((i+1)); sleep 1; done' || bad "ctr run"
  sleep 4
  n1=$(wc -l <$LOG_DIR/c1.log 2>/dev/null || echo 0)
  sleep 3
  n2=$(wc -l <$LOG_DIR/c1.log 2>/dev/null || echo 0)
  if [ "$n2" -gt "$n1" ] && [ "$n1" -gt 0 ]; then ok "log file grows with no client attached ($n1 -> $n2 lines)"; else bad "log file grows ($n1 -> $n2)"; fi
  check "stdout lines are in the file" 0 grep -q '^out 1$' $LOG_DIR/c1.log
  check "stderr lines are in the same file" 0 grep -q '^err 1$' $LOG_DIR/c1.log
  pid=$(ctr -n $NS task ls | awk '$1=="probe-c1"{print $2}')
  shim=$(ps -o ppid= -p "$pid" | tr -d ' ')
  echo "  task pid $pid, parent $shim: $(tr '\0' ' ' </proc/"$shim"/cmdline | cut -c1-90)"
  if ls -l /proc/"$shim"/fd 2>/dev/null | grep -q "$LOG_DIR/c1.log"; then ok "the shim holds the log file open"; else bad "the shim holds the log file open"; fi
  if ls -l /proc/"$pid"/fd 2>/dev/null | grep -q "$LOG_DIR/c1.log"; then echo "  note: the container process itself has the file open"; else echo "  the container process writes to a pipe, not the file: $(readlink /proc/"$pid"/fd/1)"; fi
  echo "  first lines: $(head -4 $LOG_DIR/c1.log | tr '\n' '|')"

  echo "== container inside the prepared namespace"
  addr=$(ctr -n $NS task exec --exec-id p1 probe-c1 ip -4 -o addr show eth0 2>/dev/null | awk '{print $4}')
  if [ "$addr" = "$IP_A/24" ]; then ok "the container sees eth0 $addr"; else bad "the container sees eth0 '$addr'"; fi
  check "container -> pod b" 0 ctr -n $NS task exec --exec-id p2 probe-c1 ping -c 1 -W 2 $IP_B
  # http://1.1.1.1 answers with a redirect to https. busybox wget follows it and then
  # fails (the container has no resolv.conf here), so seeing it move on to port 443
  # is the evidence that the HTTP answer came back over TCP.
  check "container -> $OUT_IP:80 (TCP, the redirect comes back)" 0 sh -c \
    "ctr -n $NS task exec --exec-id p3 probe-c1 wget -T 15 -O /dev/null http://$OUT_IP/ 2>&1 | grep -q '$OUT_IP:443'"

  echo "== kill: exit status and log survive"
  ctr -n $NS task kill -s KILL probe-c1
  sleep 2
  echo "  task after KILL: $(ctr -n $NS task ls | awk '$1=="probe-c1"{print $3}')"
  check "log file is still there after the task died" 0 test -s $LOG_DIR/c1.log

  echo "== 4. reboot, simulated: remove what /run and the kernel would lose"
  container_down probe-c1
  pod_net_down probe-a
  pod_net_down probe-b
  ip link del $BR
  rules_down
  check "veth of pod a went away with its namespace" 1 ip link show swprobea
  check "the image is still in the namespace" 0 sh -c "ctr -n $NS images ls -q | grep -qx $IMG"
  echo "  rebuild with the same addresses (the allocation would come from SQLite)"
  bridge_up
  rules_up
  pod_net probe-a $IP_A
  pod_net probe-b $IP_B
  connectivity "rebuilt"
  ctr -n $NS run -d --with-ns network:$NETNS_DIR/probe-a --log-uri file://$LOG_DIR/c2.log $IMG probe-c2 sh -c 'echo back; sleep 600' || bad "ctr run after rebuild"
  sleep 2
  addr=$(ctr -n $NS task exec --exec-id p1 probe-c2 ip -4 -o addr show eth0 2>/dev/null | awk '{print $4}')
  if [ "$addr" = "$IP_A/24" ]; then ok "a new container in the rebuilt namespace has the same IP $addr"; else bad "same IP after rebuild: '$addr'"; fi
  check "new container -> pod b" 0 ctr -n $NS task exec --exec-id p2 probe-c2 ping -c 1 -W 2 $IP_B

  echo "== cleanup"
  cleanup
  check "no probe container left" 1 sh -c "ctr -n $NS containers ls -q | grep -q probe"
  check "bridge removed" 1 ip link show $BR
  check "DOCKER-USER is empty again" 1 sh -c "iptables -S DOCKER-USER | grep -q SHEARWATER"
  check "nat POSTROUTING has no jump to us" 1 sh -c "iptables -t nat -S POSTROUTING | grep -q SHEARWATER"
  moby_after=$(ctr namespaces ls -q | sort | tr '\n' ' ')
  echo "  containerd namespaces before: $moby_before / after: $moby_after"

  echo
  echo "$pass passed, $fail failed"
  [ "$fail" = 0 ]
}

case "${1:-}" in
all) all ;;
cleanup)
  need_root
  cleanup
  ;;
*)
  echo "usage: bash hack/m2-probe.sh all|cleanup" >&2
  exit 2
  ;;
esac
