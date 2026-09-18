#!/bin/sh
# A small private CA and two peer certificates for the TLS trunk
# (SPEC §6 near-term item 3b), written to OUT (default: next to this
# script).
#
#   <OUT>/ca.pem              the CA both sides verify against
#   <OUT>/dialler.{pem,key}   the light server's trunk certificate
#   <OUT>/asterisk.{pem,key}  the PBX's
#
# The default set is for the docker harness, whose addresses are fixed by
# docker-compose.yml. A PBX on the LAN needs its own set, because the SANs
# have to be the addresses each side is really dialled by — keep it out of
# the way of the harness's:
#
#   OUT=lan DIALLER_IP=<mac> ASTERISK_IP=<pbx> sh harness/tls/gen_certs.sh
#
# This is the shape a real deployment has: a private CA, not the system
# roots (no public authority will sign an internal PBX), and certificates
# that are BOTH server and client — each side is the TLS server for the
# calls it receives and the TLS client for the ones it places, and CUCM's
# secure trunks require a client certificate.
#
# The names are what each side is actually dialled by, fixed addresses on
# the compose network (docker-compose.yml pins them, because Asterisk's
# resolver does not see compose DNS). A certificate without the matching
# IP SAN verifies fine by hand and then fails every call, which is the
# mistake this script exists to prevent.
#
# Output is gitignored: regenerate rather than commit keys. Idempotent —
# it leaves existing certificates alone unless FORCE=1.
set -eu
cd "$(dirname "$0")"
OUT="${OUT:-.}"
mkdir -p "$OUT"
cd "$OUT"
WHERE="harness/tls${OUT:+/$OUT}"
[ "$OUT" = "." ] && WHERE="harness/tls"

DIALLER_IP="${DIALLER_IP:-172.30.0.10}"
ASTERISK_IP="${ASTERISK_IP:-172.30.0.20}"
DAYS="${DAYS:-825}" # the browser-era cap; nothing here enforces it, but a
                    # harness certificate that outlives the harness is a
                    # certificate nobody notices has expired.

if [ "${FORCE:-0}" != 1 ] && [ -f ca.pem ] && [ -f dialler.pem ] && [ -f asterisk.pem ]; then
  echo "$WHERE: certificates already present (FORCE=1 to regenerate)"
  exit 0
fi

rm -f ca.pem ca.key ca.srl dialler.pem dialler.key asterisk.pem asterisk.key

echo "== CA"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout ca.key -out ca.pem -days "$DAYS" \
  -subj "/CN=dialler-harness-ca" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

peer() { # name ip dns
  name=$1; ip=$2; dns=$3
  echo "== $name  (IP:$ip, DNS:$dns)"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$name.key" -out "$name.csr" -subj "/CN=$dns" 2>/dev/null
  # serverAuth AND clientAuth: see the header. Asterisk's verify_client
  # rejects a certificate without clientAuth, and Go's verifier rejects one
  # without serverAuth, so a single-purpose certificate breaks one
  # direction of the trunk and leaves the other working — the worst
  # failure to diagnose.
  openssl x509 -req -in "$name.csr" -CA ca.pem -CAkey ca.key -CAcreateserial \
    -out "$name.pem" -days "$DAYS" \
    -extfile /dev/stdin 2>/dev/null <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=IP:$ip,DNS:$dns
EOF
  rm -f "$name.csr"
}

peer dialler "$DIALLER_IP" dialler
peer asterisk "$ASTERISK_IP" asterisk

# Asterisk runs as its own user and reads these from a read-only mount.
chmod 644 ca.pem dialler.pem asterisk.pem
chmod 644 dialler.key asterisk.key
echo "$WHERE: wrote ca.pem, dialler.{pem,key}, asterisk.{pem,key} (server $DIALLER_IP, PBX $ASTERISK_IP)"
