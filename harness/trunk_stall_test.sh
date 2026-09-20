#!/bin/sh
# A trunk connection that goes silently dead must not swallow calls.
#
# The server keeps its TCP/TLS connection to the PBX pooled and reuses it
# for every outbound INVITE. After a network interruption on either side the
# socket can stay open here while its packets go nowhere, and TCP takes a
# minute or two to notice; meanwhile every call out to the PBX sat on it
# until Timer B (32 s), and the INVITE, delivered a minute late, rang the
# PBX phone after the caller had gone (2026-09-19 13:09, the Mac back from
# an outage). Two defences, both asserted here (b2bua: errTrunkUnresponsive,
# qualify.go):
#
#   A. the per-call watchdog: a live PBX answers an INVITE with 100 Trying
#      within milliseconds, so no response at all within 3 s means a dead
#      connection — it is dropped and the call redialled once, fresh;
#   B. the OPTIONS qualify: the same rule applied every 10 s without a call,
#      so the dead connection is gone before anyone dials and the next call
#      rings at once, with no watchdog wait.
#
# The trunk is TLS (the pooled case). The server's packets TO the PBX are
# black-holed with a tc filter in its own namespace — the socket stays open,
# the PBX can still reach the server. Phase A runs with the qualify off so
# only the watchdog can act. Publishes no host ports. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test --profile impair"
NET=dialler-harness_default
ASTERISK_IP=172.30.0.20
KEEP="${KEEP:-0}"

# The pooled case needs a connection-oriented trunk: TLS, as trunk_test.sh.
sh harness/tls/gen_certs.sh
export ASTERISK_TLS=yes
export DIALLER_TRUNK="sip:$ASTERISK_IP:5061;transport=tls"
export DIALLER_TRUNK_ADDR=":5062"
export DIALLER_TRUNK_TLS_CERT=/tls/dialler.pem
export DIALLER_TRUNK_TLS_KEY=/tls/dialler.key
export DIALLER_TRUNK_TLS_CA=/tls/ca.pem
# The impairment sidecar is only the tool here: no loss, no delay of its own.
export NETEM_LOSS=0% NETEM_DELAY=0ms NETEM_JITTER=0ms

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

ctl() { # phone json
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$2'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 $1 4444 >/dev/null"
}
logs() { $COMPOSE logs --no-log-prefix --since "$1" dialler 2>&1; }
tc_in_server() { $COMPOSE exec -T netem sh -c "$1"; }
blackhole_on() {
  tc_in_server "tc qdisc replace dev eth0 root handle 1: prio \
    && tc filter add dev eth0 parent 1: protocol ip prio 1 u32 match ip dst $ASTERISK_IP/32 flowid 1:3 \
    && tc qdisc add dev eth0 parent 1:3 handle 30: netem loss 100%" >/dev/null
  tc_in_server "tc qdisc show dev eth0" | grep -q "loss 100%" || { echo "FAIL: the black hole did not apply"; exit 1; }
}
blackhole_off() { tc_in_server "tc qdisc del dev eth0 root" >/dev/null; }
# wait_for <seconds> <what> <cmd...>: poll cmd once a second.
wait_for() {
  n=$1; what=$2; shift 2
  i=0
  until "$@"; do
    i=$((i+1)); [ $i -lt "$n" ] || { echo "FAIL: $what"; return 1; }
    sleep 1
  done
}
warm_call() { # one 211 → 100 call, bridged and ended: the pooled connection now exists
  MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  ctl baresip-a '{"command":"dial","params":"100@dialler"}'
  wait_for 20 "the warm-up call never bridged" sh -c "$COMPOSE logs --no-log-prefix --since $MARK dialler 2>&1 | grep -q 'bridged.*to=100'" || {
    logs "$MARK" | grep -E 'invite|level=(ERROR|WARN)' | tail -5 | sed 's/^/   /'; exit 1; }
  ctl baresip-a '{"command":"hangup"}'
  sleep 2
}

echo "== up: server (TLS trunk, qualify OFF for phase A), asterisk, app 211, desk phone 100, netem sidecar"
DIALLER_TRUNK_QUALIFY=0 $COMPOSE up --build -d dialler asterisk netem >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d --no-deps baresip-a baresip-c >/dev/null 2>&1
wait_for 40 "Asterisk never qualified the trunk" sh -c "$COMPOSE logs --no-log-prefix asterisk 2>&1 | grep -q 'is now Reachable'"
logs 2m | grep -q "b2bua listening.*trunk_tls=true" || { echo "FAIL: the server is not on a TLS trunk"; exit 1; }

