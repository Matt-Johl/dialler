#!/bin/sh
# NAT regression test, run on a Mac (not inside docker): the engine probe —
# the real BaresipCallEngine — registers 203 (dev-s) from this Mac through Docker's
# published ports, which is a genuine NAT (the server sees Docker's gateway
# as the source, not the probe's advertised Contact). Then the harness phone
# 212 dials 203: the server must reach the probe over the TLS connection it
# registered on and relay media with symmetric RTP. Exit 0 = pass.
#
#   make probe-call                                  # expect PASS
#   DIALLER_REWRITE_CONTACT=false make probe-call    # expect FAIL (the bug this guards)
#
# Not runnable from a sandbox that forbids sockets; it is for developer
# machines and macOS CI.
set -eu
cd "$(dirname "$0")/.."
COMPOSE="docker compose -f harness/docker-compose.yml --profile test"
NET=dialler-harness_default
HOST="${HOST:-$(ipconfig getifaddr en0 2>/dev/null || true)}"
[ -n "$HOST" ] || { echo "FAIL: cannot determine this Mac's address (set HOST=...)"; exit 1; }
export DIALLER_PUBLIC_HOST="$HOST"
KEEP="${KEEP:-0}"

cleanup() { kill "${PROBE_PID:-}" 2>/dev/null || true; [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== build engine-probe"
( cd ios/DiallerEngine && swift build --disable-sandbox --product engine-probe 2>&1 | grep -E 'error|complete' )
PROBE="$(cd ios/DiallerEngine && swift build --disable-sandbox --product engine-probe --show-bin-path)/engine-probe"

echo "== up (public host $HOST, rewrite-contact=${DIALLER_REWRITE_CONTACT:-true})"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up -d baresip-b >/dev/null 2>&1
sleep 3

echo "== probe registers 203 (dev-s) from this Mac"
OUT="$(mktemp)"
"$PROBE" "$HOST" 203@dialler 5061 40 dev-s tok_dev_s_harness_fixed > "$OUT" 2>&1 &
PROBE_PID=$!
i=0
while [ $i -lt 25 ]; do grep -q '\[state\] registered' "$OUT" && break; sleep 1; i=$((i+1)); done
if ! grep -q '\[state\] registered' "$OUT"; then
  echo "FAIL: probe did not register within 25s"
  echo "   --- probe output ---"; sed 's/^/   /' "$OUT" | tail -20
  echo "   --- server ---"; $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -iE 'register|tls|error' | tail -5 | cut -c1-200 | sed 's/^/   /'
  exit 1
fi
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep 'sip register' | grep 'user=203' | tail -n1 | cut -c1-220 | sed 's/^/   /'

echo "== phone 212 dials 203"
docker run --rm --network "$NET" alpine:3.20 sh -c \
  "p='{\"command\":\"dial\",\"params\":\"203@dialler\"}'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 baresip-b 4444 >/dev/null"

# Stream the probe's progress while it runs; hard cap so this never stalls.
shown=0; i=0
while kill -0 "$PROBE_PID" 2>/dev/null && [ $i -lt 60 ]; do
  n=$(wc -l < "$OUT" | tr -d ' ')
  if [ "$n" -gt "$shown" ]; then tail -n +$((shown+1)) "$OUT" | grep -E 'INVITE|answered|established|closed|probe:|baresip:.*(fail|error)' | sed 's/^/   /'; shown=$n; fi
  sleep 1; i=$((i+1))
done
if kill -0 "$PROBE_PID" 2>/dev/null; then echo "   probe still running after 60s; killing"; kill "$PROBE_PID"; fi
wait "$PROBE_PID" 2>/dev/null && rc=0 || rc=$?

echo "== probe"
grep -E 'INVITE|answered|established|closed|baresip:.*(fail|error)|probe:' "$OUT" | sed 's/^/   /' | tail -12
echo "== server"
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'invite|bridged|call ended|level=ERROR' | cut -c1-200 | tail -6 | sed 's/^/   /'

if grep -q 'CALL ESTABLISHED' "$OUT"; then
  echo "PASS: NAT'd engine reached over its own connection, media ran"
else
  echo "FAIL: call was not established to the NAT'd engine"; exit 1
fi