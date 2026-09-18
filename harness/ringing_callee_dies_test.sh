#!/bin/sh
# The callee's registration connection dies WHILE its INVITE is out: the
# phone was suspended by iOS 160 ms after registering (CallKit had refused
# to present the call — a Focus filter, 2026-09-18 01:55) and the socket
# went with it. Nothing can answer on that connection, so the caller must
# be told 480 within a second or two, not left ringing until the 30 s ring
# timeout or its own patience runs out (15 s that night).
#
# 212 is started with answermode=manual, so it rings and never answers;
# 211 dials it; two seconds into the ringing the callee container is
# SIGKILLed (its sockets close, no unregister — the shape of a phone going
# away). The server must log the flow's death and answer 211 with 480
# promptly. Exit 0 = pass. Publishes no host ports.
set -eu
cd "$(dirname "$0")/.."
# HARNESS_NOPORTS=1 publishes nothing on the host, so this can run beside a
# native `make dev-server` without fighting it for 7443 / 5061. Safe for any
# test that lives entirely inside the compose network; NOT for ones a
# simulator or a real phone has to reach (sim_call.sh, ring_*.sh, probe_*).
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test"
NET=dialler-harness_default
KEEP="${KEEP:-0}"
# How long the caller may ring after the callee dies before we call it a
# failure: the watch polls every 0.5 s, the CANCEL/480 exchange is
# milliseconds; 5 s is generous and still a sixth of the ring timeout.
LIMIT_SECONDS="${LIMIT_SECONDS:-5}"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

logs() { $COMPOSE logs --no-log-prefix dialler 2>&1; }
ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

echo "== up: server, 211 (caller), 212 (rings, never answers)"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
BARESIP_B_ANSWER_MODE=manual $COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 5
logs | grep -q 'sip register.*user=212' || { echo "FAIL: 212 never registered"; exit 1; }

echo "== 211 dials 212; 212 rings"
ctl '{"command":"dial","params":"212@dialler"}'
sleep 2
logs | grep -q 'registration flow gone' && { echo "FAIL: the flow was already gone before the call; the test did not model a death mid-ring"; exit 1; }

echo "== kill 212 while it rings (SIGKILL: sockets close, no unregister)"
$COMPOSE kill -s SIGKILL baresip-b >/dev/null 2>&1
killed=$(date +%s)
i=0
until logs | grep -q "connection died while ringing"; do
  i=$((i+1))
  if [ $i -gt $((LIMIT_SECONDS * 2)) ]; then
    echo "FAIL: the server did not notice the callee's connection die within ${LIMIT_SECONDS}s"
    logs | grep -E 'invite|callee|call ended|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'
    exit 1
  fi
  sleep 0.5
done
noticed=$(( $(date +%s) - killed ))
echo "   server noticed after ~${noticed}s"

# And the caller was answered, not left ringing: baresip prints the final
# status when a call it placed is refused.
i=0
until $COMPOSE logs --no-log-prefix baresip-a 2>&1 | grep -qE 'session closed: 480|480 Temporarily Unavailable'; do
  i=$((i+1))
  if [ $i -gt $((LIMIT_SECONDS * 2)) ]; then
    echo "FAIL: 211 was not told 480 within ${LIMIT_SECONDS}s of the callee dying"
    $COMPOSE logs --no-log-prefix baresip-a 2>&1 | tail -6 | sed 's/^/   211: /'
    exit 1
  fi
  sleep 0.5
done
answered=$(( $(date +%s) - killed ))
ctl '{"command":"hangup"}' || true
logs | grep -E 'invite callee|call ended|flow gone' | tail -4 | sed 's/^/   /'
echo "PASS: callee died mid-ring; the caller heard 480 ~${answered}s later, not after the ring timeout"
