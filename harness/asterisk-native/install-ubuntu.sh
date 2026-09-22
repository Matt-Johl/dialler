#!/bin/sh
# Install and configure the standalone PBX on an Ubuntu box on the LAN, using
# the config files in this directory. Run ON the Ubuntu machine, as root:
#
#   scp -r harness/asterisk-native ubuntu-box:~/
#   ssh ubuntu-box 'sudo DIALLER_HOST=<this Mac's LAN address> sh asterisk-native/install-ubuntu.sh'
#
# DIALLER_HOST is the address of the light server (make dev-server on the
# Mac); Asterisk trusts calls from it by address and sends calls for 2XX to
# its trunk listener on :5062. Re-run after editing any .conf here.
#
# TRUNK_TLS=1 additionally puts the trunk on SIP over TLS, each side
# authenticating the other against a private CA (SPEC §6 item 3b) — the
# same shape as a CUCM secure trunk. Generate the certificates on the Mac
# and copy them over with this directory, so the keys travel one way only
# and the repo keeps the generator:
#
#   OUT=lan DIALLER_IP=<mac> ASTERISK_IP=<pbx> sh harness/tls/gen_certs.sh
#   scp -r harness/asterisk-native harness/tls/lan ubuntu-box:~/
#   ssh ubuntu-box 'sudo TRUNK_TLS=1 DIALLER_HOST=<mac> sh asterisk-native/install-ubuntu.sh'
#
# TRUNK_SRTP=1 puts media_encryption=sdes on the trunk endpoint, so the PBX
# offers and accepts SDES on the leg to us (SPEC §6 item 3a). It has to
# match the server: with -trunk-srtp=sdes the server offers RTP/SAVP, and
# an endpoint without this line refuses that with 488 — and the reverse
# mismatch fails the same way. Pass both here whenever you pass
# TRUNK_SRTP=sdes to make dev-server. This script renders pjsip.conf from
# the repo copy every run, so a line added by hand on the box is gone the
# next time it runs (2026-09-17: that is how the SRTP test that passed the
# day before came back as 488).
set -eu
cd "$(dirname "$0")"
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)"; exit 1; }
TRUNK_TLS="${TRUNK_TLS:-0}"
TRUNK_SRTP="${TRUNK_SRTP:-0}"
LINES="${LINES:-0}"
# A trunk is trusted by address, so it has to be told ours. A line is
# trusted by its credential and reached at whatever its REGISTER advertised,
# so in lines mode there is nothing to substitute.
if [ "$LINES" != 1 ]; then
  [ -n "${DIALLER_HOST:-}" ] || { echo "set DIALLER_HOST=<light server address>"; exit 1; }
fi
if [ "$LINES" = 1 ] && { [ "$TRUNK_TLS" = 1 ] || [ "$TRUNK_SRTP" = 1 ]; }; then
  echo "LINES=1 is plain UDP for now: a secure LINE needs a per-device certificate on the exchange, which is a follow-on (SPEC §6 item 3d). The TLS/SRTP options here configure the TRUNK." >&2
  exit 2
fi
KEYS=/etc/asterisk/keys
PBX_HOST="$(hostname -I | awk '{print $1}')"

CERTS=""
if [ "$TRUNK_TLS" = 1 ]; then
  # Wherever the scp landed: beside this directory, inside it, or in the
  # home directory it was copied to.
  for d in ../lan ./lan ../tls/lan ~/lan; do
    [ -f "$d/ca.pem" ] && [ -f "$d/asterisk.pem" ] && [ -f "$d/asterisk.key" ] && { CERTS="$d"; break; }
  done
  if [ -z "$CERTS" ]; then
    echo "TRUNK_TLS=1 needs ca.pem + asterisk.{pem,key}. On the Mac:" >&2
    echo "  OUT=lan DIALLER_IP=$DIALLER_HOST ASTERISK_IP=$PBX_HOST sh harness/tls/gen_certs.sh" >&2
    echo "  scp -r harness/tls/lan $(hostname):~/" >&2
    exit 1
  fi
  # A certificate whose SAN is not the address Asterisk is dialled by
  # verifies by hand and then fails every call, so say plainly which
  # address this one is for before installing it.
  echo "== trunk TLS: using $CERTS ($(openssl x509 -in "$CERTS/asterisk.pem" -noout -ext subjectAltName 2>/dev/null | tail -1 | tr -d ' '))"
fi

if ! command -v asterisk >/dev/null 2>&1; then
  echo "== installing asterisk"
  apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q asterisk
fi

ETC=/etc/asterisk
[ -d "$ETC.dist" ] || cp -a "$ETC" "$ETC.dist"     # keep the package's originals once
if [ "$LINES" = 1 ]; then
  # Lines mode: the light server registers a line per device instead of
  # trunking (SPEC §6 item 3c). No @DIALLER_HOST@ substitution — the address
  # we call it back on is whatever its REGISTER advertises — and no
  # identify section, which is the point (see pjsip-lines.conf).
  echo "== installing configs into $ETC (lines mode: the server registers to us)"
  cp pjsip-lines.conf "$ETC/pjsip.conf"
  cp extensions-lines.conf "$ETC/extensions.conf"
  for f in modules.conf rtp.conf logger.conf; do cp "$f" "$ETC/$f"; done
