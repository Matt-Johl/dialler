#!/bin/sh
# A caller who hangs up while the callee is still ringing must not cost the
# callee its registration. The server CANCELs the callee's INVITE over the
# callee's own TLS connection — the one its REGISTER arrived on, which the
# server reuses for everything it sends that phone (SPEC §4.4 rule 3). If
# the cancel path ever closed that connection, the phone would look
# unregistered ("registration flow gone") and every later call to it would
# take the wake path instead of ringing it directly — which is what the
# phone's log suggested on 2026-09-17 ("Connection reset by peer [54]" on
# its SIP socket in the same millisecond as the server's cancel).
#
# 212 is started with answermode=manual, so it rings and never answers.
# 211 dials it, hangs up 3 s later, then dials again: the second call must
# reach 212 through the same registration, with no wake and no re-REGISTER.
# Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."
# HARNESS_NOPORTS=1 publishes nothing on the host, so this can run beside a
# native `make dev-server` without fighting it for 7443 / 5061. Safe for any
# test that lives entirely inside the compose network; NOT for ones a
# simulator or a real phone has to reach (sim_call.sh, ring_*.sh, probe_*).
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test"
NET=dialler-harness_default
KEEP="${KEEP:-0}"
RING_SECONDS="${RING_SECONDS:-3}"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

logs() { $COMPOSE logs --no-log-prefix dialler 2>&1; }
ctl() {
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$1'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-a 4444 >/dev/null"
}

echo "== up: server, 211 (caller), 212 (rings, never answers)"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
BARESIP_B_ANSWER_MODE=manual $COMPOSE up --build -d baresip-a baresip-b >/dev/null 2>&1
sleep 5
logs | grep -q 'sip register.*user=212' || { echo "FAIL: 212 never registered"; exit 1; }
[ "$(logs | grep -c 'sip register.*user=212')" = 1 ] || { echo "FAIL: 212 registered more than once before the test began"; exit 1; }

echo "== call 1: 211 dials 212, hangs up after ${RING_SECONDS}s of ringing"
ctl '{"command":"dial","params":"212@dialler"}'
sleep "$RING_SECONDS"
ctl '{"command":"hangup"}'
sleep 3
logs | grep -E 'invite|callee|call ended|flow gone|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'
if logs | grep -q 'msg=bridged'; then
  echo "FAIL: 212 answered; the test needs a callee that only rings (answermode=manual did not take)"; exit 1
fi
logs | grep -q 'invite callee.*context canceled' || { echo "FAIL: the first call was not cancelled while ringing"; exit 1; }
echo "   call 1 cancelled while 212 was ringing"

echo "== call 2: 211 dials 212 again — must ring it through the same registration"
MARK="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ctl '{"command":"dial","params":"212@dialler"}'
sleep "$RING_SECONDS"
ctl '{"command":"hangup"}'
sleep 3
after="$($COMPOSE logs --no-log-prefix --since "$MARK" dialler 2>&1)"
echo "$after" | grep -E 'invite|callee|call ended|flow gone|woke|level=(ERROR|WARN)' | tail -6 | sed 's/^/   /'
fail=0
if echo "$after" | grep -q 'registration flow gone'; then
  echo "FAIL: the server treated 212 as unregistered after the cancel — the cancel closed its connection"; fail=1
fi
if echo "$after" | grep -q 'woke callee device'; then
  echo "FAIL: the second call took the wake path"; fail=1
fi
echo "$after" | grep -q 'invite callee.*context canceled' || { echo "FAIL: the second call did not ring 212 (no INVITE cancelled while ringing)"; fail=1; }
# The stronger check, from the callee's side: 212 saw two incoming calls
# on the one registration, and never had to register again.
incoming="$($COMPOSE logs --no-log-prefix baresip-b 2>&1 | grep -ci 'incoming' || true)"
registers="$(logs | grep -c 'sip register.*user=212' || true)"
echo "   212: incoming calls seen=$incoming, registrations=$registers"
[ "$incoming" -ge 2 ] || { echo "FAIL: 212 did not see a second incoming call"; fail=1; }
[ "$registers" = 1 ] || { echo "FAIL: 212 had to register again ($registers registrations): its connection was lost"; fail=1; }
if [ "$fail" = 0 ]; then
  echo "PASS: a cancel while ringing left 212's registration and connection intact; the next call rang it directly"
fi
exit $fail
