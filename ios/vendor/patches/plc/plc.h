/**
 * @file plc.h  Packet-loss concealment for sample-domain codecs (G.711, G.722)
 *
 * Dialler's own implementation, modelled on the method of ITU-T G.711
 * Appendix I (pitch-period repetition with overlap-add and a fade), written
 * from the description; no third-party code. Plain C, no dependencies, so
 * it can be unit-tested on the host (plc_test.c) and compiled into any
 * baresip codec module.
 *
 * Use: feed every decoded frame to plc_good(); when a frame is lost, ask
 * plc_fill() for a replacement of the same length. The first good frame
 * after a loss is cross-faded with the continuation of the synthetic
 * signal inside plc_good(), so call it BEFORE handing that frame on.
 */
#ifndef DIALLER_PLC_H
#define DIALLER_PLC_H

#include <stddef.h>
#include <stdint.h>

/* Longest history kept, in milliseconds: three pitch periods of the lowest
 * pitch we track (15 ms) plus a frame. */
#define PLC_HISTORY_MS 60
#define PLC_MAX_SRATE  16000
#define PLC_HISTORY_MAX (PLC_HISTORY_MS * PLC_MAX_SRATE / 1000)

struct plc {
	int srate;                       /* 8000 or 16000 */
	int16_t hist[PLC_HISTORY_MAX];   /* most recent good output, oldest first */
	size_t histlen;                  /* valid samples at the end of hist */
	int pitch;                       /* period in samples during a loss */
	unsigned erased;                 /* consecutive lost frames so far */
	size_t phase;                    /* read position into the repeated period */
	int16_t tail[PLC_MAX_SRATE * 5 / 1000]; /* 5 ms of synthetic continuation
	                                          for the recovery cross-fade */
	size_t taillen;
};

/* Initialise for a sample rate (8000 or 16000). Returns 0, or -1 if the
 * rate is not supported. */
int plc_init(struct plc *p, int srate);

/* A decoded frame is about to be played: record it, and if it follows a
 * loss, cross-fade its start with the synthetic continuation (in place). */
void plc_good(struct plc *p, int16_t *pcm, size_t n);

/* A frame was lost: synthesise n samples in its place. */
void plc_fill(struct plc *p, int16_t *pcm, size_t n);

#endif
