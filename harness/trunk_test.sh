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
#   transfer: desk phone 100 dials 201, then the app transfers it to 600
#         (Asterisk's Echo()). Both parties are on the PBX, so the server
#         must hand the transfer to Asterisk (REFER on the trunk leg) and
#         drop out: its call ends at once, the app sees the transfer
#         complete, and the desk phone — with the app silent — records its
#         own tone coming back from the PBX.
#
# Both legs run G.711 (the trunk is offered PCMU/PCMA only and the app is
# answered with the codec the trunk took), so the raw relay never transcodes.
#
#   make harness-trunk              # all three
#   DIRECTION=out|in|transfer make harness-trunk
set -eu
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
MEDIA_DIR=harness/baresip/media
DIRECTION="${DIRECTION:-both}"
CALL_SECONDS="${CALL_SECONDS:-8}"
KEEP="${KEEP:-0}"

python3 harness/baresip/media/gen_tone.py "$MEDIA_DIR/in.wav" >/dev/null
# A silent source for the transfer scenario (same format as the tone).
python3 - "$MEDIA_DIR/silence.wav" <<'EOF'
import sys, wave
with wave.open(sys.argv[1], "wb") as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(48000)
    w.writeframes(b"\0" * (48000 * 2 * 2 * 20))
EOF
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

if [ "$DIRECTION" = transfer ] || [ "$DIRECTION" = both ]; then
  echo "== transfer: desk phone 100 dials 201; the app transfers it to 600 (PBX echo) — the server must drop out"
  # The app is silent for this one, so any audio the desk phone records is
  # its own tone coming back from Asterisk's Echo(): proof the PBX completed
  # the transfer and carries the audio itself.
  AUDIO_SOURCE="aufile,/media/silence.wav" $COMPOSE up -d --no-deps --force-recreate baresip-a >/dev/null 2>&1
  sleep 5
  rm -f "$MEDIA_DIR/out-100.wav"
  MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  ctl baresip-c '{"command":"dial","params":"201@asterisk"}'
  sleep 4
  ctl baresip-a '{"command":"transfer","params":"600"}'
  sleep 3
  # While the desk phone is still on the (transferred) call, the server's
  # part of it must already be over.
  early="$($COMPOSE logs --no-log-prefix --since "$MARK" dialler 2>&1)"
  sleep 5
  ctl baresip-c '{"command":"hangup"}'
  sleep 2
  echo "$early" | grep -E 'transfer|call ended|level=(ERROR|WARN)' | tail -8 | sed 's/^/   /'
  ok=1
  echo "$early" | grep -q 'transfer: offloaded to PBX' || { echo "   FAIL: the server did not hand the transfer to the PBX"; ok=0; }
  echo "$early" | grep -q 'call ended' || { echo "   FAIL: the server was still on the call 3 s after the transfer"; ok=0; }
  $COMPOSE logs --no-log-prefix --since "$MARK" baresip-a 2>&1 | grep -qi 'transfer' || { echo "   FAIL: the app did not see the transfer complete (no NOTIFY 200)"; ok=0; }
  if python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-100.wav"; then
    [ "$ok" = 1 ] && echo "PASS transfer: the PBX completed it; the desk phone heard the echo with the server out of the path"
  else
    echo "   FAIL: the desk phone heard nothing after the transfer"; ok=0
  fi
  if [ "$ok" != 1 ]; then
    echo "FAIL transfer"; fail=1
    $COMPOSE logs --no-log-prefix --since "$MARK" asterisk 2>&1 | grep -iE "refer|transfer|600|error|warn" | tail -10 | sed 's/^/   asterisk: /'
  fi
fi

exit $fail
