#!/bin/sh
# Screenshot dialler-admin's pages, light and dark, desktop and phone, so a
# change to the UI can be looked at without a browser at hand. Brings the
# server and the UI up in compose, provisions, runs harness/screenshot.py
# in the Playwright image on the compose network, and leaves the PNGs in
# $OUT (default harness/shots/, git-ignored).
#
#   sh harness/screenshot.sh            # then open harness/shots/
#   KEEP=1 sh harness/screenshot.sh     # leave the stack up
set -eu
cd "$(dirname "$0")/.."
OUT="${OUT:-$(pwd)/harness/shots}"
KEEP="${KEEP:-0}"
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test"
NET="dialler-harness_default"
IMG="${PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright/python:v1.47.0-jammy}"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

mkdir -p "$OUT"
rm -f "$OUT"/*.png
echo "== up"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up -d admin >/dev/null 2>&1
sleep 2
echo "== screenshots → $OUT"
docker run --rm --network "$NET" -v "$(pwd)/harness/screenshot.py:/shot.py:ro" -v "$OUT:/out" "$IMG" sh -c "pip install -q playwright==1.47.0 >/dev/null 2>&1 && python3 /shot.py"
ls "$OUT"
