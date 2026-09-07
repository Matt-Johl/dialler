#!/bin/sh
# Run a script from this repo inside a throwaway container attached to a
# compose network, so it can reach services by name (dialler:8080,
# baresip-a:4444, ...). Needed where the host cannot connect to published
# localhost ports (build sandboxes); harmless elsewhere.
#
#   sh harness/innet.sh <network> <script> [args...]
set -eu
NET="$1"; shift
SCRIPT="$1"; shift
cd "$(dirname "$0")/.."
exec docker run --rm --network "$NET" \
  -v "$(pwd):/repo:ro" -w /repo \
  -e DIALLER_API="${DIALLER_API:-http://dialler:8080}" \
  -e DIALLER_ADMIN_TOKEN="${DIALLER_ADMIN_TOKEN:-harness}" \
  alpine:3.20 sh "$SCRIPT" "$@"
