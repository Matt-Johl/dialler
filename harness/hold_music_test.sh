#!/bin/sh
# Hold music: the party who did NOT press hold must hear something.
#
# Hold means the holder stops sending, so without the server the other party
# gets silence and usually assumes the call dropped (SPEC §4.4 rule 8b). The
# music comes from this server — pre-encoded, put on the held party's own
# stream (same SSRC, sequence and SRTP context), with nothing signalled to
# them at all — so it does not depend on the far end, its PBX, or how either
# is configured.
#
# Both paths are covered: a trunk call (G.722, Asterisk in the middle) and an
# app-to-app call (Opus, no PBX anywhere). The caller plays SILENCE for the
# whole call, so anything the other phone records can only have come from
# this server. Exit 0 = pass.
set -eu
cd "$(dirname "$0")/.."
# HARNESS_NOPORTS=1 publishes nothing on the host, so this can run beside a
# native `make dev-server`.
NOPORTS=""
[ "${HARNESS_NOPORTS:-0}" = 1 ] && { NOPORTS="-f harness/docker-compose.noports.yml"; export DIALLER_PUBLIC_HOST=dialler; }
COMPOSE="docker compose -f harness/docker-compose.yml $NOPORTS --profile test"
# TRUNK_SRTP=sdes and TRUNK_TLS=1: the same knobs as trunk_test.sh, so the
# clips the server writes into a trunk leg — hold music, ring-back, busy —
# and the relay that resumes after them run against an encrypted trunk.
# Plain RTP forgives a sequence number that jumps; libsrtp's replay window
# does not, so an SRTP trunk is where a rebase mistake shows (2026-09-17:
# no audio either way after a server-bridged transfer on the LAN PBX).
if [ -n "${TRUNK_SRTP:-}" ]; then
  export ASTERISK_SRTP=yes
  export DIALLER_TRUNK_SRTP="$TRUNK_SRTP"
fi
if [ "${TRUNK_TLS:-0}" = 1 ]; then
  sh harness/tls/gen_certs.sh
  export ASTERISK_TLS=yes
  export DIALLER_TRUNK="sip:172.30.0.20:5061;transport=tls"
  export DIALLER_TRUNK_ADDR=":5062"
  export DIALLER_TRUNK_TLS_CERT=/tls/dialler.pem
  export DIALLER_TRUNK_TLS_KEY=/tls/dialler.key
  export DIALLER_TRUNK_TLS_CA=/tls/ca.pem
fi
NET=dialler-harness_default
KEEP="${KEEP:-0}"

