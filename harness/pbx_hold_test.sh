#!/bin/sh
# The PBX's own phone holds and resumes: the app must keep hearing it.
#
# Nothing is signalled to us when a desk phone behind the PBX presses hold —
# the PBX keeps the trunk leg up and puts music, silence, or nothing on it,
# then the phone's audio again. What the app gets is whatever that does to
# the RTP timeline on one SSRC. On the LAN PBX it went 31 s backwards in one
# step (2026-09-18 08:45) and the app's playout buffer, ordered by
# timestamp, dropped every later frame: silent to the end of the call
# (SPEC §9 item 11). The relay now re-bases a timeline that parts from the
# packets' arrival (pump.forward, `pumpTimelineBreak`).
#
# Here the desk phone is baresip-c (100, on Asterisk), audible throughout;
# the app (211, baresip-a) is silent, so its recording can only hold what
# the desk phone sent. 100 holds for a few seconds and resumes; 211 must
# hear 100 after the resume about as well as before. Whether Asterisk's
# hold breaks the timeline the way the LAN phone's did is printed, not
# asserted — the user-visible outcome is what is asserted. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test"
NET=dialler-harness_default
KEEP="${KEEP:-0}"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

ctl() { # phone json
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$2'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 $1 4444 >/dev/null"
}

# resume_heard <recording.wav> <who>: after the resume the app hears the
# desk phone about as well as it did before the hold (hold_music_test.sh
# uses the same comparison the other way round).
resume_heard() {
  [ -f "$1" ] || { echo "FAIL: no recording from $2"; exit 1; }
  python3 - "$1" "$2" "${3:-5}" <<'EOPY'
import sys, wave, struct, math
path, who, window = sys.argv[1], sys.argv[2], float(sys.argv[3])
w = wave.open(path, "rb")
rate, n, ch = w.getframerate(), w.getnframes(), w.getnchannels()
mono = struct.unpack("<%dh" % (n * ch), w.readframes(n))[::ch]
dur = len(mono) / rate
def rms(a, b):
    s = mono[max(0, int(a * rate)):int(b * rate)]
    return math.sqrt(sum(v * v for v in s) / len(s)) if s else 0.0
before = rms(1.0, 4.5)          # the desk phone talking, before its hold
after = rms(dur - window, dur)  # talking again, after its resume
print("   %s: %.1fs recorded, before_rms=%.0f after_rms=%.0f (last %.0fs)" % (who, dur, before, after, window))
if before < 200:
    sys.exit("FAIL: %s heard nothing BEFORE the hold; the test proves nothing" % who)
if after < 200:
    sys.exit("FAIL: %s heard nothing after the resume — the desk phone's audio is not getting through" % who)
if after < before * 0.6:
    sys.exit("FAIL: %s heard the desk phone at %.0f%% of its pre-hold level after the resume — "
             "the relayed stream is being dropped" % (who, 100.0 * after / before))
print("   PASS: %s heard the desk phone again after its resume, at the old level" % who)
EOPY
}

echo "== up (app 211 silent; desk phone 100 audible)"
$COMPOSE up --build -d dialler asterisk >/dev/null 2>&1
sleep 3
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
AUDIO_SOURCE="aufile,/media/silence.wav" $COMPOSE up --build -d --no-deps --force-recreate baresip-a >/dev/null 2>&1
AUDIO_SOURCE="aufile,/media/in.wav" $COMPOSE up --build -d --no-deps --force-recreate baresip-c >/dev/null 2>&1
sleep 8
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'sip register.*user=211' || { echo "FAIL: 211 never registered"; exit 1; }

echo "== 211 calls the desk phone; the desk phone holds, then resumes"
rm -f harness/baresip/media/out-211.wav
ctl baresip-a '{"command":"dial","params":"100@dialler"}'
sleep 6
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'bridged.*to=100' || { echo "FAIL: the call to 100 never bridged"; exit 1; }
ctl baresip-c '{"command":"hold"}'
sleep 6
ctl baresip-c '{"command":"resume"}'
sleep 7
ctl baresip-a '{"command":"hangup"}'
sleep 2

echo "== what the relay saw on the trunk leg (callee→caller is the desk phone's stream)"
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'dir=callee→caller' | tail -3 | sed -E 's/^.*msg=relay //; s/^/   /'
if $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'source timeline broke'; then
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep 'source timeline broke' | sed -E 's/^.*msg=/   /'
else
  echo "   (this PBX's hold did not break the timeline; the relay had nothing to re-base)"
fi
resume_heard harness/baresip/media/out-211.wav "app 211"