echo "== A1: 211 dials 100 to warm the pooled trunk connection"
warm_call
echo "   bridged and ended"

echo "== A2: black hole on; 211 dials 100 straight into it"
blackhole_on
MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
START=$(date +%s)
ctl baresip-a '{"command":"dial","params":"100@dialler"}'
# 3 s for the dead pooled connection, 3 s for the redial that cannot
# connect either; 15 s is generous and under half of Timer B.
wait_for 15 "the caller was not answered within 15 s — the call sat on the dead connection" \
  sh -c "$COMPOSE logs --no-log-prefix --since $MARK baresip-a 2>&1 | grep -qE 'session closed: 4[0-9][0-9]|Temporarily Unavailable'" || {
  logs "$MARK" | grep -E 'invite|trunk|level=(ERROR|WARN)' | tail -8 | sed 's/^/   /'; exit 1; }
ELAPSED=$(( $(date +%s) - START ))
logs "$MARK" | grep -q "no response from the trunk; its connection is dead" || { echo "FAIL: the watchdog did not detect the dead connection"; exit 1; }
logs "$MARK" | grep -q "dropping its pooled connection" || { echo "FAIL: the dead connection was not dropped from the pool"; exit 1; }
logs "$MARK" | grep -q "redialling on a fresh one" || { echo "FAIL: no redial was attempted"; exit 1; }
logs "$MARK" | grep -E 'trunk|redial|invite callee' | tail -4 | sed 's/^/   /'
echo "   caller answered ${ELAPSED}s after dialling (Timer B would be 32 s)"

echo "== A3: black hole lifted; 211 dials 100 again — must bridge on a fresh connection"
blackhole_off
sleep 1
MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ctl baresip-a '{"command":"dial","params":"100@dialler"}'
wait_for 12 "the call after the black hole did not bridge — the dead connection is still in use" \
  sh -c "$COMPOSE logs --no-log-prefix --since $MARK dialler 2>&1 | grep -q 'bridged.*to=100'" || {
  logs "$MARK" | grep -E 'invite|trunk|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'; exit 1; }
ctl baresip-a '{"command":"hangup"}'
sleep 2
echo "   PASS A: a dead trunk connection was detected in ~3 s, dropped and redialled; the next call bridged afresh"

echo "== B0: server recreated with the OPTIONS qualify on (10 s); 211 registers afresh"
DIALLER_TRUNK_QUALIFY=10s $COMPOSE up -d --no-deps --force-recreate dialler netem baresip-a >/dev/null 2>&1
wait_for 30 "211 never re-registered" sh -c "$COMPOSE logs --no-log-prefix --since 40s dialler 2>&1 | grep -q 'sip register.*user=211'"
logs 40s | grep -q "trunk: qualifying with OPTIONS" || { echo "FAIL: the qualify did not start"; exit 1; }

echo "== B1: 211 dials 100 to warm the pooled trunk connection"
warm_call
echo "   bridged and ended"

echo "== B2: black hole on; nobody dials — the qualify alone must find and drop the dead connection"
MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
blackhole_on
# One qualify interval plus its 3 s timeout, with slack.
wait_for 20 "the qualify did not drop the dead connection within 20 s" \
  sh -c "$COMPOSE logs --no-log-prefix --since $MARK dialler 2>&1 | grep -q 'not answering OPTIONS; dropping its pooled connection'"
logs "$MARK" | grep -E 'trunk:' | tail -2 | sed 's/^/   /'

echo "== B3: black hole lifted; 211 dials 100 at once — must ring with no watchdog wait"
blackhole_off
MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
START=$(date +%s)
ctl baresip-a '{"command":"dial","params":"100@dialler"}'
wait_for 5 "the call after the qualify did not bridge within 5 s" \
  sh -c "$COMPOSE logs --no-log-prefix --since $MARK dialler 2>&1 | grep -q 'bridged.*to=100'" || {
  logs "$MARK" | grep -E 'invite|trunk|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'; exit 1; }
ELAPSED=$(( $(date +%s) - START ))
if logs "$MARK" | grep -q "no response from the trunk"; then
  echo "FAIL: the call still hit a dead connection and paid the watchdog wait"; exit 1
fi
ctl baresip-a '{"command":"hangup"}'
sleep 2
logs "$MARK" | grep -E 'trunk: answering|callee answered|bridged' | tail -3 | sed 's/^/   /'
echo "   bridged ${ELAPSED}s after dialling, on a fresh connection, no watchdog line"
echo "PASS: watchdog (A) and OPTIONS qualify (B) both recover a silently dead trunk connection"
