#!/bin/sh
# A registration is bound to the TLS connection it arrived on. When that
# connection dies (phone backgrounded / killed / network change) the server
# must NOT dial the stale route; it must treat the user as unregistered and
# use the wake path. Here the callee container is stopped after registering:
# the next call to it must log "registration flow gone" and, since no device
# connection exists to wake, answer 480 quickly. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."
COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
KEEP="${KEEP:-0}"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 5
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'sip register.*user=212' || { echo "FAIL: 212 never registered"; exit 1; }

echo "== kill the callee outright (SIGKILL: no clean unregister, like a phone losing Wi-Fi)"
$COMPOSE kill -s SIGKILL baresip-b >/dev/null 2>&1
sleep 2
if $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'sip unregister.*user=212'; then
  echo "FAIL: callee unregistered cleanly; the test did not model a dead flow"; exit 1
fi

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}
echo "== 211 dials 212"
start=$(date +%s)
ctl '{"command":"dial","params":"212@dialler"}'
sleep 6
ctl '{"command":"hangup"}'

echo "== server"
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'flow gone|wake undeliverable|480|invite callee|bridged' | cut -c1-200 | tail -6 | sed 's/^/   /'
if $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'invite callee'; then
  echo "FAIL: server dialled the dead route"; exit 1
fi
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'registration flow gone' || { echo "FAIL: dead flow not detected"; exit 1; }
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'callee offline, wake undeliverable' || { echo "FAIL: did not fall back to the wake path"; exit 1; }
echo "PASS: dead registration flow detected; call went to the wake path (fast 480)"
