/**
 * @file g711.c G.711 Audio Codec (Dialler replacement, see patches/README.md)
 *
 * Upstream: Copyright (C) 2010 - 2015 Alfred E. Heggestad. This file adds
 * packet-loss concealment: upstream's module has no `plch`, so every lost
 * packet was 20 ms of silence on a G.711 call (the PBX leg, before G.722
 * or whenever a PBX only has G.711). The concealment is plc/plc.c —
 * pitch-period repetition with fade, our own code — and the decoder keeps
 * a per-call state for it.
 */

#include <re.h>
#include <rem.h>
#include <baresip.h>
#include "plc.h"


/**
 * @defgroup g711 g711
 *
 * The G.711 audio codec
 */


struct audec_state {
	struct plc plc;
};


static void decode_destructor(void *arg)
{
	(void)arg;
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

	st = mem_zalloc(sizeof(*st), decode_destructor);
	if (!st)
		return ENOMEM;

	plc_init(&st->plc, 8000);
	*adsp = st;

	return 0;
}


static int pcmu_encode(struct auenc_state *aes, bool *marker, uint8_t *buf,
		       size_t *len, int fmt, const void *sampv, size_t sampc)
{
	const int16_t *p = sampv;

	(void)aes;
	(void)marker;

	if (!buf || !len || !sampv)
		return EINVAL;

	if (*len < sampc)
		return ENOMEM;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	*len = sampc;

	while (sampc--)
		*buf++ = g711_pcm2ulaw(*p++);

	return 0;
}


static int pcmu_decode(struct audec_state *ads, int fmt, void *sampv,
		       size_t *sampc, bool marker,
		       const uint8_t *buf, size_t len)
{
	int16_t *p = sampv;
	size_t n = len;

	(void)marker;

	if (!sampv || !sampc || !buf)
		return EINVAL;

	if (*sampc < len)
		return ENOMEM;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	*sampc = len;

	while (n--)
		*p++ = g711_ulaw2pcm(*buf++);

	if (ads)
		plc_good(&ads->plc, sampv, len);

	return 0;
}


static int pcma_encode(struct auenc_state *aes, bool *marker, uint8_t *buf,
		       size_t *len, int fmt, const void *sampv, size_t sampc)
{
	const int16_t *p = sampv;

	(void)aes;
	(void)marker;

	if (!buf || !len || !sampv)
		return EINVAL;

	if (*len < sampc)
		return ENOMEM;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	*len = sampc;

	while (sampc--)
		*buf++ = g711_pcm2alaw(*p++);

	return 0;
}


static int pcma_decode(struct audec_state *ads, int fmt, void *sampv,
		       size_t *sampc, bool marker,
		       const uint8_t *buf, size_t len)
{
	int16_t *p = sampv;
	size_t n = len;

	(void)marker;

	if (!sampv || !sampc || !buf)
		return EINVAL;

	if (*sampc < len)
		return ENOMEM;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	*sampc = len;

	while (n--)
		*p++ = g711_alaw2pcm(*buf++);

	if (ads)
		plc_good(&ads->plc, sampv, len);

	return 0;
}


/* One lost frame: synthesise a frame the size of the last decoded one
 * (160 samples per 20 ms; ptime follows what the peer sends). */
static int g711_plc(struct audec_state *ads, int fmt, void *sampv,
		    size_t *sampc, const uint8_t *buf, size_t len)
{
	size_t n = 160;
	(void)buf;
	(void)len;

	if (!ads || !sampv || !sampc)
		return EINVAL;

	if (fmt != AUFMT_S16LE)
		return ENOTSUP;

	if (n > *sampc)
		n = *sampc;

	plc_fill(&ads->plc, sampv, n);
	*sampc = n;

	return 0;
}


static struct aucodec pcmu = {
	.pt      = "0",
	.name    = "PCMU",
	.srate   = 8000,
	.crate   = 8000,
	.ch      = 1,
	.pch     = 1,
	.ench    = pcmu_encode,
	.decupdh = decode_update,
	.dech    = pcmu_decode,
	.plch    = g711_plc,
};

static struct aucodec pcma = {
	.pt      = "8",
	.name    = "PCMA",
	.srate   = 8000,
	.crate   = 8000,
	.ch      = 1,
	.pch     = 1,
	.ench    = pcma_encode,
	.decupdh = decode_update,
	.dech    = pcma_decode,
	.plch    = g711_plc,
};


static int module_init(void)
{
	aucodec_register(baresip_aucodecl(), &pcmu);
	aucodec_register(baresip_aucodecl(), &pcma);

	return 0;
}


static int module_close(void)
{
	aucodec_unregister(&pcma);
	aucodec_unregister(&pcmu);

	return 0;
}


EXPORT_SYM const struct mod_export DECL_EXPORTS(g711) = {
	"g711",
	"audio codec",
	module_init,
	module_close,
};
