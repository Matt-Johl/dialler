#!/bin/sh
# QoS gate (SPEC §4.4): the server marks media DSCP EF and signalling CS3.
# A tcpdump sidecar in the server's network namespace watches while app 211
# registers and makes an echo call; every RTP packet the server sends must
# carry tos 0xb8 and its SIP/TLS packets tos 0x60.
#
#   make harness-qos
set -eu
cd "$(dirname "$0")/.."
COMPOSE="docker compose -f harness/docker-compose.yml --profile test --profile qos"
NET=dialler-harness_default
SERVER_IP=172.30.0.10
KEEP="${KEEP:-0}"

python3 harness/baresip/media/gen_tone.py harness/baresip/media/in.wav >/dev/null
cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up: server and app 211, then the capture sidecar"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a >/dev/null 2>&1
sleep 5
# Last, and without deps: a later `up` that recreated the server would
# strand the sidecar in a dead namespace ("The interface disappeared").
$COMPOSE up --build -d --no-deps qos >/dev/null 2>&1
sleep 2

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}
echo "== app 211 dials echo (server sends RTP back)"
ctl '{"command":"dial","params":"echo@dialler"}'
sleep 4
ctl '{"command":"hangup"}'
sleep 2

# tcpdump -v prints the header ("IP (tos 0xb8, ttl …)") and the flow
# ("    172.30.0.10.20000 > 172.30.0.2.x: UDP, …") on two lines: join them.
cap="$($COMPOSE logs --no-log-prefix qos 2>&1 | awk '/IP \(tos/ { hdr = $0; next } hdr != "" { print hdr " " $0; hdr = "" }')"
rtp_total=$(printf '%s\n' "$cap" | grep -c "$SERVER_IP\.20[0-9][0-9][0-9] > .*UDP" || true)
rtp_ef=$(printf '%s\n' "$cap" | grep "$SERVER_IP\.20[0-9][0-9][0-9] > .*UDP" | grep -c "tos 0xb8" || true)
sip_total=$(printf '%s\n' "$cap" | grep -c "$SERVER_IP\.5061 > " || true)
sip_cs3=$(printf '%s\n' "$cap" | grep "$SERVER_IP\.5061 > " | grep -c "tos 0x60" || true)
echo "   server RTP packets: $rtp_total, marked EF: $rtp_ef"
echo "   server SIP/TLS packets: $sip_total, marked CS3: $sip_cs3"
fail=0
[ "$rtp_total" -gt 50 ] || { echo "FAIL: too few RTP packets captured ($rtp_total)"; fail=1; }
[ "$rtp_ef" = "$rtp_total" ] || { echo "FAIL: not every server RTP packet is EF"; fail=1; }
[ "$sip_total" -gt 0 ] && [ "$sip_cs3" = "$sip_total" ] || { echo "FAIL: server SIP/TLS packets not all CS3"; fail=1; }
if [ "$fail" = 0 ]; then echo "PASS: media EF and signalling CS3 on everything the server sent"; fi
exit $fail
