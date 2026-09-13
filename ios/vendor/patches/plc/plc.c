/**
 * @file plc.c  Packet-loss concealment: pitch-period repetition with fade
 *
 * Method (after ITU-T G.711 Appendix I, own implementation):
 *  - keep the last 60 ms of good output;
 *  - on the first lost frame, estimate the pitch period by normalised
 *    cross-correlation of the last 20 ms against the history, over
 *    2.5-15 ms (400-67 Hz);
 *  - fill lost frames by repeating the last pitch period, overlap-adding
 *    the first quarter period with the history so the join is continuous;
 *  - attenuate by 20 % per further lost frame and go silent after five
 *    (100 ms): a long loss repeated forever sounds like a stuck buzz;
 *  - on recovery, cross-fade the first 5 ms of the real frame with the
 *    continuation of the synthetic signal.
 */
#include <string.h>
#include "plc.h"

#define PITCH_MIN_MS 2.5
#define PITCH_MAX_MS 15
#define CORR_WINDOW_MS 20
#define MAX_ERASED 5

static int ms_to_samples(int srate, double ms)
{
	return (int)(srate * ms / 1000.0 + 0.5);
}

int plc_init(struct plc *p, int srate)
{
	if (!p || (srate != 8000 && srate != 16000))
		return -1;
	memset(p, 0, sizeof(*p));
	p->srate = srate;
	return 0;
}

/* Append n samples to the history, keeping the newest PLC_HISTORY. */
static void hist_push(struct plc *p, const int16_t *pcm, size_t n)
{
	const size_t cap = (size_t)PLC_HISTORY_MS * p->srate / 1000;

	if (n >= cap) {
		memcpy(p->hist, pcm + (n - cap), cap * sizeof(int16_t));
		p->histlen = cap;
		return;
	}
	if (p->histlen + n > cap) {
		size_t drop = p->histlen + n - cap;
		memmove(p->hist, p->hist + drop,
			(p->histlen - drop) * sizeof(int16_t));
		p->histlen -= drop;
	}
	memcpy(p->hist + p->histlen, pcm, n * sizeof(int16_t));
	p->histlen += n;
}

/* Pitch period of the most recent history, in samples. Normalised
 * cross-correlation of the last window against itself shifted back by
 * each candidate lag; the best lag wins. Falls back to the maximum period
 * when the history is too short or has no periodicity. */
static int find_pitch(const struct plc *p)
{
	const int lag_min = ms_to_samples(p->srate, PITCH_MIN_MS);
	const int lag_max = ms_to_samples(p->srate, PITCH_MAX_MS);
	const int win = ms_to_samples(p->srate, CORR_WINDOW_MS);
	const int16_t *h = p->hist + p->histlen;
	int lag, best = lag_max;
	double best_score = 0;

	if ((int)p->histlen < win + lag_max)
		return lag_max;

	for (lag = lag_min; lag <= lag_max; lag++) {
		double xy = 0, xx = 0, yy = 0;
		int i;
		for (i = 1; i <= win; i++) {
			double x = h[-i], y = h[-i - lag];
			xy += x * y;
			xx += x * x;
			yy += y * y;
		}
		if (xx <= 0 || yy <= 0)
			continue;
		{
			double score = xy / (xx > yy ? xx : yy);
			/* the normalised score also penalises an energy mismatch */
			if (score > best_score) {
				best_score = score;
				best = lag;
			}
		}
	}
	return best;
}

void plc_good(struct plc *p, int16_t *pcm, size_t n)
{
	if (!p || !pcm || !n)
		return;

	if (p->erased && p->taillen) {
		/* Recovery: cross-fade the start of the real frame with the
		 * synthetic continuation so the switch back is click-free. */
		size_t k = p->taillen < n ? p->taillen : n;
		size_t i;
		for (i = 0; i < k; i++) {
			double w = (double)(i + 1) / (double)(k + 1);
			pcm[i] = (int16_t)(pcm[i] * w + p->tail[i] * (1.0 - w));
		}
	}
	p->erased = 0;
	p->taillen = 0;
	hist_push(p, pcm, n);
}

/* Generate n samples of the repeated pitch period at the current phase,
 * scaled by gain. */
static void synth(struct plc *p, int16_t *out, size_t n, double gain)
{
	const int16_t *period = p->hist + p->histlen - p->pitch;
	size_t i;
	for (i = 0; i < n; i++) {
		out[i] = (int16_t)(period[p->phase] * gain);
		p->phase = (p->phase + 1) % (size_t)p->pitch;
	}
}

void plc_fill(struct plc *p, int16_t *pcm, size_t n)
{
	double gain;
	size_t i;

	if (!p || !pcm || !n)
		return;

	if (p->histlen < (size_t)ms_to_samples(p->srate, PITCH_MAX_MS) * 2) {
		/* Nothing to repeat yet (start of the call): silence. */
		memset(pcm, 0, n * sizeof(int16_t));
		p->erased++;
		return;
	}

	if (p->erased == 0) {
		int q;
		p->pitch = find_pitch(p);
		p->phase = 0;
		/* Overlap-add the first quarter period with a continuation of
		 * the history, so the synthetic signal starts where the real
		 * one stopped instead of jumping to the period's beginning. */
		q = p->pitch / 4;
		if (q > 0) {
			const int16_t *period = p->hist + p->histlen - p->pitch;
			/* the "would have been" continuation: two periods back,
			 * advanced past what we already played */
			const int16_t *cont = p->hist + p->histlen - 2 * p->pitch;
			for (i = 0; i < (size_t)q && i < n; i++) {
				double w = (double)(i + 1) / (double)(q + 1);
				double a = cont[i + p->pitch] * (1.0 - w); /* == hist tail continued */
				double b = period[i] * w;
				pcm[i] = (int16_t)(a + b);
			}
			p->phase = (size_t)q % (size_t)p->pitch;
			if (n > (size_t)q)
				synth(p, pcm + q, n - q, 1.0);
			p->erased = 1;
			goto tail;
		}
	}

	p->erased++;
	gain = p->erased > MAX_ERASED ? 0.0 : 1.0 - 0.2 * (p->erased - 1);
	if (gain < 0)
		gain = 0;
	synth(p, pcm, n, gain);

 tail:
	/* Remember what the next few ms would have been, for the recovery
	 * cross-fade in plc_good(). Do not advance the phase for real. */
	{
		size_t saved = p->phase;
		double g = p->erased > MAX_ERASED ? 0.0 : 1.0 - 0.2 * p->erased;
		p->taillen = ms_to_samples(p->srate, 5);
		if (g < 0)
			g = 0;
		synth(p, p->tail, p->taillen, g);
		p->phase = saved;
	}
	/* The concealed audio is what the listener heard: keep the history
	 * continuous so a second loss repeats from the right place. */
	hist_push(p, pcm, n);
}
