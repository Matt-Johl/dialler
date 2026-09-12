#!/bin/sh
# Ring a real phone through a server running NATIVELY on this Mac (`make
# dev-server`), with the harness's phone-b (212, in docker) as the caller.
#
# Why: Docker Desktop's UDP port proxy does not carry a two-way RTP flow
# between a LAN phone and a container — the phone gets ~1 s of audio and
# then nothing until the flow closes (`make sim-call` cannot see this: the
# simulator is on the Mac, where the proxy's return path works). A native
# server binds the media ports on the Mac's own interface; the docker caller
# reaches it through ordinary outbound NAT, which is fine.
#
#   make dev-server                 # terminal 1: native server + provisioning
#   make harness-ring-native        # terminal 2: 212 dials 201 (the phone)
set -eu
cd "$(dirname "$0")/.."
HOST="${HOST:-$(ipconfig getifaddr en0 2>/dev/null || ifconfig en0 2>/dev/null | awk '/inet /{print $2; exit}')}"
[ -n "$HOST" ] || { echo "FAIL: cannot determine this Mac's LAN address; set HOST=..."; exit 1; }
C="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
CALL_SECONDS="${CALL_SECONDS:-20}"

export BARESIP_B_OUTBOUND="$HOST:5061"
# --no-deps: do not start (or recreate) the dialler container; the server is native.
$C up -d --no-deps --force-recreate baresip-b >/dev/null 2>&1
sleep 3
ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-b 4444 >/dev/null"
}
echo "== phone-b (212, docker) → native server at $HOST → dials 201 (the phone)"
ctl '{"command":"dial","params":"201@dialler"}'
echo "   ringing for ${CALL_SECONDS}s — answer on the phone; the native server's log shows the relay counters"
sleep "$CALL_SECONDS"
ctl '{"command":"hangup"}'
