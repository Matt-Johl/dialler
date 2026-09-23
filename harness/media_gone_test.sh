#!/bin/sh
# A bridged call whose parties vanish must end itself.
#
# A B2BUA cannot rely on a BYE to end a call: a phone that crashes, loses
# its network or is suspended sends none, and its dialog then has nothing to
# cancel it. On 2026-09-22 that left a call relaying for 14½ hours with both
# media counters frozen — the app had crashed mid-call and its TLS flow died
# without even an RST reaching the server.
#
# Both phones are SIGKILLed mid-call, which is the same silent death: no BYE,
# no RTP, no RTCP, nothing closed. The server must notice the silence and end
# the call (b2bua watchMedia). The second half asserts the other side of the
# invariant: killing only ONE phone must NOT end the call, because one party
# still being heard means a one-way call, which is a bad call and not a dead
# one. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."

# Short enough to test, long enough that a sweep (timeout/4) and the kill
# cannot race. The production default is 60 s.
TIMEOUT_S="${TIMEOUT_S:-8}"
export DIALLER_MEDIA_TIMEOUT="${TIMEOUT_S}s"
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

# ended_calls counts the calls the server has finished.
ended_calls() { srvlog | grep -c 'msg="call ended"' || true; }

echo "== up (media-timeout=$DIALLER_MEDIA_TIMEOUT)"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 5

# ---- one party gone: the call must survive -------------------------------
echo "== 211 dials 212, then the CALLEE alone is killed"
ctl '{"command":"dial","params":"212@dialler"}'
sleep 6
srvlog | grep -q 'msg=bridged' || { echo "FAIL: the call never bridged"; exit 1; }
before=$(ended_calls)

$COMPOSE kill -s SIGKILL baresip-b >/dev/null 2>&1
# Well past the timeout: the surviving caller is still sending, so the call
# must still be up. (baresip-a has its own 30 s dead-media timeout, which is
# why this window stays short.)
sleep $((TIMEOUT_S + 6))
if [ "$(ended_calls)" -ne "$before" ]; then
  srvlog | grep -E 'no media|call ended' | cut -c1-200 | tail -4 | sed 's/^/   /'
  echo "FAIL: ended a call one party was still on"; exit 1
fi
echo "   ok: still up with one party sending"

# ---- both parties gone: the call must end --------------------------------
echo "== now the CALLER too: nobody is on the call and no BYE can arrive"
$COMPOSE kill -s SIGKILL baresip-a >/dev/null 2>&1

# Give it the timeout plus a sweep and some slack.
waited=0
limit=$((TIMEOUT_S * 2 + 15))
while [ "$waited" -lt "$limit" ]; do
  [ "$(ended_calls)" -gt "$before" ] && break
  sleep 2
  waited=$((waited + 2))
done

echo "== server"
srvlog | grep -E 'no media|call ended|msg=bridged' | cut -c1-200 | tail -5 | sed 's/^/   /'

if [ "$(ended_calls)" -le "$before" ]; then
  echo "FAIL: a call nobody was on was still running after ${limit}s"; exit 1
fi
srvlog | grep -q 'no media from either party' || {
  echo "FAIL: the call ended, but not because the media supervisor noticed"; exit 1; }

# And the relay must have stopped with it: no ticks after the teardown.
sleep 5
last_end=$(srvlog | grep -n 'msg="call ended"' | tail -1 | cut -d: -f1)
last_tick=$(srvlog | grep -n 'msg=relay ' | tail -1 | cut -d: -f1)
if [ -n "$last_tick" ] && [ -n "$last_end" ] && [ "$last_tick" -gt "$last_end" ]; then
  echo "FAIL: the relay is still ticking after the call ended (the 2026-09-22 leak)"; exit 1
fi

echo "PASS: a call with no media either way ended itself; one with a live party did not"
