// CBaresip implementation.
//
// Threading model (the one libre expects): libre/baresip are initialised,
// run and torn down on one loop thread. Every public call from another
// thread is marshalled onto that thread through an mqueue — a pipe the loop
// polls, so pushing wakes it — and waits for the result. Calls made from the
// loop thread itself (from inside an event callback) run directly, which
// avoids self-deadlock. re_cancel() only clears a flag, so stopping is also
// an mqueue op: the loop wakes, cancels itself, and re_main returns.
//
// Initialising on the loop thread is not cosmetic: libre_init() binds the
// libre context to the calling thread (thread-local, freed by a thread-exit
// destructor, and re_global for everyone else). Initialising on the caller's
// thread and only polling here worked until the caller was a GCD worker
// thread — libdispatch retires those when idle, the destructor freed the
// context the loop was polling, and re_main() returned "unasked" (err=0 or
// EINVAL, with "re_unlock error") seconds after a (re)start from a queue.
#include "include/cbaresip.h"

#include <errno.h>
#include <execinfo.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <re.h>
#include <rem.h> /* enum aufmt for the test devices */
#define DEBUG_MODULE "cbaresip" /* re_dbg.h expects the including module to name itself */
#define DEBUG_LEVEL 5
#include <re_dbg.h> /* not in re.h's umbrella: libre's debug channel */
#include <baresip.h>

/* Loop-thread liveness. libre's re_main() returns on re_cancel (our
 * OP_STOP) — or on a poll error it treats as fatal. Then the thread is
 * gone, no op can ever run, and cb_alive() tells the engine to rebuild the
 * stack. A backstop: with the stack owned by the loop thread this is not
 * expected to happen any more. */
static _Atomic int g_stopping;  /* OP_STOP asked the loop to end */
static _Atomic int g_loop_dead; /* re_main returned without being asked */

enum op_type {
    OP_UA_ALLOC = 1,
    OP_UA_REGISTER,
    OP_ANSWER,
    OP_HANGUP,
    OP_UA_FREE,
    OP_STOP,
    OP_AUDIO_INTERRUPT_BEGIN,
    OP_AUDIO_INTERRUPT_END,
    OP_AUDIO_TEST_ALLOC,
    OP_AUDIO_TEST_FREE,
    OP_MEDIA_STATS,
    OP_DIAL,
    OP_MUTE,
    OP_HOLD,
    OP_TRANSFER,
    OP_TRANSP_RESET
};

static cb_media_stats_t media_stats; /* filled by OP_MEDIA_STATS on the loop thread */

/* From the (patched) audiounit module — modules/audiounit/audiounit.h is
 * not part of baresip.h. See ios/vendor/patches/README.md. */
void audiosess_interrupt(bool hold);
void audiosess_stats(uint64_t *play, uint64_t *rec, uint64_t *energy);

/* Test devices (audio-probe). */
static struct auplay_st *test_play;
static struct ausrc_st *test_src;
static void test_write_h(struct auframe *af, void *arg) { (void)af; (void)arg; }
static void test_read_h(struct auframe *af, void *arg) { (void)af; (void)arg; }
static void test_error_h(int err, const char *str, void *arg) { (void)err; (void)str; (void)arg; }

struct op {
    enum op_type type;
    const char *aor; /* account line, or the URI to dial */
    bool flag;       /* mute / hold on-off */
    int err;
    bool done;
    pthread_mutex_t mu;
    pthread_cond_t cv;
};

static struct {
    pthread_t thread;
    bool running;
    cb_event_cb cb;
    void *ctx;
    struct mqueue *mq;
    struct ua *ua;
    struct call *call;
    bool registered;
} g;

/* ---- logging → app --------------------------------------------------------- */

static void emit(cb_event_t ev, const char *peer, const char *text)
{
    if (g.cb)
        g.cb(g.ctx, ev, peer ? peer : "", text ? text : "");
}

