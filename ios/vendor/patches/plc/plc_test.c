/* Host-side test for plc.c: `make plc-test`. A voiced-like signal (three
 * harmonics of 150 Hz) in 20 ms frames; frames 50 and 51 are lost. The
 * concealed frames must keep the energy up (no audible hole), join the
 * neighbours without a step, and a long loss must fade to silence. */
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include "plc.h"

static double rms(const int16_t *x, size_t n)
{
	double s = 0;
	size_t i;
	for (i = 0; i < n; i++)
		s += (double)x[i] * x[i];
	return sqrt(s / n);
}

static int run(int srate)
{
	const size_t frame = srate / 50; /* 20 ms */
	struct plc p;
	int16_t truth[100][320], buf[100][320];
	size_t f, i;
	double good, e50, e51, e52;
	int fails = 0;

	plc_init(&p, srate);
	for (f = 0; f < 100; f++) {
		for (i = 0; i < frame; i++) {
			double t = (double)(f * frame + i) / srate;
			truth[f][i] = (int16_t)(6000 * sin(2 * M_PI * 150 * t)
				+ 3000 * sin(2 * M_PI * 300 * t)
				+ 1500 * sin(2 * M_PI * 450 * t));
			buf[f][i] = truth[f][i];
		}
	}
	for (f = 0; f < 50; f++)
		plc_good(&p, buf[f], frame);
	good = rms(truth[49], frame);
	plc_fill(&p, buf[50], frame);
	plc_fill(&p, buf[51], frame);
	plc_good(&p, buf[52], frame); /* recovery cross-fade applied in place */

	/* Error against what was actually lost: repetition of a periodic
	 * signal should reproduce it closely (error well below the signal),
	 * the second frame is attenuated by design, and the cross-faded
	 * recovery frame must stay close to the real one. */
	{
		int16_t d[320];
		for (i = 0; i < frame; i++) d[i] = (int16_t)(buf[50][i] - truth[50][i]);
		e50 = rms(d, frame);
		for (i = 0; i < frame; i++) d[i] = (int16_t)(buf[51][i] - truth[51][i]);
		e51 = rms(d, frame);
		for (i = 0; i < frame; i++) d[i] = (int16_t)(buf[52][i] - truth[52][i]);
		e52 = rms(d, frame);
	}
	printf("%d Hz: signal rms=%.0f  concealed rms=%.0f/%.0f  error rms: %.0f (lost 1) %.0f (lost 2) %.0f (recovery)\n",
	       srate, good, rms(buf[50], frame), rms(buf[51], frame), e50, e51, e52);
	if (rms(buf[50], frame) < 0.5 * good || rms(buf[51], frame) < 0.4 * good) {
		printf("FAIL: concealed frames lost their energy (a hole)\n");
		fails++;
	}
	if (e50 > 0.5 * good) {
		printf("FAIL: first concealed frame is not a continuation of the signal\n");
		fails++;
	}
	if (e52 > 0.5 * good) {
		printf("FAIL: recovery cross-fade damaged the first good frame\n");
		fails++;
	}
	/* Long loss: fades out, silent by the 7th frame. */
	for (f = 0; f < 7; f++)
		plc_fill(&p, buf[60 + f], frame);
	if (rms(buf[66], frame) > 1) {
		printf("FAIL: a long loss must fade to silence, got rms=%.0f\n", rms(buf[66], frame));
		fails++;
	}
	if (rms(buf[61], frame) >= rms(buf[60], frame)) {
		printf("FAIL: no attenuation across consecutive lost frames\n");
		fails++;
	}
	return fails;
}

int main(void)
{
	int fails = run(8000) + run(16000);
	/* Loss before any history: silence, not garbage. */
	{
		struct plc p;
		int16_t out[160];
		size_t i;
		plc_init(&p, 8000);
		plc_fill(&p, out, 160);
		for (i = 0; i < 160; i++)
			if (out[i]) { printf("FAIL: non-silent fill with no history\n"); fails++; break; }
	}
	printf(fails ? "PLC TEST FAILED (%d)\n" : "PLC TEST PASSED\n", fails);
	return fails ? 1 : 0;
}
