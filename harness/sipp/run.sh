#!/bin/sh
# Runs the app-leg conformance scenarios against the dialler server.
# Exit code is non-zero if any scenario fails. All SIPp output is captured
# and shown on failure so connection/TLS problems are visible, not just
# scenario mismatches.
set -u
HOST="${DIALLER_HOST:-dialler}"
PORT="${DIALLER_SIP_PORT:-5061}"
USER="${SIP_USER:-211}"

echo "== environment"
sipp -v 2>&1 | grep -m1 -i 'sipp v' | sed 's/^/   /'
echo "   TLS probe of $HOST:$PORT:"
echo | timeout 5 openssl s_client -connect "$HOST:$PORT" -brief 2>&1 | sed 's/^/     /' | head -n 6

run() {
  name="$1"; shift
  echo "== $name"
  sipp "$HOST:$PORT" -t l1 -sf "/scenarios/$name.xml" -s "$USER" -m 1 -nostdin \
       -timeout 10s -timeout_error -trace_err -error_file "/tmp/$name.err" "$@" \
       > "/tmp/$name.out" 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "   ok"
    return 0
  fi
  echo "   FAIL (sipp exit $rc)"
  echo "   --- sipp output (tail) ---"
  tail -n 15 "/tmp/$name.out" 2>/dev/null | sed 's/^/   /'
  if [ -s "/tmp/$name.err" ]; then
    echo "   --- error file ---"
    sed 's/^/   /' "/tmp/$name.err"
  fi
  return 1
}

status=0
tls_ok=1
run register_tls     || { status=1; tls_ok=0; }
run invite_offline_480 || status=1
run invite_unknown_404 || status=1
run unregister_tls   || status=1
# App-leg authentication (SPEC §4.4 rule 2): a credential for another user
# and no credential at all are both refused after the Digest challenge.
run register_wrong_device_403 || status=1
run register_unenrolled_403   || status=1

# Plain TCP must be refused outright: the app leg is TLS-only (SPEC §4.4).
# Only meaningful if TLS itself works, otherwise "refused" proves nothing.
echo "== plain_tcp_refused"
if [ "$tls_ok" -eq 0 ]; then
  echo "   inconclusive (TLS path failed above)"
elif sipp "$HOST:$PORT" -t t1 -sf /scenarios/register_tls.xml -s "$USER" -m 1 -nostdin \
        -timeout 5s -timeout_error > /tmp/plain_tcp.out 2>&1; then
  echo "   FAIL: plain TCP REGISTER succeeded"; status=1
else
  echo "   ok"
fi
exit $status
