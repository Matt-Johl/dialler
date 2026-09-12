#!/bin/sh
# In-place, idempotent source patches for baresip beyond the audiounit
# full-file replacements. Usage: apply-baresip.sh <baresip-src-dir>
#
# ua_refresh_register(): send a REGISTER now on the existing register
# clients. baresip's ua_register() destroys and re-creates its clients, and
# libre's client sends an un-REGISTER for the same contact as it is
# destroyed — which lands AFTER the new REGISTER and leaves the server with
# no binding (found by `make sim-call`). Refreshing in place (sipreg_send)
# has no such side effect.
set -eu
SRC="$1"

if ! grep -q 'reg_refresh' "$SRC/src/reg.c"; then
  perl -0pi -e 's/\nbool reg_isok\(const struct reg \*reg\)\n/\nint reg_refresh(struct reg *reg)\n{\n\tif (!reg)\n\t\treturn EINVAL;\n\n\tif (!reg->sipreg)\n\t\treturn ENOENT;\n\n\treturn sipreg_send(reg->sipreg);\n}\n\n\nbool reg_isok(const struct reg *reg)\n/' "$SRC/src/reg.c"
  grep -q 'reg_refresh' "$SRC/src/reg.c" || { echo "patch: reg.c anchor not found"; exit 1; }
fi

if ! grep -q 'reg_refresh' "$SRC/src/core.h"; then
  perl -pi -e 's/^(void reg_stop\(struct reg \*reg\);)$/$1\nint  reg_refresh(struct reg *reg);/' "$SRC/src/core.h"
  grep -q 'reg_refresh' "$SRC/src/core.h" || { echo "patch: core.h anchor not found"; exit 1; }
fi

if ! grep -q 'ua_refresh_register' "$SRC/src/ua.c"; then
  perl -0pi -e 's/(\t\treg_unregister\(reg\);\n\t\}\n\}\n)/$1\n\n\/**\n * Dialler: send a REGISTER now on every existing register client, keeping\n * the clients (so no un-REGISTER is sent on the way).\n *\n * \@param ua User-Agent\n *\n * \@return 0 if success, ENOENT if no register client exists, else errorcode\n *\/\nint ua_refresh_register(struct ua *ua)\n{\n\tstruct le *le;\n\tint err = ENOENT;\n\n\tif (!ua)\n\t\treturn EINVAL;\n\n\tfor (le = ua->regl.head; le; le = le->next) {\n\t\tstruct reg *reg = le->data;\n\t\tint e = reg_refresh(reg);\n\n\t\tif (!e)\n\t\t\terr = 0;\n\t\telse if (err == ENOENT)\n\t\t\terr = e;\n\t}\n\n\treturn err;\n}\n/' "$SRC/src/ua.c"
  grep -q 'ua_refresh_register' "$SRC/src/ua.c" || { echo "patch: ua.c anchor not found"; exit 1; }
fi

if ! grep -q 'ua_refresh_register' "$SRC/include/baresip.h"; then
  perl -pi -e 's/^(void ua_unregister\(struct ua \*ua\);)$/$1\nint  ua_refresh_register(struct ua *ua);/' "$SRC/include/baresip.h"
  grep -q 'ua_refresh_register' "$SRC/include/baresip.h" || { echo "patch: baresip.h anchor not found"; exit 1; }
fi
echo "   baresip: ua_refresh_register patch present"

# Packet-loss concealment on the receive path. baresip 3.15's aurecv_receive
# discards the lost-frame count it is given ("TODO: what if lostc > 1") and
# never calls the codec's PLC handler, so every lost packet is 20 ms of
# silence whatever the codec — Opus's own concealment and its in-band FEC
# were never used (measured: 2 % loss → one audible gap per lost packet).
# Now each lost frame is concealed before the arriving packet is decoded:
# the frame right before it gets the packet, so Opus can decode the FEC it
# carries; earlier ones get plain PLC (a NULL packet). Concealed frames
# take their own timestamps, spaced evenly across the hole. Codecs without
# a PLC handler (G.711 until ios/vendor/patches adds one) are unchanged.
if ! grep -q 'Dialler: conceal lost frames' "$SRC/src/aureceiver.c"; then
  PLC_NEW="$(mktemp)"
  cat > "$PLC_NEW" <<'EOF'
	/* Dialler: conceal lost frames before decoding this packet (see
	 * ios/vendor/patches/apply-baresip.sh). The codec's PLC handler makes
	 * one frame per call; the frame right before this packet gets the
	 * packet itself, so Opus decodes its in-band FEC, the rest get PLC. */
	if (lostc && ar->ac && ar->ac->plch) {
		struct mbuf empty;
		uint32_t step;
		unsigned i, n = lostc > 5 ? 5 : lostc;

		mbuf_init(&empty);
		step = (hdr->ts - prev_ts) / (lostc + 1);
		for (i = lostc - n; i < lostc; i++) {
			struct rtp_header lh = *hdr;
			lh.ts = hdr->ts - step * (lostc - i);
			lh.m  = false;
			(void)aurecv_stream_decode(ar, &lh,
						   i + 1 == lostc ? mb : &empty,
						   1, drop);
		}
	}

	(void)aurecv_stream_decode(ar, hdr, mb, 0, drop);
EOF
  PLC_NEW="$PLC_NEW" perl -0pi -e 'BEGIN { local $/; open my $f, "<", $ENV{PLC_NEW} or die; $new = <$f>; close $f }
    s/\t\/\* TODO:  what if lostc > 1 \?\*\/.*?\t\(void\)aurecv_stream_decode\(ar, hdr, mb, 0, drop\);\n/$new/s;
    s/\tint wrap;\n\t\(void\) lostc;\n/\tint wrap;\n\tuint32_t prev_ts;\n/;
    s/(\twrap = timestamp_wrap\(hdr->ts, ar->ts_recv\.last\);\n)/\tprev_ts = ar->ts_recv.last;\n$1/;' "$SRC/src/aureceiver.c"
  rm -f "$PLC_NEW"
  grep -q 'Dialler: conceal lost frames' "$SRC/src/aureceiver.c" || { echo "patch: aureceiver.c anchor not found"; exit 1; }
  grep -q 'prev_ts = ar->ts_recv.last' "$SRC/src/aureceiver.c" || { echo "patch: aureceiver.c prev_ts anchor not found"; exit 1; }
fi
echo "   baresip: receive-path PLC/FEC patch present"
