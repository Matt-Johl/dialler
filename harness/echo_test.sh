#!/bin/sh
# Echo self-test, headless: app 201 (baresip-a) dials the reserved "echo"
# destination and records what comes back — its own tone, relayed by the
# server on the app leg alone (no PBX). With TRUNK=1 it dials 600, the
# PBX's Echo() through the trunk leg.
#
#   make harness-echo
#   TRUNK=1 make harness-echo
set -eu
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
MEDIA_DIR=harness/baresip/media
CALL_SECONDS="${CALL_SECONDS:-8}"
KEEP="${KEEP:-0}"
TARGET="echo@dialler"; WHERE="the server"
[ "${TRUNK:-0}" = 1 ] && { TARGET="600@dialler"; WHERE="the PBX through the trunk"; }

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

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

echo "== app 201 dials $TARGET (echo by $WHERE)"
ctl "{\"command\":\"dial\",\"params\":\"$TARGET\"}"
sleep "$CALL_SECONDS"
ctl '{"command":"hangup"}'
sleep 2
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'echo|invite|bridged|call ended|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'

echo "== media: what the app heard back"
if python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-201.wav"; then
  echo "PASS: the app heard its own audio back from $WHERE"
else
  echo "FAIL: no echo from $WHERE"; exit 1
fi