else
  echo "== installing configs into $ETC (trunk peer $DIALLER_HOST)"
  sed -e "s|@DIALLER_HOST@|$DIALLER_HOST|g" pjsip.conf > "$ETC/pjsip.conf"
  for f in extensions.conf modules.conf rtp.conf logger.conf; do cp "$f" "$ETC/$f"; done
fi

if [ "$TRUNK_TLS" = 1 ]; then
  echo "== trunk TLS: installing certificates into $KEYS"
  mkdir -p "$KEYS"
  cp "$CERTS/ca.pem" "$CERTS/asterisk.pem" "$CERTS/asterisk.key" "$KEYS/"
  chmod 640 "$KEYS"/*.key && chmod 644 "$KEYS"/*.pem
  # Same profile as harness/asterisk/pjsip-tls.conf — mutual TLS, private
  # CA, TLS 1.2 — with this box's own paths and no compose addresses.
  cat >> "$ETC/pjsip.conf" <<EOF

; ---- SIP over TLS on the trunk leg (SPEC §6 item 3b) -----------------------
; Written by install-ubuntu.sh with TRUNK_TLS=1. Mutual authentication
; against the CA in $KEYS: the light server must present a certificate
; signed by it, and it must accept ours.
[transport-tls]
type=transport
tos=cs3
cos=3
protocol=tls
bind=0.0.0.0:5061
cert_file=$KEYS/asterisk.pem
priv_key_file=$KEYS/asterisk.key
ca_list_file=$KEYS/ca.pem
method=tlsv1_2
verify_client=yes
require_client_cert=yes
verify_server=yes
EOF
  # Move the trunk endpoint onto it, exactly as the container build does.
  awk '/^\[dialler\]$/ { d=1 } /^\[/ && !/^\[dialler\]$/ { d=0 } \
       d && /^contact=sip:/ { print $0 "\\;transport=tls"; next } \
       { print } \
       d && /^type=endpoint$/ { print "transport=transport-tls" }' \
    "$ETC/pjsip.conf" > "$ETC/pjsip.conf.new" && mv "$ETC/pjsip.conf.new" "$ETC/pjsip.conf"
fi
if [ "$TRUNK_SRTP" = 1 ]; then
  echo "== trunk SRTP: media_encryption=sdes on the dialler endpoint"
  awk '/^\[dialler\]$/ { d=1 } /^\[/ && !/^\[dialler\]$/ { d=0 } { print } \
       d && /^type=endpoint$/ { print "media_encryption=sdes" }' \
    "$ETC/pjsip.conf" > "$ETC/pjsip.conf.new" && mv "$ETC/pjsip.conf.new" "$ETC/pjsip.conf"
  [ "$TRUNK_TLS" = 1 ] || echo "WARNING: SDES without TLS sends the media keys in cleartext SDP; the server will warn too" >&2
fi
chown -R asterisk:asterisk "$ETC"/*.conf "$KEYS" 2>/dev/null || true

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
  ufw allow 5060/udp >/dev/null && ufw allow 10000:10200/udp >/dev/null
  echo "== ufw: opened 5060/udp and 10000-10200/udp"
  [ "$TRUNK_TLS" = 1 ] && { ufw allow 5061/tcp >/dev/null; echo "== ufw: opened 5061/tcp (SIP over TLS)"; }
fi

systemctl enable asterisk >/dev/null 2>&1 || true
systemctl restart asterisk
sleep 2
asterisk -rx 'pjsip show endpoints' | sed -n '1,40p'
# Wideband on the trunk (SPEC §4.4 rule 4) needs codec_g722, which ships
# with the Asterisk core; without it every call silently falls back to G.711.
if asterisk -rx 'core show codecs audio' | grep -q g722; then
  echo "codec g722: present"
else
  echo "WARNING: Asterisk has no g722 codec — trunk calls will be G.711 only" >&2
fi
if [ "$TRUNK_TLS" = 1 ]; then
  if asterisk -rx 'pjsip show transport transport-tls' | grep -q '0.0.0.0:5061'; then
    echo "trunk TLS: listening on 5061"
  else
    echo "WARNING: the TLS transport did not load — check 'asterisk -rx \"pjsip show transports\"' and the log" >&2
  fi
fi
if [ "$TRUNK_SRTP" = 1 ]; then
  # Ubuntu's package ships res_srtp, but assert it: without the module an
  # endpoint with media_encryption=sdes refuses every call with 488, which
  # looks exactly like a bad offer from the server (Alpine, 2026-09-16).
  if asterisk -rx 'module show like res_srtp' | grep -q 'res_srtp.so.*Running'; then
    echo "trunk SRTP: res_srtp loaded"
  else
    echo "WARNING: res_srtp is not loaded — SDES calls will be refused with 488 (apt install asterisk-modules?)" >&2
  fi
fi
if [ "$TRUNK_TLS" = 1 ] || [ "$TRUNK_SRTP" = 1 ]; then
  echo
  echo "== now on the Mac:  $( [ "$TRUNK_TLS" = 1 ] && printf 'TRUNK_TLS=1 ' )$( [ "$TRUNK_SRTP" = 1 ] && printf 'TRUNK_SRTP=sdes ' )ASTERISK_HOST=$PBX_HOST make dev-server"
fi
echo "== done. Phone: register 101/dialler101 to $PBX_HOST:5060 UDP; console: sudo asterisk -rvvv"