static void emit_line(const char *p, size_t n)
{
    char buf[512];
    if (!p)
        return;
    if (n >= sizeof(buf))
        n = sizeof(buf) - 1;
    memcpy(buf, p, n);
    while (n && (buf[n - 1] == '\n' || buf[n - 1] == '\r'))
        n--;
    buf[n] = 0;
    if (n)
        emit(CB_EVENT_LOG, "", buf);
}

static void log_handler(uint32_t level, const char *msg)
{
    (void)level;
    if (msg)
        emit_line(msg, strlen(msg));
}
static struct log log_entry = { .h = log_handler };

static void dbg_print(int level, const char *p, size_t len, void *arg)
{
    (void)level;
    (void)arg;
    emit_line(p, len);
}

/* ---- baresip events (loop thread) ------------------------------------------ */

static void event_handler(enum ua_event ev, struct bevent *event, void *arg)
{
    (void)arg;
    struct call *call = bevent_get_call(event);
    const char *txt = bevent_get_text(event);
    const char *peer = call ? call_peeruri(call) : "";

    switch (ev) {
    case UA_EVENT_REGISTER_OK:
        g.registered = true;
        emit(CB_EVENT_REGISTER_OK, "", txt);
        break;
    case UA_EVENT_REGISTER_FAIL:
        g.registered = false;
        emit(CB_EVENT_REGISTER_FAIL, "", txt);
        break;
    case UA_EVENT_UNREGISTERING:
        g.registered = false;
        emit(CB_EVENT_OTHER, "", "unregistering");
        break;
    case UA_EVENT_CALL_INCOMING:
        g.call = call;
        /* text = the caller's From display name (may be empty), not the
         * event text: the app names the call from it (directory first). */
        emit(CB_EVENT_CALL_INCOMING, peer, call ? call_peername(call) : "");
        break;
    case UA_EVENT_CALL_OUTGOING:
        g.call = call;
        emit(CB_EVENT_CALL_OUTGOING, peer, txt);
        break;
    case UA_EVENT_CALL_RINGING:
        emit(CB_EVENT_CALL_RINGING, peer, txt);
        break;
    case UA_EVENT_CALL_PROGRESS:
        emit(CB_EVENT_CALL_PROGRESS, peer, txt);
        break;
    case UA_EVENT_CALL_TRANSFER_FAILED:
        emit(CB_EVENT_CALL_TRANSFER_FAILED, peer, txt);
        break;
    case UA_EVENT_CALL_ESTABLISHED:
        g.call = call;
        emit(CB_EVENT_CALL_ESTABLISHED, peer, txt);
        break;
    case UA_EVENT_CALL_CLOSED:
        if (g.call == call)
            g.call = NULL;
        emit(CB_EVENT_CALL_CLOSED, peer, txt);
        break;
    default:
        emit(CB_EVENT_OTHER, peer, txt);
        break;
    }
}

/* ---- operations (always executed on the loop thread) ----------------------- */

