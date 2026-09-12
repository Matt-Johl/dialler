#!/bin/sh
# Echo self-test, headless: app 211 (baresip-a) dials the reserved "echo"
# destination and records what comes back — its own tone, relayed by the
# server on the app leg alone (no PBX). With TRUNK=1 it dials 600, the
# PBX's Echo() through the trunk leg.
#
# This is also the audio-quality gate (SPEC §7.2): the recording is
# correlated against the tone that was played, giving the mouth-to-ear
# delay of the whole loop, and every silent stretch inside a tone burst is
# a gap nothing concealed. Bounds (override per run):
#   MAX_DELAY_MS   150 on the server echo, 200 through the PBX
#   MAX_GAP_MS     0   longest tolerated gap
#   MAX_GAPS       0   number of gaps tolerated
# IMPAIR=1 adds 2 % loss and 30 ± 10 ms jitter to everything the server
# sends (harness/netem); the bounds then default to what the codec on that
# path conceals today: Opus (server echo) recovers single losses from its
# in-band FEC and conceals the rest, leaving at most one short gap per call
# from a double loss (measured over six runs: 0–1 gap ≤ 40 ms); G.711/G.722
# through the PBX have no concealment yet, so a few short gaps are allowed
# until it lands (SPEC §6 near-term item 5, plan Phase D).
#
#   make harness-echo
#   TRUNK=1 make harness-echo
#   IMPAIR=1 make harness-echo
set -eu
cd "$(dirname "$0")/.."

PROFILES="--profile test"
[ "${IMPAIR:-0}" = 1 ] && PROFILES="$PROFILES --profile impair"
COMPOSE="docker compose -f harness/docker-compose.yml $PROFILES"
NET=dialler-harness_default
MEDIA_DIR=harness/baresip/media
CALL_SECONDS="${CALL_SECONDS:-8}"
KEEP="${KEEP:-0}"
TARGET="echo@dialler"; WHERE="the server"; DELAY_BOUND=150; GAP_BOUND=0; GAPS_BOUND=0
if [ "${TRUNK:-0}" = 1 ]; then
  TARGET="600@dialler"; WHERE="the PBX through the trunk"; DELAY_BOUND=200
  # This loop is G.722 (G.711 fallback) and neither has concealment yet:
  # with the server's egress impaired on both legs (to the PBX and back to
  # the phone) every lost packet is a gap — measured 8 of 16 drops audible
  # on G.711. Tightened to "concealed" in plan Phase D.
  [ "${IMPAIR:-0}" = 1 ] && { GAP_BOUND=60; GAPS_BOUND=12; }
else
  [ "${IMPAIR:-0}" = 1 ] && { GAP_BOUND=40; GAPS_BOUND=1; }
fi
# Under impairment the shaper's own 30 ms is real path delay and the
# adaptive jitter buffer is expected to grow into the 10 ms jitter: +50 ms.
[ "${IMPAIR:-0}" = 1 ] && DELAY_BOUND=$((DELAY_BOUND + 50))
MAX_DELAY_MS="${MAX_DELAY_MS:-$DELAY_BOUND}"
MAX_GAP_MS="${MAX_GAP_MS:-$GAP_BOUND}"
MAX_GAPS="${MAX_GAPS:-$GAPS_BOUND}"

python3 harness/baresip/media/gen_tone.py "$MEDIA_DIR/in.wav" >/dev/null
rm -f "$MEDIA_DIR"/out-*.wav

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up"
$COMPOSE up --build -d dialler asterisk >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a >/dev/null 2>&1
sleep 5
if [ "${IMPAIR:-0}" = 1 ]; then
  # Last, once nothing will recreate the server: the sidecar shapes the
  # server container's own interface, and a later `up` that recreated the
  # server would leave it shaping a dead namespace (seen: the first runs
  # measured an unimpaired path).
  $COMPOSE up --build -d netem >/dev/null 2>&1
  sleep 1
  echo "== impaired: $($COMPOSE logs --no-log-prefix netem 2>&1 | grep 'netem applied' | tail -1)"
fi

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

echo "== app 211 dials $TARGET (echo by $WHERE)"
ctl "{\"command\":\"dial\",\"params\":\"$TARGET\"}"
sleep "$CALL_SECONDS"
ctl '{"command":"hangup"}'
sleep 2
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'echo|invite|bridged|call ended|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'
if [ "${IMPAIR:-0}" = 1 ]; then
  # Proof the shaping applied to this call: netem's own counters.
  stats="$($COMPOSE exec -T netem tc -s qdisc show dev eth0 2>&1 | tr '\n' ' ' | sed 's/  */ /g')"
  echo "   netem: $stats"
  case "$stats" in *"dropped 0,"*|*"Cannot find"*) echo "FAIL: the impairment did not apply (no packets dropped)"; exit 1 ;; esac
fi

echo "== media: what the app heard back (delay ≤ ${MAX_DELAY_MS} ms, gaps ≤ ${MAX_GAPS} × ${MAX_GAP_MS} ms)"
if python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-211.wav" --reference "$MEDIA_DIR/in.wav" \
     --max-delay-ms "$MAX_DELAY_MS" --max-gap-ms "$MAX_GAP_MS" --max-gaps "$MAX_GAPS"; then
  echo "PASS: the app heard its own audio back from $WHERE within the quality bounds"
else
  echo "FAIL: echo from $WHERE outside the quality bounds"; exit 1
fi
