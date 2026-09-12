#!/bin/sh
# Shape the shared namespace's eth0 egress: random loss plus delay with
# jitter (NETEM_LOSS / NETEM_DELAY / NETEM_JITTER). Applied to the server's
# namespace this impairs everything the server sends — the packets a phone's
# decoder must conceal — while the phones' own sending stays clean.
set -eu
LOSS="${NETEM_LOSS:-2%}"; DELAY="${NETEM_DELAY:-30ms}"; JITTER="${NETEM_JITTER:-10ms}"
tc qdisc replace dev eth0 root netem loss "$LOSS" delay "$DELAY" "$JITTER"
echo "netem applied: loss $LOSS delay $DELAY jitter $JITTER"
tc qdisc show dev eth0
# Stay up so `docker compose` keeps the qdisc's owner around; the qdisc
# itself lives in the shared namespace until the service goes down.
while :; do sleep 3600; done