static int do_op(struct op *op)
{
    int err = 0;
    switch (op->type) {
    case OP_UA_ALLOC:
        if (g.ua) {
            info("cbaresip: ua_alloc: freeing the previous user agent\n");
            g.ua = mem_deref(g.ua);
        }
        info("cbaresip: ua_alloc: creating\n");
        err = ua_alloc(&g.ua, op->aor);
        info("cbaresip: ua_alloc: created (err=%d); registering\n", err);
        if (!err)
            err = ua_register(g.ua); /* ua_alloc only prepares the register clients */
        info("cbaresip: ua_alloc: register requested (err=%d)\n", err);
        break;
    case OP_UA_REGISTER:
        if (!g.ua) {
            err = ENOENT;
            break;
        }
        /* ua_register() on a registered UA replaces its sipreg clients, and
         * libre's old client sends an un-REGISTER for the same contact as it
         * is destroyed — AFTER the new REGISTER, leaving the server with no
         * binding (found by `make sim-call`). ua_refresh_register (vendor
         * patch) re-sends on the existing client instead; only when there
         * is none yet do we start registration. */
        err = ua_refresh_register(g.ua);
        if (err == ENOENT)
            err = ua_register(g.ua);
        break;
    case OP_ANSWER:
        if (!g.ua || !g.call)
            err = ENOENT;
        else
            err = ua_answer(g.ua, g.call, VIDMODE_OFF);
        break;
    case OP_DIAL:
        if (!g.ua)
            err = ENOENT;
        else if (g.call)
            err = EBUSY; /* one call at a time (call_max_calls 1) */
        else
            err = ua_connect(g.ua, &g.call, NULL, op->aor, VIDMODE_OFF);
        break;
    case OP_MUTE:
        if (g.call)
            audio_mute(call_audio(g.call), op->flag);
        break;
    case OP_HOLD:
        err = g.call ? call_hold(g.call, op->flag) : ENOENT;
        break;
    case OP_TRANSFER:
        err = g.call ? call_transfer(g.call, op->aor) : ENOENT;
        break;
    case OP_HANGUP:
        if (g.ua)
            ua_hangup(g.ua, g.call, 0, NULL);
        g.call = NULL;
        break;
    case OP_TRANSP_RESET:
        /* libre keeps SIP TCP/TLS connections cached per destination and
         * sends on a cached one synchronously. After iOS has torn the
         * app's sockets down (suspension), that cached connection is dead
         * and every send on it fails at once with EPROTO — seen as
         * "ua_alloc -100" on every registration attempt, since nothing
         * evicts the connection until the loop happens to read its EOF.
         * baresip's network-change reset flushes the cache and rebuilds
         * the transports on the current addresses; registration is ours. */
        info("cbaresip: resetting SIP transports (dropping cached connections)\n");
        err = uag_reset_transp(false, false);
        info("cbaresip: transports reset (err=%d)\n", err);
        break;
    case OP_UA_FREE:
        if (g.ua) {
            info("cbaresip: ua_free: hanging up and freeing the user agent\n");
            ua_hangup(g.ua, NULL, 0, NULL);
            g.ua = mem_deref(g.ua);
            info("cbaresip: ua_free: done\n");
        }
        g.registered = false;
        g.call = NULL;
        break;
    case OP_STOP:
        atomic_store(&g_stopping, 1);
        if (g.ua) {
            ua_hangup(g.ua, NULL, 0, NULL);
            g.ua = mem_deref(g.ua);
        }
        bevent_unregister(event_handler);
        re_cancel(); /* takes effect as soon as this handler returns to the loop */
        break;
    case OP_AUDIO_INTERRUPT_BEGIN:
        audiosess_interrupt(true);
        break;
    case OP_AUDIO_INTERRUPT_END:
        audiosess_interrupt(false);
        break;
    case OP_AUDIO_TEST_ALLOC: {
        struct auplay_prm pp = { .srate = 48000, .ch = 1, .ptime = 20, .fmt = AUFMT_S16LE };
        struct ausrc_prm sp = { .srate = 48000, .ch = 1, .ptime = 20, .fmt = AUFMT_S16LE, .duration = 0 };
        test_play = mem_deref(test_play);
        test_src = mem_deref(test_src);
        err = auplay_alloc(&test_play, baresip_auplayl(), "audiounit", &pp, "default", test_write_h, NULL);
        if (!err)
            err = ausrc_alloc(&test_src, baresip_ausrcl(), "audiounit", &sp, "default", test_read_h, test_error_h, NULL);
        break;
    }
    case OP_AUDIO_TEST_FREE:
        test_src = mem_deref(test_src);
        test_play = mem_deref(test_play);
        break;
    case OP_MEDIA_STATS: {
        struct audio *au = g.call ? call_audio(g.call) : NULL;
        struct stream *s = au ? audio_strm(au) : NULL;
        memset(&media_stats, 0, sizeof(media_stats));
        if (s) {
            const struct rtcp_stats *rs = stream_rtcp_stats(s);
            struct jbuf_stat jb;
            media_stats.tx_packets = stream_metric_get_tx_n_packets(s);
            media_stats.rx_packets = stream_metric_get_rx_n_packets(s);
            media_stats.rx_errors = stream_metric_get_rx_n_err(s);
            if (rs) {
                media_stats.rx_lost = rs->rx.lost;
                media_stats.rx_jitter_us = rs->rx.jit;
            }
            if (0 == stream_jbuf_stats(s, &jb)) {
                media_stats.jb_late = jb.n_late;
                media_stats.jb_lost = jb.n_lost;
                media_stats.jb_underflow = jb.n_underflow;
                media_stats.jb_overflow = jb.n_overflow;
            }
        }
        break;
    }
    }
    return err;
}

