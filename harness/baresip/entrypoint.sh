#!/bin/sh
# Builds a per-container ~/.baresip from the templates, substituting the SIP
# user/domain and locating the distro's module directory.
set -eu
: "${SIP_USER:=201}"
: "${SIP_DOMAIN:=dialler}"
: "${REGINT:=300}"
CFG="$HOME/.baresip"
mkdir -p "$CFG"

MODDIR="$(dirname "$(find /usr/local/lib /usr/lib -path '*baresip/modules/opus.so' 2>/dev/null | head -n1)")"
[ -n "$MODDIR" ] || { echo "baresip modules not found"; exit 1; }

sed -e "s|@MODULE_PATH@|$MODDIR|g" -e "s|@SIP_USER@|$SIP_USER|g" /opt/baresip/config > "$CFG/config"
if [ "$REGINT" = 0 ]; then
  # Start with no user agent at all: the wake test creates one on demand via
  # ctrl_tcp `uanew`, which registers immediately — the same shape as the app
  # bringing its SIP stack up after a wake. (A UA created with regint=0 has no
  # registration objects, so `uareg` on it is a no-op.)
  echo "# no accounts: created on wake via uanew" > "$CFG/accounts"
else
  OB=""
  [ -n "${OUTBOUND:-}" ] && OB=";outbound=\"sip:$OUTBOUND;transport=tls\""
  sed -e "s|@SIP_USER@|$SIP_USER|g" -e "s|@SIP_DOMAIN@|$SIP_DOMAIN|g" -e "s|@REGINT@|$REGINT|g" -e "s|@OUTBOUND@|$OB|g" /opt/baresip/accounts > "$CFG/accounts"
fi

[ -f /media/in.wav ] || echo "warning: /media/in.wav missing; run harness/baresip/media/gen_tone.py"

# Note: a NAT'd phone cannot be modelled with docker networks on Docker
# Desktop (it routes between bridges at the VM level, bypassing any router
# container). The real NAT regression test is `make probe-call`: the engine
# probe on the Mac reaches the server through Docker's port forwarding, which
# genuinely rewrites the source address.
exec baresip -f "$CFG" -v