cleanup() { [ "$KEEP" = 1 ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

ctl() { # phone json
  docker run --rm --network "$NET" alpine:3.20 sh -c \
    "p='$2'; len=\$(printf %s \"\$p\" | wc -c | tr -d ' '); printf '%s:%s,' \"\$len\" \"\$p\" | nc -w2 $1 4444 >/dev/null"
}

# heard <recording.wav> <who>: silent until the hold, then the server's
# music. The caller plays silence throughout, so audio can only be ours.
heard() {
  [ -f "$1" ] || { echo "FAIL: no recording from $2"; exit 1; }
  python3 - "$1" "$2" <<'PY'
import sys, wave, struct, math
path, who = sys.argv[1], sys.argv[2]
w = wave.open(path, "rb")
rate, n, ch = w.getframerate(), w.getnframes(), w.getnchannels()
mono = struct.unpack("<%dh" % (n * ch), w.readframes(n))[::ch]
def rms(a, b):
    s = mono[int(a * rate):int(b * rate)]
    return math.sqrt(sum(v * v for v in s) / len(s)) if s else 0.0
total, head = rms(0, len(mono) / rate), rms(0, 2.0)
print("   %s: %.1fs total_rms=%.0f first_2s_rms=%.0f" % (who, len(mono) / rate, total, head))
if total < 200:
    sys.exit("FAIL: %s heard nothing at all — no hold music reached it" % who)
if head > total / 4:
    sys.exit("FAIL: %s had audio before the hold; the caller was not silent, so this proves nothing" % who)
print("   PASS: %s was silent until the hold, then heard the server's music" % who)
PY
}

# tail_audible <recording.wav> <seconds> <who>: the last N seconds carry
# audio. Used where the point is simply that the party was not abandoned in
# silence — the busy tone at the end of a transfer nobody answered.
tail_audible() {
  [ -f "$1" ] || { echo "FAIL: no recording from $3"; exit 1; }
  python3 - "$1" "$2" "$3" <<'EOPY'
import sys, wave, struct, math
path, window, who = sys.argv[1], float(sys.argv[2]), sys.argv[3]
w = wave.open(path, "rb")
rate, n, ch = w.getframerate(), w.getnframes(), w.getnchannels()
mono = struct.unpack("<%dh" % (n * ch), w.readframes(n))[::ch]
dur = len(mono) / rate
tail = mono[max(0, int((dur - window) * rate)):]
rms = math.sqrt(sum(v * v for v in tail) / len(tail)) if tail else 0.0
print("   %s: %.1fs recorded, last %.0fs rms=%.0f" % (who, dur, window, rms))
if rms < 200:
    sys.exit("FAIL: %s was left in silence" % who)
print("   PASS: %s heard the tone rather than silence" % who)
EOPY
}

# resume_heard <recording.wav> <who>: after the resume the far end hears the
# caller about as well as it did before the hold.
#
# A plain "is there audio" check is not enough here. With a PBX in the
# middle it re-originates RTP, so a relay whose numbering jumped backwards
# still gets SOME audio through — degraded, not silent. Comparing the two
# talking windows is what shows it.
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
before = rms(1.0, 4.5)          # talking, before the hold
after = rms(dur - window, dur)  # talking, after the resume
print("   %s: %.1fs recorded, before_rms=%.0f after_rms=%.0f (last %.0fs)" % (who, dur, before, after, window))
if before < 200:
    sys.exit("FAIL: %s heard nothing BEFORE the hold; the test proves nothing" % who)
if after < 200:
    sys.exit("FAIL: %s heard nothing after the resume — the caller's audio is not getting through" % who)
if after < before * 0.6:
    sys.exit("FAIL: %s heard the caller at %.0f%% of its pre-hold level after the resume — "
             "the relayed stream is being dropped" % (who, 100.0 * after / before))
print("   PASS: %s heard the caller again after the resume, at its old level" % who)
EOPY
}

# hold_call <target> <expect-codec> <recording> <who>
hold_call() {
  rm -f "$3"
  ctl baresip-a "{\"command\":\"dial\",\"params\":\"$1@dialler\"}"
  sleep 6
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q "bridged.*to=$1" || { echo "FAIL: the call to $1 never bridged"; exit 1; }
  ctl baresip-a '{"command":"hold"}'
  sleep 8
  ctl baresip-a '{"command":"resume"}'
  sleep 2
  ctl baresip-a '{"command":"hangup"}'
  sleep 2
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep 'playing to the waiting party' | tail -1 | sed 's/^/   /'
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q "playing to the waiting party.*codec=$2" || {
    echo "FAIL: no $2 hold music for the call to $1"; exit 1; }
  heard "$3" "$4"
}

echo "== up (the app-leg caller is silent for the whole call)"
$COMPOSE up --build -d dialler asterisk >/dev/null 2>&1
sleep 3
sh harness/innet.sh "$NET" harness/provision.sh >/dev/null
AUDIO_SOURCE="aufile,/media/silence.wav" $COMPOSE up --build -d --no-deps --force-recreate baresip-a baresip-b baresip-c >/dev/null 2>&1
sleep 8
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'sip register.*user=211' || { echo "FAIL: 211 never registered"; exit 1; }

echo "== trunk call: 211 holds the desk phone (G.722, Asterisk in the middle)"
hold_call 100 G722 harness/baresip/media/out-100.wav "the desk phone"

echo "== app to app: 211 holds 212 (Opus, no PBX in the path)"
hold_call 212 opus harness/baresip/media/out-212.wav "app 212"

# The half the music proof cannot see: after a resume the relay must carry
# the caller again. Hold music moves our sequence numbers and timestamps on
# while the source knows nothing about it, so the relayed stream has to be
# rebased — otherwise the far end drops every packet as stale, which is the
# caller's microphone going silent for good (2026-09-15).
echo "== resume: 211 (now audible) holds the desk phone, resumes, and must be heard again"
AUDIO_SOURCE="aufile,/media/in.wav" $COMPOSE up --build -d --no-deps --force-recreate baresip-a >/dev/null 2>&1
sleep 8
rm -f harness/baresip/media/out-100.wav
ctl baresip-a '{"command":"dial","params":"100@dialler"}'
sleep 5
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'bridged.*to=100' || { echo "FAIL: the call never bridged"; exit 1; }
ctl baresip-a '{"command":"hold"}'
sleep 5
ctl baresip-a '{"command":"resume"}'
sleep 7
ctl baresip-a '{"command":"hangup"}'
sleep 2
resume_heard harness/baresip/media/out-100.wav "the desk phone"

# A transfer: the waiting party hears music while the target is resolved,
# then RING-BACK the moment it alerts — and that alert is also when the
# referrer is let go (SPEC §4.4 rule 6a), so the person who pressed Transfer
# is free to make another call instead of being tied to a phone ringing
# somewhere else. Extension 700 alerts for 5 s and then fails, so the
# hand-off happens and then there is nobody to hand to: the waiting party
# gets busy and the call ends. Nobody is left listening to silence.
echo "== transfer to a target that rings, then fails: referrer released, then busy"
rm -f harness/baresip/media/out-212.wav
ctl baresip-a '{"command":"dial","params":"212@dialler"}'
sleep 5
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'bridged.*to=212' || { echo "FAIL: the app-to-app call never bridged"; exit 1; }
ctl baresip-a '{"command":"transfer","params":"700@dialler"}'
sleep 16
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'playing to the waiting party|transfer: target is ringing|transfer: failed after' | tail -4 | sed 's/^/   /'
for want in 'clip=ringback' 'transfer: target is ringing; releasing the referrer' 'clip=busy'; do
  $COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q "$want" || { echo "FAIL: expected '$want' during the transfer"; exit 1; }
done
# The referrer must actually be gone — that is the whole point.
$COMPOSE logs --no-log-prefix baresip-a 2>&1 | grep -q 'Call with sip:212@dialler.*terminated' || {
  echo "FAIL: the referrer was not released; it is still tied to the call"; exit 1; }
echo "   PASS: ring-back on alerting, referrer released, busy when nobody answered"
tail_audible harness/baresip/media/out-212.wav 3 "app 212 (transferred to a target that never answered)"

# Refused BEFORE it ever rang: nothing was handed over, so the referrer
# keeps the call and is told. 701 rejects with busy immediately.
echo "== transfer to a target that rejects at once: the call stays with the referrer"
rm -f harness/baresip/media/out-212.wav
ctl baresip-a '{"command":"dial","params":"212@dialler"}'
sleep 5
ctl baresip-a '{"command":"transfer","params":"701@dialler"}'
sleep 6
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -E 'transfer: refused before it rang' | tail -1 | sed 's/^/   /'
$COMPOSE logs --no-log-prefix dialler 2>&1 | grep -q 'transfer: refused before it rang' || {
  echo "FAIL: an immediate rejection should not release the referrer"; exit 1; }
$COMPOSE logs --no-log-prefix baresip-a 2>&1 | grep -q 'transfer failed: 486' || {
  echo "FAIL: the referrer was not told why the transfer failed"; exit 1; }
sleep 3
ctl baresip-a '{"command":"hangup"}'
sleep 2
resume_heard harness/baresip/media/out-212.wav "app 212 (transfer rejected outright)" 3

echo "== hold released cleanly both times"
[ "$($COMPOSE logs --no-log-prefix dialler 2>&1 | grep -c 'hold released')" -ge 3 ] || {
  echo "FAIL: a hold was never released; the music would outlive the hold"; exit 1; }
echo "   PASS"
