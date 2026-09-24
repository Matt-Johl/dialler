#!/bin/sh
# A bridged call whose party vanishes must end itself.
#
# A B2BUA cannot rely on a BYE: a phone that crashes, loses its network or is
# suspended sends none, and its dialog then has nothing to cancel it. On
# 2026-09-22 that left a call relaying for 14½ hours; on 2026-09-23 a device
# test force-quit the app mid-call and the desk phone talked on into it for as
# long as anyone watched.
#
# The server asks each party, in its own dialog, whether it is still there
# (b2bua qualifyLegs). Here the callee is SIGKILLed mid-call while the caller
# carries on: it stops answering, and the call must end. The first half is the
# case that must NOT end — both parties answering.
#
# What this does NOT need is an exception list. A held, muted or recvonly
# party answers an OPTIONS exactly as a talking one does, which is why
# harness-hold-music and harness-pbx-hold pass unchanged at a short
# -peer-timeout; run them that way when changing any of this. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."

# Short enough to test, long enough that an ask (timeout/4) and the kill
# cannot race. The production default is 60 s.
TIMEOUT_S="${TIMEOUT_S:-8}"
export DIALLER_PEER_TIMEOUT="${TIMEOUT_S}s"
KEEP="${KEEP:-0}"

# HARNESS_NOPORTS=1 publishes nothing on the host, so this can run beside a
# native `make dev-server`. Everything here lives inside the compose network.
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test"
NET=dialler-harness_default

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

srvlog() { $COMPOSE logs --no-log-prefix dialler 2>&1; }
ended_calls() { srvlog | grep -c 'msg="call ended"' || true; }

echo "== up (peer-timeout=$DIALLER_PEER_TIMEOUT)"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 5

# ---- both parties answering: the call must survive ------------------------
echo "== 211 dials 212; both parties answering"
ctl '{"command":"dial","params":"212@dialler"}'
sleep 6
srvlog | grep -q 'msg=bridged' || { echo "FAIL: the call never bridged"; exit 1; }
before=$(ended_calls)

sleep $((TIMEOUT_S + 6))
if [ "$(ended_calls)" -ne "$before" ]; then
  srvlog | grep -E 'stopped answering|call ended' | cut -c1-200 | tail -4 | sed 's/^/   /'
  echo "FAIL: ended a call both parties were answering"; exit 1
fi
echo "   ok: still up while both answer"

# ---- one party gone: the call must end ------------------------------------
echo "== SIGKILL the CALLEE; the caller talks on into a party that is gone"
$COMPOSE kill -s SIGKILL baresip-b >/dev/null 2>&1

waited=0
limit=$((TIMEOUT_S * 2 + 20))
while [ "$waited" -lt "$limit" ]; do
  [ "$(ended_calls)" -gt "$before" ] && break
  sleep 2
  waited=$((waited + 2))
done

echo "== server"
srvlog | grep -E 'stopped answering|call ended|msg=bridged' | cut -c1-200 | tail -5 | sed 's/^/   /'

if [ "$(ended_calls)" -le "$before" ]; then
  echo "FAIL: a call the callee had left was still running after ${limit}s"; exit 1
fi
srvlog | grep -q 'party stopped answering' || {
  echo "FAIL: the call ended, but not because the party stopped answering"; exit 1; }

# And the relay must have stopped with it: no ticks after the teardown.
sleep 5
last_end=$(srvlog | grep -n 'msg="call ended"' | tail -1 | cut -d: -f1)
last_tick=$(srvlog | grep -n 'msg=relay ' | tail -1 | cut -d: -f1)
if [ -n "$last_tick" ] && [ -n "$last_end" ] && [ "$last_tick" -gt "$last_end" ]; then
  echo "FAIL: the relay is still ticking after the call ended (the 2026-09-22 leak)"; exit 1
fi

echo "PASS: the call ended when a party stopped answering, and not while both did"
