#!/bin/sh
# Ring the iOS app running in the simulator (or on a device on this Mac's
# network) through the real server: phone-b (202) dials 201, whose SIP user
# is NOT registered by any baresip, so the server wakes device `dev-a` — the
# app, connected to 127.0.0.1:7443 as dev-a — and CallKit rings.
#
#   make harness-up            # once; prints the dev-a token for the app's Settings
#   (app: host 127.0.0.1, port 7443, device dev-a, token, Save & connect)
#   make harness-ring-sim      # rings the app; hangs up after CALL_SECONDS
set -eu
cd "$(dirname "$0")/.."
C="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
CALL_SECONDS="${CALL_SECONDS:-20}"

$C up -d dialler baresip-b >/dev/null 2>&1
sleep 3
ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-b 4444 >/dev/null"
}
echo "== phone-b (202) dials 201 → server wakes dev-a (the app)"
ctl '{"command":"dial","params":"201@dialler"}'
sleep 1
$C logs --no-log-prefix --since 5s dialler 2>&1 | grep -E 'invite|woke|480|404' | sed 's/^/   /' || true
echo "   ringing for ${CALL_SECONDS}s — answer or decline in the app"
sleep "$CALL_SECONDS"
ctl '{"command":"hangup"}'
echo "== server"
$C logs --no-log-prefix --since 60s dialler 2>&1 | grep -E 'invite|woke|wake_ack|register|bridged|ended|480' | sed 's/^/   /' || true
