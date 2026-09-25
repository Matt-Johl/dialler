#!/bin/sh
# The admin gate's latency probe (ADMIN-API.md §4.7; harness/admin_test.sh):
# as dev-hc (213), REGISTER to the server and INVITE a user with no phone
# (214, answered 480 by the wake path), COUNT times, INTERVAL seconds
# apart, and print one line per round: "<register ms> <invite ms>". Both
# are SIPp's rtd 1: the authenticated request to its final response. A
# scenario that fails prints "FAIL <name>" and the exit code is 1.
#
#   docker compose … run --rm --entrypoint /work/probe.sh sipp COUNT INTERVAL
set -u
HOST="${DIALLER_HOST:-dialler}"
PORT="${DIALLER_SIP_PORT:-5061}"
COUNT="${1:-6}"
INTERVAL="${2:-1}"
cd /work
status=0
i=0
while [ "$i" -lt "$COUNT" ]; do
  line=""
  for s in probe_register probe_invite; do
    rm -f "${s}"_*_rtt.csv
    if sipp "$HOST:$PORT" -t l1 -sf "/scenarios/$s.xml" -s 213 -m 1 -nostdin \
         -timeout 10s -timeout_error -trace_rtt -rtt_freq 1 >/dev/null 2>&1; then
      ms="$(tail -n1 "${s}"_*_rtt.csv 2>/dev/null | cut -d';' -f2)"
      line="$line ${ms:-?}"
    else
      echo "FAIL $s"; status=1; line="$line ?"
    fi
  done
  echo "$line"
  i=$((i+1))
  [ "$i" -lt "$COUNT" ] && sleep "$INTERVAL"
done
exit $status
