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
set -eu
cd "$(dirname "$0")"
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)"; exit 1; }
[ -n "${DIALLER_HOST:-}" ] || { echo "set DIALLER_HOST=<light server address>"; exit 1; }

if ! command -v asterisk >/dev/null 2>&1; then
  echo "== installing asterisk"
  apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q asterisk
fi

ETC=/etc/asterisk
[ -d "$ETC.dist" ] || cp -a "$ETC" "$ETC.dist"     # keep the package's originals once
echo "== installing configs into $ETC (trunk peer $DIALLER_HOST)"
sed -e "s|@DIALLER_HOST@|$DIALLER_HOST|g" pjsip.conf > "$ETC/pjsip.conf"
for f in extensions.conf modules.conf rtp.conf logger.conf; do cp "$f" "$ETC/$f"; done
chown asterisk:asterisk "$ETC"/*.conf 2>/dev/null || true

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
  ufw allow 5060/udp >/dev/null && ufw allow 10000:10200/udp >/dev/null
  echo "== ufw: opened 5060/udp and 10000-10200/udp"
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
echo "== done. Phone: register 101/dialler101 to $(hostname -I | awk '{print $1}'):5060 UDP; console: sudo asterisk -rvvv"
