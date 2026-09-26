#!/bin/sh
# dialler-admin end to end (SPEC §6 item 9c): the real UI binary against
# the real call server, driven with curl from inside the compose network.
# Login, the Fleet page with the provisioned devices, adding a device and
# being shown its code and QR, the device page, the server and calls
# pages, a CSRF refusal, and the sign-out. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."

COMPOSE_FILE="${COMPOSE_FILE:-harness/docker-compose.yml}"
PROJECT="${PROJECT:-dialler-harness}"
KEEP="${KEEP:-0}"
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f $COMPOSE_FILE $NOPORTS --profile test"
NET="${PROJECT}_default"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== up"
$COMPOSE up --build -d dialler >/dev/null 2>&1
sleep 2
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
$COMPOSE up -d admin >/dev/null 2>&1
sleep 2

# The whole browser session runs in one alpine container so the cookie jar
# persists between requests. It prints one line per check.
docker run --rm --network "$NET" alpine:3.20 sh -c '
set -u
apk add -q curl >/dev/null 2>&1
UI=https://admin:8443
J=/tmp/jar
fail() { echo "FAIL: $*"; exit 1; }
c() { curl -sk -c $J -b $J "$@"; }

# 1. No session: a page redirects to the login.
code=$(c -o /dev/null -w "%{http_code}" $UI/devices)
[ "$code" = 302 ] || fail "unauthenticated /devices gave $code, want 302"
echo "ok: unauthenticated pages redirect to login"

# 2. Wrong password is refused without touching the API; right one signs in.
body=$(c -d "password=wrong" $UI/login)
echo "$body" | grep -q "not right" || fail "wrong password not refused"
code=$(c -o /dev/null -w "%{http_code}" -d "password=harness" $UI/login)
[ "$code" = 302 ] || fail "login gave $code, want 302"
echo "ok: login"

# 3. Fleet shows the provisioned devices and the trunk state.
fleet=$(c $UI/devices)
echo "$fleet" | grep -q "<h1>Devices</h1>" || fail "no Devices heading"
for dev in dev-a dev-ha dev-hb; do echo "$fleet" | grep -q "$dev" || fail "fleet lacks $dev"; done
echo "$fleet" | grep -q "Fleet" && fail "the page is named Devices, not Fleet"
csrf=$(echo "$fleet" | sed -n "s/.*name=\"csrf\" value=\"\([^\"]*\)\".*/\1/p" | head -n1)
[ -n "$csrf" ] || fail "no csrf token on the fleet page"
echo "ok: devices page lists the devices"

# 4. A POST without the CSRF token is refused.
code=$(c -o /dev/null -w "%{http_code}" -d "csrf=bogus&user=250" $UI/devices)
[ "$code" = 403 ] || fail "bad csrf gave $code, want 403"
echo "ok: csrf enforced"

# 5. Add a device: the answer shows its code, its QR and the link.
added=$(c -d "csrf=$csrf&user=250&description=UI+test" $UI/devices)
echo "$added" | grep -q "Device added" || fail "add device did not confirm"
echo "$added" | grep -q "<svg" || fail "no QR on the added page"
echo "$added" | grep -q "dialler://enrol?" || fail "no enrolment link on the added page"
id=$(echo "$added" | sed -n "s/.*device id \(dev_[A-Z0-9]*\).*/\1/p" | head -n1)
[ -n "$id" ] || fail "no device id on the added page"
code=$(echo "$added" | sed -n "s/.*<p class=\"bigcode\">\([A-Z0-9]*\)<.*/\1/p" | head -n1)
echo "ok: added $id with code $code and a QR"

# 6. The device page, a rename with If-Match, and the directory CSV.
page=$(c $UI/devices/$id)
echo "$page" | grep -q "UI test" || fail "device page lacks the description"
upd=$(echo "$page" | sed -n "s/.*name=\"updated_at\" value=\"\([^\"]*\)\".*/\1/p" | head -n1)
code=$(c -o /dev/null -w "%{http_code}" -d "csrf=$csrf&description=Renamed&updated_at=$upd" $UI/devices/$id/description)
[ "$code" = 303 ] || fail "rename gave $code, want 303"
c $UI/devices/$id | grep -q "Renamed" || fail "rename did not stick"
code=$(c -o /dev/null -w "%{http_code}" -d "csrf=$csrf&description=Again&updated_at=$upd" $UI/devices/$id/description)
[ "$code" = 200 ] || fail "stale rename gave $code, want the page with the message"
c -d "csrf=$csrf&description=Again&updated_at=$upd" $UI/devices/$id/description | grep -q "Changed by someone else" || fail "stale If-Match not explained"
c $UI/devices/dev-a/directory.csv | head -n1 | grep -q "display_name,uri,mode,favourite" || fail "csv download"
echo "ok: device page, If-Match, csv"

# 7. Server and calls pages.
c $UI/server | grep -q "<h1>Server</h1>" || fail "server page"
c $UI/server | grep -q "Recent events" || fail "server page events"
c $UI/calls | grep -q "No calls in progress" || fail "calls page"
echo "ok: server and calls pages"

# 8. Purge the test device with its id typed, then sign out.
code=$(c -o /dev/null -w "%{http_code}" -d "csrf=$csrf&confirm=$id" $UI/devices/$id/purge)
[ "$code" = 303 ] || fail "purge gave $code"
code=$(c -o /dev/null -w "%{http_code}" -d "csrf=$csrf" $UI/logout)
[ "$code" = 302 ] || fail "logout gave $code"
code=$(c -o /dev/null -w "%{http_code}" $UI/devices)
[ "$code" = 302 ] || fail "after logout /devices gave $code, want 302"
echo "ok: purge and sign out"
' || exit 1

# The call server saw only its own API: no panic, no error.
if $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -qE 'panic:|level=ERROR'; then
  echo "FAIL: the call server logged an error during the UI run:"
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'panic:|level=ERROR' | head -5; exit 1
fi
echo "ADMIN UI PASS"
