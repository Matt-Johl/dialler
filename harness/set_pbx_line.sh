#!/bin/sh
# Set one device's PBX line through the admin API, from inside the compose
# network (busybox wget, so POST rather than PUT).
#
#   sh harness/innet.sh <network> harness/set_pbx_line.sh <device> <digest_user> <secret>
#
# A file of its own rather than a wget buried in a test: the JSON body and
# the two layers of shell quoting that reach it through innet.sh are exactly
# the kind of thing that silently does nothing and makes a test pass.
set -eu
API="${DIALLER_API:-https://dialler:8081}"
TOKEN="${DIALLER_ADMIN_TOKEN:-harness}"
[ $# -eq 3 ] || { echo "usage: set_pbx_line.sh <device> <digest_user> <secret>" >&2; exit 2; }

body="{\"digest_user\":\"$2\",\"secret\":\"$3\"}"
if command -v curl >/dev/null 2>&1; then
  curl -sSkf -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
       -X POST "$API/v1/admin/devices/$1/pbx-line" -d "$body"
else
  wget -q -O- --no-check-certificate --header="Authorization: Bearer $TOKEN" \
       --header='Content-Type: application/json' --post-data="$body" \
       "$API/v1/admin/devices/$1/pbx-line"
fi
echo
