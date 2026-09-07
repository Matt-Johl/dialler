#!/bin/sh
# The app's call path, end to end, with no phone and no human: the real
# engine + audiounit driver run on the iOS simulator (a process spawned
# inside the simulator is not subject to the sandbox that blocks this shell
# from reaching localhost), register through the docker server, phone-b
# dials in and plays a tone, and the result is asserted on what the
# simulated phone received: RTP packets and the energy of the rendered
# audio. Loss/jitter/jitter-buffer counters make blips visible too.
#
#   make harness-up                      # once (advertises this Mac's en0 address)
#   make sim-call                        # PASS / PASS with GAPS / FAIL: <why>
#   ACTIVATE_MS=1500 make sim-call       # CallKit activation after establishment
#   SIM="iPhone 16 Pro" make sim-call
set -eu
cd "$(dirname "$0")/.."
SIM="${SIM:-iPhone 16}"
# The address the server advertises for SIP and media. It must be reachable
# both from the simulated phone (this Mac) and from the caller inside the
# docker network, so it is this Mac's LAN address, not localhost: with
# 127.0.0.1 the caller's media would be sent to the container's own loopback.
HOST="${HOST:-$(ipconfig getifaddr en0 2>/dev/null || ifconfig en0 2>/dev/null | awk '/inet /{print $2; exit}')}"
[ -n "$HOST" ] || { echo "FAIL: cannot determine this Mac's LAN address; set HOST=..."; exit 1; }
ACTIVATE_MS="${ACTIVATE_MS:-150}"
# Ringing time before the scripted user answers. A person takes seconds,
# and the caller streams into the server the whole time (ANSWER_MS=8000
# reproduces the device's long ring).
ANSWER_MS="${ANSWER_MS:-800}"
# Calls taken by ONE simulated phone process (the app takes many per
# launch; CALLS=2 reproduces "first call fine, second silent").
CALLS="${CALLS:-1}"
WAIT="${WAIT:-$((30 * CALLS + 15))}"
# A simctl-spawned process has no microphone, so by default the simulated
# phone transmits a tone file instead — a real phone transmits, and that
# is what makes the server latch its symmetric RTP onto the phone's source
# address. SIM_SOURCE=mic uses the (absent) microphone, i.e. no transmit.
TONE="$(pwd)/harness/baresip/media/in.wav"
case "${SIM_SOURCE:-tone}" in
  mic)  SOURCE="" ;;
  tone) SOURCE="aufile,$TONE" ;;
  *)    SOURCE="$SIM_SOURCE" ;;
esac
SCRATCH="${TMPDIR:-/tmp}/spm-build-ios"
IOS_SDK="$(xcrun --sdk iphonesimulator --show-sdk-path)"
OUT="${TMPDIR:-/tmp}/sim-call.log"
C="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
# The server must advertise an address the simulated phone can reach for SIP
# and media; localhost works because Docker publishes those ports there.
# Exported so every compose invocation below sees the same config — a
# differing value makes `compose up` recreate the server mid-test.
export DIALLER_PUBLIC_HOST="$HOST"

echo "== build sim-call for the simulator"
( cd ios/DiallerEngine && swift build --disable-sandbox --scratch-path "$SCRATCH" \
    --triple arm64-apple-ios17.0-simulator --sdk "$IOS_SDK" --product sim-call 2>&1 \
  | grep -E 'error|Build of' || true )
BIN="$SCRATCH/arm64-apple-ios-simulator/debug/sim-call"
[ -x "$BIN" ] || { echo "FAIL: $BIN not built"; exit 1; }

# SERVER=native: the server runs on this Mac (`make dev-server`); only the
# docker caller is started, registered to it over outbound NAT.
SERVER="${SERVER:-docker}"
if [ "$SERVER" = native ]; then
  echo "== native server at $HOST (make dev-server); starting the docker caller only"
  BARESIP_B_OUTBOUND="$HOST:5061" $C up -d --no-deps --force-recreate baresip-b >/dev/null 2>&1
elif docker ps --format '{{.Names}}' | grep -q '^dialler-harness-dialler-1$'; then
  # Leave a running server alone (its flags, e.g. -log-level, were chosen
  # by whoever started it); a plain `up` would recreate it with defaults.
  echo "== server already running (leaving it as is)"
  # phone-b is (re)created once, below, when it is the callee.
  [ -n "${OUTBOUND:-}" ] || $C up -d --no-deps baresip-b >/dev/null 2>&1
else
  echo "== server advertising $HOST"
  $C up -d dialler baresip-b >/dev/null 2>&1
fi
sleep 2

