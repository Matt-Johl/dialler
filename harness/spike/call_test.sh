#!/bin/sh
# Engine-spike variant of the headless call test: same script, pointed at the
# standalone diago B2BUA in ../spike via docker-compose.spike.yml.
set -eu
cd "$(dirname "$0")/../.."
COMPOSE_FILE=harness/docker-compose.spike.yml \
PROJECT=dialler-spike \
SERVER=spike PHONE_A=phone-a PHONE_B=phone-b DOMAIN=spike \
MEDIA_DIR=harness/spike/media PROFILE="" PROVISION=0 \
exec sh harness/call_test.sh
