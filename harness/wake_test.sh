#!/bin/sh
# Headless wake test (SPEC §7.2, the LPC/APNS path without a device):
#   - phone-b (202) starts UNREGISTERED (REGINT=0);
#   - fake-app holds a wire-protocol connection to the gateway as dev-b;
#   - phone-a (201) dials 202 → server finds 202 unregistered → sends `wake`
#     to dev-b → fake-app acks and tells phone-b to REGISTER → the server's
#     WaitRegistered fires → the bridge completes → media flows.
# Asserts the wake arrived, the server logged the wake path, and the callee
# recorded audio. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
MEDIA_DIR=harness/baresip/media
CALL_SECONDS="${CALL_SECONDS:-8}"
# On wake the fake app creates phone-b's user agent, which registers at once
# (a UA started with regint=0 has no registration objects, so uareg is a no-op).
ACCOUNT_B='<sip:202@dialler;transport=tls>;auth_pass=unused;regint=300;answermode=auto;audio_codecs=opus/48000/2,PCMU/8000/1'
WAKE_CMD="${WAKE_CMD:-{\"command\":\"uanew\",\"params\":\"$ACCOUNT_B\"}}"
KEEP="${KEEP:-0}"

python3 harness/baresip/media/gen_tone.py "$MEDIA_DIR/in.wav" >/dev/null
rm -f "$MEDIA_DIR"/out-*.wav

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
PROV="$(sh harness/innet.sh "$NET" harness/provision.sh)"
TOKEN_B="$(printf '%s\n' "$PROV" | sed -n 's/.*"device_id":"dev-b".*"token":"\([^"]*\)".*/\1/p' | head -n1)"
[ -n "$TOKEN_B" ] || { echo "FAIL: could not read dev-b token from provisioning:"; echo "$PROV"; exit 1; }

BARESIP_B_REGINT=0 $COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 4
if $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'sip register.*user=202'; then
  echo "FAIL: phone-b registered at startup; the wake path would not be exercised"; exit 1
fi

echo "== fake-app connects as dev-b and waits for a wake"
WAKE_OUT="$(mktemp)"
$COMPOSE run --rm -T --entrypoint /fake-app dialler \
    -server dialler:7443 -device dev-b -token "$TOKEN_B" -kind extension \
    -phone-ctl baresip-b:4444 -wake-cmd "$WAKE_CMD" -timeout 40s -hold 20s \
    > "$WAKE_OUT" 2>"$WAKE_OUT.err" &
FAKE_PID=$!
sleep 3

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

echo "== dial 201 -> 202 (202 is asleep)"
ctl '{"command":"dial","params":"202@dialler"}'
sleep "$CALL_SECONDS"
ctl '{"command":"hangup"}'
sleep 2

wait $FAKE_PID && fake_rc=0 || fake_rc=$?
echo "== fake-app (exit $fake_rc)"
sed 's/^/   /' "$WAKE_OUT.err" | grep -E 'connected|wake|register|error' || true
if ! grep -q '"call_id"' "$WAKE_OUT"; then
  echo "FAIL: fake-app never received a wake"; exit 1
fi
echo "   wake: $(cat "$WAKE_OUT")"

echo "== server"
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'invite|woke|wake_ack|sip register.*202|bridged|call ended|level=(ERROR|WARN)' | tail -12 | sed 's/^/   /'
for want in 'woke callee device' 'wake_ack' 'sip register.*user=202' 'msg=bridged'; do
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -qE "$want" || { echo "FAIL: server log lacks: $want"; exit 1; }
done

echo "== media"
python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-202.wav"