static void mq_handler(int id, void *data, void *arg)
{
    (void)id;
    (void)arg;
    struct op *op = data;
    int err = do_op(op);
    pthread_mutex_lock(&op->mu);
    op->err = err;
    op->done = true;
    pthread_cond_broadcast(&op->cv);
    pthread_mutex_unlock(&op->mu);
}

static bool on_loop_thread(void)
{
    return g.running && pthread_equal(pthread_self(), g.thread);
}

/* Run an op on the loop thread and wait (bounded) for its result. */
static int run_op_flag(enum op_type type, const char *aor, bool flag)
{
    if (!g.running || !g.mq)
        return -ENOTCONN;
    if (atomic_load(&g_loop_dead))
        return -ENOTCONN; /* nobody will ever run it; do not wait 10 s */

    struct op op = { .type = type, .aor = aor, .flag = flag };
    if (on_loop_thread())
        return -do_op(&op);

    pthread_mutex_init(&op.mu, NULL);
    pthread_cond_init(&op.cv, NULL);

    int err = mqueue_push(g.mq, (int)type, &op);
    if (err)
        goto out;

    struct timespec deadline;
    clock_gettime(CLOCK_REALTIME, &deadline);
    deadline.tv_sec += 10;
    pthread_mutex_lock(&op.mu);
    while (!op.done) {
        if (pthread_cond_timedwait(&op.cv, &op.mu, &deadline) == ETIMEDOUT) {
            err = ETIMEDOUT;
            break;
        }
    }
    if (op.done)
        err = op.err;
    pthread_mutex_unlock(&op.mu);

out:
    pthread_cond_destroy(&op.cv);
    pthread_mutex_destroy(&op.mu);
    return -err;
}

static int run_op(enum op_type type, const char *aor)
{
    return run_op_flag(type, aor, false);
}

/* ---- lifecycle ------------------------------------------------------------- */

/* ---- loop-thread stall watchdog (diagnostics, always on) ------------------
 * A 1 s libre timer on the loop thread stamps a heartbeat. A detached
 * thread watches it and, if the loop has not beaten for 3 s, sends the loop
 * thread SIGUSR1 so it prints its own stack to stderr. A stuck loop is
 * otherwise invisible: every operation just times out (-ETIMEDOUT) and the
 * app "does nothing". Cheap enough to leave in. */
static _Atomic uint64_t g_loop_beat;
static struct tmr g_beat_tmr;
static pthread_t g_loop_pthread;
static _Atomic int g_watchdog_run;
static _Atomic int g_stall_reported;

