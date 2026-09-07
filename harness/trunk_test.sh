#!/bin/sh
# Headless PBX-leg calls (SPEC §4.4 rule 7, Phase 2): the light server as a
# SIP trunk peer of Asterisk, both directions, asserted on recorded audio.
#
#   out:  app 201 (baresip-a) dials 100 → server → trunk (TCP) → Asterisk →
#         desk phone 100 (baresip-c, registered to Asterisk over UDP with
#         Digest, G.711 only). Asserts the desk phone recorded the app's tone.
#   in:   desk phone 100 dials 201 → Asterisk dial plan → PJSIP/201@dialler →
#         server's trunk listener → app 201 (registered). Asserts the app
#         recorded the desk phone's tone.
#
# Both legs run G.711 (the trunk is offered PCMU/PCMA only and the app is
# answered with the codec the trunk took), so the raw relay never transcodes.
#
#   make harness-trunk              # both directions
#   DIRECTION=out make harness-trunk
set -eu
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
MEDIA_DIR=harness/baresip/media
DIRECTION="${DIRECTION:-both}"
CALL_SECONDS="${CALL_SECONDS:-8}"
KEEP="${KEEP:-0}"

python3 harness/baresip/media/gen_tone.py "$MEDIA_DIR/in.wav" >/dev/null
rm -f "$MEDIA_DIR"/out-*.wav

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up: server, asterisk, app 201, desk phone 100"
$COMPOSE up --build -d dialler asterisk >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a baresip-c >/dev/null 2>&1
sleep 6

ctl() { # phone json
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$2'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 $1 4444 >/dev/null"
}

if ! $COMPOSE logs --no-log-prefix baresip-c 2>&1 | grep -q "200 OK"; then
  echo "FAIL: desk phone 100 did not register to Asterisk"
  $COMPOSE logs --no-log-prefix baresip-c 2>&1 | tail -8 | sed 's/^/   /'
  exit 1
fi
echo "   desk phone 100 registered to Asterisk; app 201 registered to the server"

# Asterisk only dials a trunk contact it has qualified (OPTIONS → 200).
i=0
until $COMPOSE logs --no-log-prefix asterisk 2>&1 | grep -q "is now Reachable.*RTT\|Contact dialler/.* is now Reachable"; do
  i=$((i+1)); [ $i -le 40 ] || { echo "FAIL: Asterisk never qualified the server as reachable"; $COMPOSE logs --no-log-prefix asterisk 2>&1 | grep -i reachable | tail -3; exit 1; }
  sleep 1
done
echo "   Asterisk qualified the server's trunk listener (Reachable)"

fail=0
if [ "$DIRECTION" = out ] || [ "$DIRECTION" = both ]; then
  echo "== out: app 201 dials 100 (desk phone via the trunk)"
  ctl baresip-a '{"command":"dial","params":"100@dialler"}'
  sleep "$CALL_SECONDS"
  ctl baresip-a '{"command":"hangup"}'
  sleep 2
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'invite|callee answered|bridged|call ended|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'
  if python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-100.wav"; then
    echo "PASS out: the desk phone heard the app through the trunk"
  else
    echo "FAIL out"; fail=1
    $COMPOSE logs --no-log-prefix asterisk 2>&1 | grep -iE "dialler|100|error|warn|rtp" | tail -8 | sed 's/^/   asterisk: /'
  fi
fi

if [ "$DIRECTION" = in ] || [ "$DIRECTION" = both ]; then
  echo "== in: desk phone 100 dials 201 (app via Asterisk → trunk)"
  rm -f "$MEDIA_DIR/out-201.wav"
  ctl baresip-c '{"command":"dial","params":"201@asterisk"}'
  sleep "$CALL_SECONDS"
  ctl baresip-c '{"command":"hangup"}'
  sleep 2
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'invite|callee answered|bridged|call ended|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'
  if python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-201.wav"; then
    echo "PASS in: the app heard the desk phone through the trunk"
  else
    echo "FAIL in"; fail=1
    $COMPOSE logs --no-log-prefix asterisk 2>&1 | grep -iE "dialler|201|error|warn" | tail -8 | sed 's/^/   asterisk: /'
  fi
fi

exit $fail
