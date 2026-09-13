/**
 * @file g722.c  G.722 audio codec (Dialler replacement, see patches/README.md)
 *
 * Upstream: Copyright (C) 2010 Alfred E. Heggestad, built on spandsp
 * (LGPL). This file keeps upstream's module shape but codes through the
 * public-domain G.722 implementation WebRTC carries (Steve Underwood's,
 * webrtc/modules/third_party/g722), so nothing LGPL is linked into the
 * app. Same wire format, same 64 kbit/s mode.
 *
 * From RFC 3551 §4.5.2: the G.722 encoder produces a stream of octets,
 * octet-aligned in the RTP packet. Even though the actual sampling rate
 * for G.722 audio is 16,000 Hz, the RTP clock rate is 8,000 Hz (assigned
 * in error in RFC 1890 and kept for compatibility): 160 clock ticks and
 * 160 octets per 20 ms, 320 samples.
 *
 * Packet-loss concealment (plc/plc.c, shared with g711) fills a lost
 * frame from the last decoded audio; upstream's module has none.
 */
#include <stdlib.h>
#include <string.h>
#include <re.h>
#include <rem_au.h>
#include <baresip.h>
#include "modules/third_party/g722/g722_enc_dec.h"
#include "plc.h"


enum {
	G722_SAMPLE_RATE = 16000,
	G722_BITRATE_64k = 64000
};


struct auenc_state {
	G722EncoderState enc;
};

struct audec_state {
	G722DecoderState dec;
	struct plc plc;
	size_t last_sampc; /* samples in the last decoded frame (the loss size) */
};


static int encode_update(struct auenc_state **aesp,
			 const struct aucodec *ac,
			 struct auenc_param *prm, const char *fmtp)
{
	struct auenc_state *st;
	(void)prm;
	(void)fmtp;

	if (!aesp || !ac)
		return EINVAL;

	if (*aesp)
		return 0;

	st = mem_zalloc(sizeof(*st), NULL);
	if (!st)
		return ENOMEM;

	if (!WebRtc_g722_encode_init(&st->enc, G722_BITRATE_64k, 0)) {
		mem_deref(st);
		return EPROTO;
	}

	*aesp = st;

	return 0;
}


static int decode_update(struct audec_state **adsp,
			 const struct aucodec *ac, const char *fmtp)
{
	struct audec_state *st;
	(void)fmtp;

	if (!adsp || !ac)
		return EINVAL;

	if (*adsp)
		return 0;

	st = mem_zalloc(sizeof(*st), NULL);
	if (!st)
		return ENOMEM;

	if (!WebRtc_g722_decode_init(&st->dec, G722_BITRATE_64k, 0)) {
		mem_deref(st);
		return EPROTO;
	}
	plc_init(&st->plc, G722_SAMPLE_RATE);
	st->last_sampc = 320;

	*adsp = st;

	return 0;
}


static int encode(struct auenc_state *st,
		  bool *marker, uint8_t *buf, size_t *len,
		  int fmt, const void *sampv, size_t sampc)
{
	size_t n;
	(void)marker;

	if (!st || !buf || !len || !sampv)
		return EINVAL;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	/* One octet per sample pair: 320 samples -> 160 octets. */
	if (sampc / 2 > *len)
		return EOVERFLOW;

	n = WebRtc_g722_encode(&st->enc, buf, sampv, sampc);
	if (n == 0)
		return EPROTO;

	*len = n;

	return 0;
}


static int decode(struct audec_state *st, int fmt, void *sampv, size_t *sampc,
		  bool marker, const uint8_t *buf, size_t len)
{
	size_t n;
	(void)marker;

	if (!st || !sampv || !sampc || !buf)
		return EINVAL;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	/* Two samples per octet. */
	if (len * 2 > *sampc)
		return ENOMEM;

	n = WebRtc_g722_decode(&st->dec, sampv, buf, len);

	*sampc = n;
	if (n) {
		st->last_sampc = n;
		plc_good(&st->plc, sampv, n);
	}

	return 0;
}


/* One lost frame: synthesise a frame the size of the last decoded one. */
static int g722_plc(struct audec_state *st, int fmt, void *sampv,
		    size_t *sampc, const uint8_t *buf, size_t len)
{
	size_t n;
	(void)buf;
	(void)len;

	if (!st || !sampv || !sampc)
		return EINVAL;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	n = st->last_sampc;
	if (n > *sampc)
		n = *sampc;

	plc_fill(&st->plc, sampv, n);
	*sampc = n;

	return 0;
}


static struct aucodec g722 = {
	.pt      = "9",
	.name    = "G722",
	.srate   = G722_SAMPLE_RATE,
	.crate   = 8000,
	.ch      = 1,
	.pch     = 1,
	.encupdh = encode_update,
	.ench    = encode,
	.decupdh = decode_update,
	.dech    = decode,
	.plch    = g722_plc,
};


static int module_init(void)
{
	aucodec_register(baresip_aucodecl(), &g722);
	return 0;
}


static int module_close(void)
{
	aucodec_unregister(&g722);
	return 0;
}


EXPORT_SYM const struct mod_export DECL_EXPORTS(g722) = {
	"g722",
	"codec",
	module_init,
	module_close
};
