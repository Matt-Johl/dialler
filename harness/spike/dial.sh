#!/bin/sh
# Drive phone-a to call 202 via baresip's ctrl_tcp interface (netstring-framed
# JSON), hold the call briefly so tone flows, then hang up.
set -eu
HOST="${CTRL_HOST:-127.0.0.1}"
PORT="${CTRL_PORT:-4401}"
DURATION="${CALL_SECONDS:-6}"

# netstring: "<len>:<payload>,"
send() {
  payload="$1"
  len=$(printf %s "$payload" | wc -c | tr -d ' ')
  printf '%s:%s,' "$len" "$payload" | nc -w2 "$HOST" "$PORT" >/dev/null 2>&1 || \
    { echo "ctrl_tcp connect failed on $HOST:$PORT"; exit 1; }
}

echo "dial 201 -> 202"
send '{"command":"dial","params":"202@spike"}'
sleep "$DURATION"
echo "hangup"
send '{"command":"hangup"}'
sleep 1
echo "done"
