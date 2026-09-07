/**
 * @file sess.c  AudioUnit sound driver - session
 *
 * Copyright (C) 2010 Alfred E. Heggestad
 *
 * Dialler patch (manual audio, see ios/vendor/patches/README.md):
 *
 *  - Units are never started inside the alloc functions. Every allocated
 *    unit is started in ONE step, after the current allocation chain has
 *    returned to the libre loop (so player and recorder are both fully
 *    initialised before either starts), and only when the host has not
 *    put the audio on hold.
 *  - The host's hold (CallKit: hold until didActivate, hold again on
 *    didDeactivate) is remembered even while no units exist.
 *  - Frame counters on both callbacks, so a probe or the app can assert
 *    that CoreAudio is actually delivering audio.
 */
#include <AudioUnit/AudioUnit.h>
#include <stdatomic.h>
#include <re.h>
#include <rem.h>
#include <baresip.h>
#include "audiounit.h"


struct audiosess {
	struct list sessl;
};


struct audiosess_st {
	struct audiosess *as;
	struct le le;
	audiosess_int_h *inth;
	void *arg;
};


static struct audiosess *gas;

/* Dialler: host hold state, deferred-start timer, frame counters. */
static bool audiosess_hold = false;
static struct tmr start_tmr;
static atomic_uint_least64_t play_frames;
static atomic_uint_least64_t rec_frames;
static atomic_uint_least64_t play_energy; /* sum |sample| of rendered S16 audio */


static void sess_destructor(void *arg)
{
	struct audiosess_st *st = arg;

	list_unlink(&st->le);
	mem_deref(st->as);
}


static void destructor(void *arg)
{
	struct audiosess *as = arg;

	list_flush(&as->sessl);

	gas = NULL;
}


int audiosess_alloc(struct audiosess_st **stp,
		    audiosess_int_h *inth, void *arg)
{
	struct audiosess_st *st = NULL;
	struct audiosess *as = NULL;
	int err = 0;
	bool created = false;

	if (!stp)
		return EINVAL;


	if (gas)
		goto makesess;

	as = mem_zalloc(sizeof(*as), destructor);
	if (!as)
		return ENOMEM;

	gas = as;
	created = true;

 makesess:
	st = mem_zalloc(sizeof(*st), sess_destructor);
	if (!st) {
		err = ENOMEM;
		goto out;
	}
	st->inth = inth;
	st->arg = arg;
	st->as = created ? gas : mem_ref(gas);

	list_append(&gas->sessl, &st->le, st);

 out:
	if (err) {
		mem_deref(as);
		mem_deref(st);
	}
	else {
		*stp = st;
	}

	return err;
}


static void start_all(void *arg)
{
	struct le *le;
	(void)arg;

	if (audiosess_hold || !gas)
		return;

	for (le = gas->sessl.head; le; le = le->next) {

		struct audiosess_st *st = le->data;

		if (st->inth)
			st->inth(false, st->arg);
	}

	info("audiounit: started %u unit(s)\n", list_count(&gas->sessl));
}


bool audiosess_interrupted(void)
{
	return audiosess_hold;
}


/**
 * Called by player/recorder alloc instead of starting the unit: start
 * every allocated unit once the allocation chain has returned to the loop.
 */
void audiosess_request_start(void)
{
	if (audiosess_hold) {
		info("audiounit: unit ready; start held until the audio "
		     "session is active\n");
		return;
	}

	tmr_start(&start_tmr, 0, start_all, NULL);
}


void audiosess_interrupt(bool hold)
{
	struct le *le;

	audiosess_hold = hold;

	if (hold) {
		tmr_cancel(&start_tmr);

		if (!gas)
			return;

		for (le = gas->sessl.head; le; le = le->next) {

			struct audiosess_st *st = le->data;

			if (st->inth)
				st->inth(true, st->arg);
		}
		return;
	}

	start_all(NULL);
}


void audiosess_close(void)
{
	tmr_cancel(&start_tmr);
}


void audiosess_count(bool play, size_t frames)
{
	if (play)
		atomic_fetch_add_explicit(&play_frames, frames,
					  memory_order_relaxed);
	else
		atomic_fetch_add_explicit(&rec_frames, frames,
					  memory_order_relaxed);
}


void audiosess_count_energy(uint64_t abs_sum)
{
	atomic_fetch_add_explicit(&play_energy, abs_sum, memory_order_relaxed);
}


void audiosess_stats(uint64_t *play, uint64_t *rec, uint64_t *energy)
{
	if (play)
		*play = atomic_load_explicit(&play_frames, memory_order_relaxed);
	if (rec)
		*rec = atomic_load_explicit(&rec_frames, memory_order_relaxed);
	if (energy)
		*energy = atomic_load_explicit(&play_energy, memory_order_relaxed);
}