static uint64_t mono_ms(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static void beat_handler(void *arg)
{
    (void)arg;
    atomic_store(&g_loop_beat, mono_ms());
    tmr_start(&g_beat_tmr, 1000, beat_handler, NULL);
}

static void stall_dump(int sig)
{
    void *bt[64];
    int n;
    static const char msg[] =
        "cbaresip: LOOP THREAD STALLED (no heartbeat for 3 s); its stack:\n";
    (void)sig;
    n = backtrace(bt, 64);
    (void)!write(STDERR_FILENO, msg, sizeof(msg) - 1);
    backtrace_symbols_fd(bt, n, STDERR_FILENO);
}

static void wd_say(const char *fmt, uint64_t v)
{
    char buf[128];
    int n = snprintf(buf, sizeof(buf), fmt, (unsigned long long)v);
    if (n > 0)
        (void)!write(STDERR_FILENO, buf, (size_t)n);
}

static void *watchdog_thread(void *arg)
{
    (void)arg;
    while (g.running) {
        usleep(500000);
        uint64_t beat = atomic_load(&g_loop_beat);
        if (!beat)
            continue;
        uint64_t age = mono_ms() - beat;
        if (age > 3000) {
            if (!atomic_exchange(&g_stall_reported, 1)) {
                wd_say("cbaresip: watchdog: loop heartbeat stalled %llu ms; "
                       "asking the loop thread for its stack\n", age);
                pthread_kill(g_loop_pthread, SIGUSR1);
            }
        }
        else {
            atomic_store(&g_stall_reported, 0);
        }
    }
    atomic_store(&g_watchdog_run, 0);
    return NULL;
}

/* Loop thread only: bring libre/baresip up. Everything created here is
 * bound to this thread's libre context, which lives as long as the loop. */
static int stack_open(const char *config)
{
    int err = libre_init();
    if (err)
        return err;

    dbg_init(DBG_INFO, DBG_NONE);
    dbg_handler_set(dbg_print, NULL);
    log_register_handler(&log_entry);
    log_enable_debug(true);
    log_enable_info(true);

    err = conf_configure_buf((const uint8_t *)config, strlen(config));
    if (!err)
        err = baresip_init(conf_config());
    if (!err)
        err = ua_init("Dialler", false, true, true); /* udp=off, tcp, tls */
    if (!err)
        err = conf_modules();
    if (!err)
        err = bevent_register(event_handler, NULL);
    /* The queue's pipe is registered with this thread's loop; pushes from
     * any thread wake it. */
    if (!err)
        err = mqueue_alloc(&g.mq, mq_handler, NULL);
    return err;
}

/* Loop thread only: the reverse of stack_open(), safe after a partial open. */
static void stack_close(void)
{
    g.mq = mem_deref(g.mq);
    log_unregister_handler(&log_entry);
    ua_close();
    baresip_close();
    dbg_handler_set(NULL, NULL);
    libre_close(); /* releases this thread's libre context */
}

struct start_args {
    const char *config; /* valid until we signal done, not after */
    int err;
    bool done;
    pthread_mutex_t mu;
    pthread_cond_t cv;
};

static void *loop_thread(void *arg)
{
    struct start_args *a = arg;
    struct sigaction sa;
    pthread_t wd;
    int err;

    g.thread = pthread_self(); /* on_loop_thread() is right from the first line */
    err = stack_open(a->config);
    if (err)
        stack_close();

    pthread_mutex_lock(&a->mu);
    a->err = err;
    a->done = true;
    pthread_cond_broadcast(&a->cv);
    pthread_mutex_unlock(&a->mu);
    if (err)
        return NULL; /* a lives on cb_start's stack: untouched from here on */

    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = stall_dump;
    sigemptyset(&sa.sa_mask);
    sigaction(SIGUSR1, &sa, NULL);

    g_loop_pthread = pthread_self();
    tmr_init(&g_beat_tmr);
    beat_handler(NULL);
    if (!atomic_exchange(&g_watchdog_run, 1)) {
        if (pthread_create(&wd, NULL, watchdog_thread, NULL) == 0)
            pthread_detach(wd);
        else
            atomic_store(&g_watchdog_run, 0);
    }

    err = re_main(NULL);
    if (!atomic_load(&g_stopping)) {
        /* Nobody asked: libre gave up on a poll error. Re-entering re_main
         * here corrupts its lock state (tried: a flood of "re_unlock
         * error"), so the stack comes down and the engine rebuilds it
         * (cb_alive() → false). */
        wd_say("cbaresip: re_main returned err=%llu unasked: the SIP "
               "loop is dead until the engine restarts the stack\n",
               (uint64_t)err);
        atomic_store(&g_loop_dead, 1);
    }

    tmr_cancel(&g_beat_tmr);
    atomic_store(&g_loop_beat, 0);
    stack_close();
    return NULL;
}

bool cb_alive(void)
{
    return g.running && !atomic_load(&g_loop_dead);
}

int cb_start(const char *config, cb_event_cb cb, void *ctx)
{
    struct start_args a = { .config = config };
    if (g.running)
        return -EALREADY;

    memset(&g, 0, sizeof(g));
    atomic_store(&g_loop_dead, 0);
    atomic_store(&g_stopping, 0);
    g.cb = cb;
    g.ctx = ctx;

    pthread_mutex_init(&a.mu, NULL);
    pthread_cond_init(&a.cv, NULL);
    g.running = true; /* before the thread exists: on_loop_thread() needs it */
    if (pthread_create(&g.thread, NULL, loop_thread, &a) != 0) {
        memset(&g, 0, sizeof(g));
        pthread_cond_destroy(&a.cv);
        pthread_mutex_destroy(&a.mu);
        return -EAGAIN;
    }

    pthread_mutex_lock(&a.mu);
    while (!a.done)
        pthread_cond_wait(&a.cv, &a.mu);
    pthread_mutex_unlock(&a.mu);
    pthread_cond_destroy(&a.cv);
    pthread_mutex_destroy(&a.mu);

    if (a.err) {
        pthread_join(g.thread, NULL);
        memset(&g, 0, sizeof(g));
        return -a.err;
    }
    return 0;
}

void cb_stop(void)
{
    if (!g.running)
        return;

    if (!atomic_load(&g_loop_dead))
        run_op(OP_STOP, NULL);  /* loop thread: hang up, cancel */
    if (!on_loop_thread())
        pthread_join(g.thread, NULL); /* it tears the stack down before it ends */
    /* From the loop thread (a callback stopping the engine) the teardown
     * runs after this handler returns; g is cleared now either way, and
     * cb_start() resets the atomics. */
    memset(&g, 0, sizeof(g));
}

int cb_ua_alloc(const char *aor)
{
    return run_op(OP_UA_ALLOC, aor);
}

int cb_ua_register(void)
{
    return run_op(OP_UA_REGISTER, NULL);
}

int cb_answer(void)
{
    return run_op(OP_ANSWER, NULL);
}

int cb_dial(const char *uri)
{
    return run_op(OP_DIAL, uri);
}

void cb_mute(bool muted)
{
    run_op_flag(OP_MUTE, NULL, muted);
}

int cb_hold(bool hold)
{
    return run_op_flag(OP_HOLD, NULL, hold);
}

int cb_transfer(const char *uri)
{
    return run_op(OP_TRANSFER, uri);
}

void cb_hangup(void)
{
    run_op(OP_HANGUP, NULL);
}

void cb_ua_free(void)
{
    run_op(OP_UA_FREE, NULL);
}

int cb_reset_transports(void)
{
    return run_op(OP_TRANSP_RESET, NULL);
}

bool cb_registered(void)
{
    return g.registered;
}

void cb_audio_interrupt(bool begin)
{
    run_op(begin ? OP_AUDIO_INTERRUPT_BEGIN : OP_AUDIO_INTERRUPT_END, NULL);
}

void cb_audio_stats(uint64_t *play_frames, uint64_t *rec_frames, uint64_t *play_energy)
{
    audiosess_stats(play_frames, rec_frames, play_energy);
}

void cb_media_stats(cb_media_stats_t *out)
{
    if (!out)
        return;
    if (run_op(OP_MEDIA_STATS, NULL) != 0)
        memset(&media_stats, 0, sizeof(media_stats));
    *out = media_stats;
}

int cb_audio_test_alloc(void)
{
    return run_op(OP_AUDIO_TEST_ALLOC, NULL);
}

void cb_audio_test_free(void)
{
    run_op(OP_AUDIO_TEST_FREE, NULL);
}

const char *cb_version(void)
{
    return "baresip " BARESIP_VERSION;
}
