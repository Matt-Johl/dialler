#!/bin/sh
# Enrol the real app (201 → dev-a), the two docker harness phones (211 →
# dev-ha, 212 → dev-hb) and the simulator (203 → dev-s), and seed the
# directory. Run after `docker compose up dialler`, either from the host
# (curl) or inside the compose network via innet.sh (busybox wget):
#
#   sh harness/provision.sh
#   sh harness/innet.sh dialler-harness_default harness/provision.sh
#
# Tokens are FIXED dev fixtures so a re-provisioned harness (every
# `down -v` / `harness-up`) keeps the same credentials and the app does not
# need re-entering. Override with DEV_A_TOKEN / DEV_B_TOKEN. Not for prod.
#
# Each device line the server prints also carries a fresh enrolment "code"
# and its "url" (dialler://enrol?…), valid for fifteen minutes: type the
# code into a fresh install's onboarding screen (or render the url as a QR)
# to enrol it through the real flow (SPEC §6 item 8). Claiming it rotates
# that device's token, so a phone enrolled that way no longer uses the
# fixed one above.
set -eu
# TLS on the server's (self-signed in dev) certificate, hence -k below.
API="${DIALLER_API:-https://127.0.0.1:8080}"
TOKEN="${DIALLER_ADMIN_TOKEN:-harness}"
DEV_A_TOKEN="${DEV_A_TOKEN:-tok_dev_a_harness_fixed}"
DEV_HA_TOKEN="${DEV_HA_TOKEN:-tok_dev_ha_harness_fixed}"
DEV_HB_TOKEN="${DEV_HB_TOKEN:-tok_dev_hb_harness_fixed}"

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
# dev-a / 201 is the REAL app's identity (what `make dev-server` prints for
# its Settings). No harness phone uses it: a real phone pointed at this
# server (the app with the native server stopped) would otherwise take the
# harness's calls to 201 — it rang the user's phone during the trunk tests.
#
# A phone enrolled by code holds a ROTATED token, and re-issuing the fixed
# one here on every `make dev-server` start threw that phone off the
# server until it re-enrolled (2026-09-21). So dev-a's credential is only
# issued when dev-a is not enrolled yet (a fresh harness volume); an
# enrolled dev-a is left as it is and merely gets a fresh code.
get() {
  if command -v curl >/dev/null 2>&1; then
    curl -sSk -H "Authorization: Bearer $TOKEN" "$API$1"
  else
    wget -q -O- --no-check-certificate --header="Authorization: Bearer $TOKEN" "$API$1"
  fi
}
if get /v1/admin/devices | tr '}' '\n' | grep '"device_id":"dev-a"' | grep -q '"enrolled":true'; then
  echo "dev-a is enrolled already; keeping its credential"
  post /v1/admin/devices '{"device_id":"dev-a","user":"201"}'
else
  post /v1/admin/devices "{\"device_id\":\"dev-a\",\"user\":\"201\",\"token\":\"$DEV_A_TOKEN\"}"
fi
# The docker phones (baresip-a / baresip-b in docker-compose.yml).
post /v1/admin/devices "{\"device_id\":\"dev-ha\",\"user\":\"211\",\"token\":\"$DEV_HA_TOKEN\"}"
post /v1/admin/devices "{\"device_id\":\"dev-hb\",\"user\":\"212\",\"token\":\"$DEV_HB_TOKEN\"}"
# The simulator harness (sim_call.sh) and the Mac engine probe: 203 → dev-s.
post /v1/admin/devices "{\"device_id\":\"dev-s\",\"user\":\"203\",\"token\":\"${DEV_S_TOKEN:-tok_dev_s_harness_fixed}\"}"

# PBX lines (SPEC §6 item 3c): PBX_LINES=1 gives the two docker phones the
# credentials harness/asterisk/pjsip-lines.conf expects, so the server can
# register them as third-party SIP devices. Only meaningful against a PBX
# built with ASTERISK_LINES=yes and a server run with -pbx-mode=lines; the
# credentials are simply stored otherwise.
#
# The secret is write-only: nothing reads it back out of the server, here or
# anywhere else, which is why it is repeated in the test rather than fetched.
if [ "${PBX_LINES:-0}" = 1 ]; then
  echo "# pbx lines"
  post /v1/admin/devices/dev-ha/pbx-line \
    "{\"digest_user\":\"${LINE_HA_USER:-line211}\",\"secret\":\"${LINE_HA_SECRET:-linepass-211}\"}" >/dev/null
  post /v1/admin/devices/dev-hb/pbx-line \
    "{\"digest_user\":\"${LINE_HB_USER:-line212}\",\"secret\":\"${LINE_HB_SECRET:-linepass-212}\"}" >/dev/null
  echo "dev-ha: line 211, dev-hb: line 212"
fi

# Server-managed settings (SPEC §6 item 8b): DEV_A_SSIDS="Office,Office-5G"
# gives the real app's device its office Wi-Fi list, which the app applies
# to Local Push on its next welcome. Unset, dev-a is left as the phone has
# it; the harness phones need none.
if [ -n "${DEV_A_SSIDS:-}" ]; then
  echo "# settings"
  list="$(printf '%s' "$DEV_A_SSIDS" | awk -F, '{for (i=1;i<=NF;i++) {gsub(/^ +| +$/,"",$i); printf "%s\"%s\"", (i>1?",":""), $i}}')"
  post /v1/admin/devices/dev-a/config "{\"ssids\":[$list]}"
fi

echo "# directories (one per device, SPEC §6 item 7; the same seed for each)"
# POST per contact rather than the replace-all PUT: this also runs under
# busybox wget (innet.sh), which has no PUT. Upsert-by-URI keeps a re-seed
# from duplicating anything.
for dev in dev-a dev-ha dev-hb dev-s; do
  for c in \
    '{"display_name":"Matt (201)","uri":"sip:201@dialler","mode":"local","favourite":true}' \
    '{"display_name":"Harness phone A (211)","uri":"sip:211@dialler","mode":"local"}' \
    '{"display_name":"Harness phone B (212)","uri":"sip:212@dialler","mode":"local"}' \
    '{"display_name":"Desk phone (100)","uri":"sip:100@asterisk","mode":"trunk"}' \
    '{"display_name":"SIP phone (101)","uri":"sip:101@asterisk","mode":"trunk","favourite":true}' \
    '{"display_name":"Echo test (server)","uri":"sip:echo@dialler","mode":"local"}' \
    '{"display_name":"Echo test (PBX, 600)","uri":"sip:600@asterisk","mode":"trunk"}'
  do
    post "/v1/admin/devices/$dev/directory" "$c" >/dev/null
  done
  echo "$dev: seeded"
done
