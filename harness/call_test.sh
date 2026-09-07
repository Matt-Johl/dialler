#!/bin/sh
# Headless app↔app call (SPEC §7.2): PHONE_A (201) dials 202, the server
# bridges the legs with media proxied through itself, and the callee's
# recording is asserted to contain audio. Exit 0 = pass.
#
# Driven entirely inside the compose network (a throwaway alpine container
# talks to baresip's ctrl_tcp), so it works from sandboxes that cannot
# connect to published localhost ports.
#
# Defaults target the main harness (the real dialler-server). The engine
# spike uses the same script with its own compose file and service names.
set -eu
cd "$(dirname "$0")/.."

COMPOSE_FILE="${COMPOSE_FILE:-harness/docker-compose.yml}"
PROJECT="${PROJECT:-dialler-harness}"
SERVER="${SERVER:-dialler}"
PHONE_A="${PHONE_A:-baresip-a}"
PHONE_B="${PHONE_B:-baresip-b}"
DOMAIN="${DOMAIN:-dialler}"
MEDIA_DIR="${MEDIA_DIR:-harness/baresip/media}"
PROFILE="${PROFILE:---profile test}"
PROVISION="${PROVISION:-1}"
CALL_SECONDS="${CALL_SECONDS:-8}"
KEEP="${KEEP:-0}"

COMPOSE="docker compose -f $COMPOSE_FILE $PROFILE"
NET="${PROJECT}_default"

python3 harness/baresip/media/gen_tone.py "$MEDIA_DIR/in.wav" >/dev/null
rm -f "$MEDIA_DIR"/out-*.wav

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up ($COMPOSE_FILE)"
$COMPOSE up --build -d "$SERVER" >/dev/null 2>&1
if [ "$PROVISION" = 1 ]; then
  sleep 2
  sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
fi
$COMPOSE up --build -d "$PHONE_A" "$PHONE_B" >/dev/null 2>&1
sleep 5

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 $PHONE_A 4444 >/dev/null"
}

echo "== dial 201 -> 202@$DOMAIN"
ctl "{\"command\":\"dial\",\"params\":\"202@$DOMAIN\"}"
sleep "$CALL_SECONDS"
ctl '{"command":"hangup"}'
sleep 2

echo "== server"
$COMPOSE logs --no-log-prefix "$SERVER" 2>&1 | grep -E 'invite|bridged|call ended|sip register|level=(ERROR|WARN)' | tail -12 | sed 's/^/   /'
if ! $COMPOSE logs --no-log-prefix "$SERVER" 2>&1 | grep -q 'msg=bridged'; then
  echo "FAIL: server never bridged the call"; exit 1
fi

echo "== callee"
$COMPOSE logs --no-log-prefix "$PHONE_B" 2>&1 | grep -iE 'Call established|incoming rtp' | sed 's/^/   /'

echo "== media"
python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-202.wav"