if [ -n "${OUTBOUND:-}" ] && [ "$OUTBOUND" != echo ] && [ "$OUTBOUND" != 600 ]; then
  # The simulated phone dials phone-b the moment it registers, so phone-b
  # must already hold a live registration with THIS server instance:
  # recreate it and wait for that registration BEFORE the phone starts.
  # (echo / 600 need no callee: the server or the PBX answers.)
  echo "== phone-b (202) must be registered (callee)"
  MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if [ "$SERVER" = native ]; then
    BARESIP_B_OUTBOUND="$HOST:5061" $C up -d --no-deps --force-recreate baresip-b >/dev/null 2>&1
    sleep 4
  else
    $C up -d --no-deps --force-recreate baresip-b >/dev/null 2>&1
    i=0
    until $C logs --no-log-prefix --since "$MARK" dialler 2>/dev/null | grep -q 'sip register" user=202'; do
      i=$((i+1)); [ $i -le 25 ] || { echo "FAIL: phone-b did not register within 25s"; exit 1; }
      sleep 1
    done
  fi
  echo "   phone-b registered"
fi

echo "== simulator $SIM"
xcrun simctl boot "$SIM" 2>/dev/null || true
: > "$OUT"
# GATEWAY=no: no wire-protocol session, so no wake; calls ring from the INVITE.
# OUTBOUND=202 (or any target): the simulated phone dials phone-b, which
# auto-answers and plays its tone — the keypad / directory path.
MODE=""; [ "${GATEWAY:-yes}" = no ] && MODE=nogateway
[ -n "${OUTBOUND:-}" ] && MODE="outbound:$OUTBOUND"
SIGNAL_PORT="${SIGNAL_PORT:-${DIALLER_SIGNAL_HOSTPORT:-7443}}"
# HOLD_MS=3000: hold the call for 3 s after the first verdict, then resume;
# asserts media stops while held and flows again after.
# TRANSFER=echo: after the first verdict the simulated phone REFERs the
# caller (phone-b) to "echo"; our call must end as transferred, and phone-b's
# recording must then contain audio it did not get from us (its own echo).
if [ -n "${TRANSFER:-}" ]; then SOURCE=""; fi   # silent phone, so any audio phone-b records is the echo
xcrun simctl spawn "$SIM" "$BIN" "$HOST" "$SIGNAL_PORT" dev-a tok_dev_a_harness_fixed "$WAIT" "$ACTIVATE_MS" "$SOURCE" "$ANSWER_MS" "$CALLS" "$MODE" "${HOLD_MS:-0}" "${TRANSFER:-}" > "$OUT" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT

i=0
until grep -q 'sim: registered' "$OUT"; do
  i=$((i+1)); [ $i -le 30 ] || { echo "FAIL: simulated phone did not register within 30s"; grep -E 'sim:|engine:' "$OUT" | sed 's/^/   /'; exit 1; }
  grep -q 'sim: FAIL\|sim: gateway error' "$OUT" && { grep -E 'sim:|engine:' "$OUT" | sed 's/^/   /'; exit 1; }
  sleep 1
done
echo "   registered as 201 (dev-a)"

[ -n "${OUTBOUND:-}" ] || echo "== phone-b (202) dials 201"
ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-b 4444 >/dev/null"
}
n=1
while :; do
  if [ -n "${OUTBOUND:-}" ]; then
    echo "   (outbound: the simulated phone dials $OUTBOUND)"
  else
    ctl '{"command":"dial","params":"201@dialler"}'
  fi
  i=0
  until grep -q "sim: call $n: \|sim: FAIL" "$OUT"; do
    i=$((i+1)); [ $i -le 45 ] || break
    sleep 1
  done
  ctl '{"command":"hangup"}' 2>/dev/null || true
  [ "$n" -lt "$CALLS" ] || break
  n=$((n+1))
  # Wait for the simulated phone to report the previous call ended.
  i=0
  until grep -q "sim: ready for call $n" "$OUT"; do
    i=$((i+1)); [ $i -le 20 ] || break
    sleep 1
  done
  echo "== phone-b dials 201 again (call $n)"
  sleep 2
done
i=0
until grep -q 'sim: PASS\|sim: FAIL' "$OUT"; do
  i=$((i+1)); [ $i -le 15 ] || break
  sleep 1
done
wait $PID 2>/dev/null || true
trap - EXIT

echo "== simulated phone"
grep -E 'sim:|engine:|callkit|audiounit:|stream:|INVITE|answered|incoming|wake|call .* ended|registering' "$OUT" | grep -v '^  baresip:' | sed 's/^/   /'
if [ -n "${TRANSFER:-}" ]; then
  echo "== transfer: did phone-b hear the echo after being transferred?"
  sleep 3
  python3 harness/spike/assert_audio.py "$(pwd)/harness/baresip/media/out-202.wav" || { echo "FAIL: phone-b heard nothing after the transfer"; exit 1; }
fi
if [ "$SERVER" = native ]; then
  echo "== server: native; its relay lines are in the dev-server terminal"
else
  echo "== server"
  $C logs --no-log-prefix --since 90s dialler 2>&1 | grep -E 'invite|woke|wake_ack|register|bridged|ended|480|rtp|media|relay|RELAY' | tail -20 | sed 's/^/   /' || true
fi

grep -q 'sim: PASS' "$OUT"
