#!/bin/sh
# Enrol the two harness phones (201 → dev-a, 202 → dev-b) and seed the
# directory. Run after `docker compose up dialler`, either from the host
# (curl) or inside the compose network via innet.sh (busybox wget):
#
#   sh harness/provision.sh
#   sh harness/innet.sh dialler-harness_default harness/provision.sh
#
# Tokens are FIXED dev fixtures so a re-provisioned harness (every
# `down -v` / `harness-up`) keeps the same credentials and the app does not
# need re-entering. Override with DEV_A_TOKEN / DEV_B_TOKEN. Not for prod.
set -eu
# TLS on the server's (self-signed in dev) certificate, hence -k below.
API="${DIALLER_API:-https://127.0.0.1:8080}"
TOKEN="${DIALLER_ADMIN_TOKEN:-harness}"
DEV_A_TOKEN="${DEV_A_TOKEN:-tok_dev_a_harness_fixed}"
DEV_B_TOKEN="${DEV_B_TOKEN:-tok_dev_b_harness_fixed}"

# post PATH BODY — prints the response body; fails on a non-2xx status and
# shows the server's explanation instead of a bare exit code.
post() {
  if command -v curl >/dev/null 2>&1; then
    out="$(curl -sSk -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
                -X POST "$API$1" -d "$2" -w '\n%{http_code}')"
    code="${out##*
}"
    body="${out%
*}"
    case "$code" in
      2*) echo "$body" ;;
      *)  echo "POST $1 → HTTP $code: $body" >&2; return 1 ;;
    esac
  else
    # busybox wget: non-2xx exits non-zero and reports on stderr
    if ! body="$(wget -q -O- --no-check-certificate --header="Authorization: Bearer $TOKEN" \
                  --header='Content-Type: application/json' \
                  --post-data="$2" "$API$1" 2>&1)"; then
      echo "POST $1 failed: $body" >&2; return 1
    fi
    echo "$body"
  fi
}

echo "# devices (fixed dev tokens)"
post /v1/admin/devices "{\"device_id\":\"dev-a\",\"user\":\"201\",\"token\":\"$DEV_A_TOKEN\"}"
post /v1/admin/devices "{\"device_id\":\"dev-b\",\"user\":\"202\",\"token\":\"$DEV_B_TOKEN\"}"

echo "# directory"
post /v1/directory '{"display_name":"Matt (201)","uri":"sip:201@dialler","mode":"local"}'
post /v1/directory '{"display_name":"Phone B (202)","uri":"sip:202@dialler","mode":"local"}'
post /v1/directory '{"display_name":"Desk phone (100)","uri":"sip:100@asterisk","mode":"trunk"}'
post /v1/directory '{"display_name":"SIP phone (101)","uri":"sip:101@asterisk","mode":"trunk"}'
post /v1/directory '{"display_name":"Echo test (server)","uri":"sip:echo@dialler","mode":"local"}'
post /v1/directory '{"display_name":"Echo test (PBX, 600)","uri":"sip:600@asterisk","mode":"trunk"}'
