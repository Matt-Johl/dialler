#!/bin/sh
# A PBX extension that is not registered must be REFUSED, not rung.
#
# The PBX owns its extensions' registration state; this server is a trunk
# peer and keeps none (SPEC §4.4 rule 7). So the rejection has to come from
# Asterisk, and the dialplan has to turn Dial()'s outcome into a SIP status:
# without the dial-status context a failed Dial falls through to a bare
# Hangup(), the caller is left on the ring-back it was given when the call
# started, and a phone that is switched off is indistinguishable from one
# that is ringing (a real call on 2026-09-15 rang for 12 s and then only
# because the caller gave up).
#
# Here the desk phone container is never started, so 100 has no contact:
# Asterisk must answer 480 Temporarily Unavailable, and quickly. Then the
# phone is started and the same call must connect, so the fast refusal is
# not just "the PBX refuses everything". Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."
# No published host ports: this test is safe to run beside a native
# `make dev-server` (harness/docker-compose.noports.yml).
COMPOSE="docker compose -f harness/docker-compose.yml -f harness/docker-compose.noports.yml --profile test"
NET=dialler-harness_default
KEEP="${KEEP:-0}"
export DIALLER_PUBLIC_HOST=dialler

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

echo "== up (dialler + asterisk; desk phone 100 deliberately NOT started)"
$COMPOSE up --build -d dialler asterisk >/dev/null 2>&1
sleep 3
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a >/dev/null 2>&1
sleep 5
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'sip register.*user=211' || { echo "FAIL: 211 never registered"; exit 1; }

echo "== 211 dials 100, which has never registered to the PBX"
ctl '{"command":"dial","params":"100@dialler"}'
sleep 6
ctl '{"command":"hangup"}' 2>/dev/null || true

line="$($COMPOSE logs --no-log-prefix dialler 2>&1 | grep 'invite callee.*to=100' | tail -1)"
echo "   $line"
case "$line" in
  *"480 Temporarily Unavailable"*) ;;
  *"Timer_B"*|*"context canceled"*)
    echo "FAIL: the PBX never answered — it rang an extension with no registration instead of refusing it"; exit 1 ;;
  *) echo "FAIL: expected 480 Temporarily Unavailable from the PBX, got the line above"; exit 1 ;;
esac

# It must be a refusal, not a 30 s ring that happened to end in 480: the
# INVITE and the failure are logged with timestamps, so compare them.
sent="$($COMPOSE logs -t --no-log-prefix dialler 2>&1 | grep 'invite: to trunk.*to=100' | tail -1 | cut -c12-19)"
failed="$($COMPOSE logs -t --no-log-prefix dialler 2>&1 | grep 'invite callee.*to=100' | tail -1 | cut -c12-19)"
secs() { echo "$1" | awk -F: '{print $1*3600 + $2*60 + $3}'; }
took=$(( $(secs "$failed") - $(secs "$sent") ))
[ "$took" -le 5 ] || { echo "FAIL: the 480 took ${took}s — the PBX rang it first instead of refusing at once"; exit 1; }
echo "   PASS: refused with 480 Temporarily Unavailable in ${took}s, no ringing"

# And the caller must never have been told it was ringing. This server used
# to send 180 before it had sent the INVITE anywhere, so every call rang
# instantly whatever the destination; now 180 waits for something that is
# actually alerting, and an unreachable destination goes 100 → 480 so the
# app plays congestion at once (SPEC §4.4 rule 8a).
rang="$($COMPOSE logs --no-log-prefix baresip-a 2>&1 | grep -c '180 Ringing' || true)"
[ "$rang" -eq 0 ] || { echo "FAIL: the caller was told 180 Ringing ($rang×) for a destination that was never reachable"; exit 1; }
echo "   PASS: the caller saw 100 Trying then 480 — never a 180"

echo "== now start the desk phone and dial it again: it must connect"
$COMPOSE up --build -d baresip-c >/dev/null 2>&1
sleep 8
ctl '{"command":"dial","params":"100@dialler"}'
sleep 6
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'bridged.*to=100' || {
  echo "FAIL: a registered desk phone no longer connects"; exit 1; }
ctl '{"command":"hangup"}' 2>/dev/null || true
echo "   PASS: registered desk phone still answers and bridges"
# The other half — that a callee which really IS ringing still gets the
# caller its 180 — is not assertable here: this harness desk phone
# auto-answers and often goes straight to 200 without sending 18x at all.
# `OUTBOUND=212 make sim-call` pins it instead, on the app leg, where the
# callee rings for a known interval and the app must ask for ring-back.

# The case that actually bit (2026-09-15): the phone was switched off, so it
# never unregistered and its contact stayed on file. Dial() then rings a dead
# contact for the full 30 s and the caller hears ring-back the whole time.
# qualify + remove_unavailable on the AOR is what drops it, so CHANUNAVAIL
# reaches the dialplan and becomes a 480. SIGKILL models the switched-off
# phone: no unregister, contact left behind.
echo "== switch the desk phone off (SIGKILL: contact left on file) and dial it"
$COMPOSE kill -s SIGKILL baresip-c >/dev/null 2>&1
if $COMPOSE logs --no-log-prefix asterisk 2>&1 | tail -20 | grep -q 'Removing.*contact.*100'; then
  echo "   (contact already gone)"
fi
# qualify_frequency=5 + qualify_timeout=3 on the harness AOR: the dead
# contact is noticed within about ten seconds.
sleep 14
before="$($COMPOSE logs --no-log-prefix dialler 2>&1 | grep -c 'invite callee.*to=100' || true)"
ctl '{"command":"dial","params":"100@dialler"}'
sleep 6
ctl '{"command":"hangup"}' 2>/dev/null || true
line="$($COMPOSE logs --no-log-prefix dialler 2>&1 | grep 'invite callee.*to=100' | tail -1)"
after="$($COMPOSE logs --no-log-prefix dialler 2>&1 | grep -c 'invite callee.*to=100' || true)"
[ "$after" -gt "$before" ] || { echo "FAIL: the call to the switched-off phone never failed — it is still ringing"; exit 1; }
echo "   $line"
case "$line" in
  *"480 Temporarily Unavailable"*|*"404 Not Found"*)
    echo "   PASS: a switched-off phone is refused, not rung" ;;
  *) echo "FAIL: expected the PBX to refuse a switched-off phone, got the line above"; exit 1 ;;
esac
