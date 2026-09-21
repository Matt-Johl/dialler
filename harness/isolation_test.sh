#!/bin/sh
# The admin isolation rule (SPEC §4.8, §6 item 9): nothing the operator
# does interrupts the call server or any device other than the one named.
#
# With a call bridged between the two docker phones (dev-ha → dev-hb) and
# two bystander gateway sessions held (dev-ha's own, and dev-s), the admin
# revokes dev-s, replaces dev-a's whole directory, sets dev-a's Wi-Fi list
# and mints dev-a a code — all while the call is up. Then:
#   - the call's audio must have flowed for its whole length;
#   - dev-ha's session must have seen no error, no directory_changed and
#     no config frame;
#   - dev-s's session must have been closed with unauthorized (the one
#     scoped effect), and nothing else.
#
#   make harness-isolation
set -eu
cd "$(dirname "$0")/.."
COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
MEDIA_DIR=harness/baresip/media
CALL_SECONDS="${CALL_SECONDS:-10}"
KEEP="${KEEP:-0}"

python3 harness/baresip/media/gen_tone.py "$MEDIA_DIR/in.wav" >/dev/null
rm -f "$MEDIA_DIR"/out-*.wav
OUT="$(mktemp -d)"
cleanup() {
  docker kill iso-ha iso-s >/dev/null 2>&1 || true
  [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== up"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 5

IMG="$(docker inspect --format '{{.Image}}' dialler-harness-dialler-1)"
echo "== bystander sessions: dev-ha (the caller's device) and dev-s"
docker run --rm --name iso-ha --network "$NET" --entrypoint /fake-app "$IMG" \
    -server dialler:7443 -device dev-ha -token tok_dev_ha_harness_fixed -kind app -timeout 60s -hold 1s \
    2>"$OUT/ha.err" >/dev/null &
docker run --rm --name iso-s --network "$NET" --entrypoint /fake-app "$IMG" \
    -server dialler:7443 -device dev-s -token tok_dev_s_harness_fixed -kind app -timeout 60s -hold 1s \
    2>"$OUT/s.err" >/dev/null &
sleep 3
grep -q connected "$OUT/ha.err" && grep -q connected "$OUT/s.err" || { echo "FAIL: bystanders did not connect"; cat "$OUT/ha.err" "$OUT/s.err"; exit 1; }

ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}
# The admin's verbs need DELETE and PUT, which busybox wget lacks; a
# throwaway container with curl does them.
admin() {
  # The arguments travel as positional parameters, so JSON bodies survive.
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    'apk add --no-cache curl >/dev/null 2>&1 && curl -sk -o /dev/null -w "%{http_code}" -H "Authorization: Bearer harness" -H "Content-Type: application/json" "$@"' _ "$@"
}

echo "== dial 211 -> 212 and let it bridge"
ctl '{"command":"dial","params":"212@dialler"}'
sleep 3
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'msg=bridged' || { echo "FAIL: the call did not bridge"; exit 1; }

echo "== while the call is up: revoke dev-s, replace dev-a's directory, set dev-a's Wi-Fi, mint dev-a a code"
r1="$(admin -X DELETE https://dialler:8080/v1/admin/devices/dev-s)"
r2="$(admin -X PUT -d '{"contacts":[{"display_name":"Only one","uri":"sip:999@dialler","mode":"local"}]}' https://dialler:8080/v1/admin/devices/dev-a/directory)"
r3="$(admin -X PUT -d '{"ssids":["Office"]}' https://dialler:8080/v1/admin/devices/dev-a/config)"
r4="$(admin -X POST https://dialler:8080/v1/admin/devices/dev-a/enrol-code)"
echo "   revoke $r1, replace $r2, config $r3, code $r4"
[ "$r1$r2$r3$r4" = "204200200200" ] || { echo "FAIL: an admin action did not succeed"; exit 1; }

sleep "$((CALL_SECONDS - 3))"
ctl '{"command":"hangup"}'
sleep 2
docker kill iso-ha iso-s >/dev/null 2>&1 || true
wait 2>/dev/null || true

echo "== the call"
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'msg=bridged|call ended|level=ERROR' | tail -4 | sed 's/^/   /'
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'call ended' || { echo "FAIL: the call did not end normally"; exit 1; }
echo "== the audio flowed for the whole call"
python3 harness/spike/assert_audio.py "$MEDIA_DIR/out-212.wav" || { echo "FAIL: the callee's audio was disturbed"; exit 1; }

echo "== dev-ha's session saw nothing"
if grep -qE 'gateway error|directory_changed received|config received' "$OUT/ha.err"; then
  echo "FAIL: dev-ha's session was touched:"; grep -E 'gateway error|received' "$OUT/ha.err" | sed 's/^/   /'; exit 1
fi
echo "   ok: no error, no directory_changed, no config"
echo "== dev-s's session was closed with unauthorized, and only that"
grep -q 'gateway error.*unauthorized' "$OUT/s.err" || { echo "FAIL: dev-s was not disconnected on revoke:"; cat "$OUT/s.err"; exit 1; }
echo "   ok"
echo "PASS: admin actions during a call touched only the devices they named"
