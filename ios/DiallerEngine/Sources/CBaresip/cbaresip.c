// CBaresip implementation.
//
// Threading model (the one libre expects): libre/baresip run on one loop
// thread. Every public call from another thread is marshalled onto that
// thread through an mqueue — a pipe the loop polls, so pushing wakes it —
// and waits for the result. Calls made from the loop thread itself (from
// inside an event callback) run directly, which avoids self-deadlock.
// re_cancel() only clears a flag, so stopping is also an mqueue op: the
// loop wakes, cancels itself, and re_main returns.
#include "include/cbaresip.h"

#include <errno.h>
#include <pthread.h>
#include <string.h>
#include <time.h>

#include <re.h>
#include <rem.h> /* enum aufmt for the test devices */
#define DEBUG_MODULE "cbaresip" /* re_dbg.h expects the including module to name itself */
#define DEBUG_LEVEL 5
#include <re_dbg.h> /* not in re.h's umbrella: libre's debug channel */
#include <baresip.h>

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
    OP_TRANSFER
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
        emit(CB_EVENT_CALL_INCOMING, peer, txt);
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
        if (g.ua)
            g.ua = mem_deref(g.ua);
        err = ua_alloc(&g.ua, op->aor);
        if (!err)
            err = ua_register(g.ua); /* ua_alloc only prepares the register clients */
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
    case OP_UA_FREE:
        if (g.ua) {
            ua_hangup(g.ua, NULL, 0, NULL);
            g.ua = mem_deref(g.ua);
        }
        g.registered = false;
        g.call = NULL;
        break;
    case OP_STOP:
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

static void *loop_thread(void *arg)
{
    (void)arg;
    re_main(NULL);
    return NULL;
}

int cb_start(const char *config, cb_event_cb cb, void *ctx)
{
    int err;
    if (g.running)
        return -EALREADY;

    g.cb = cb;
    g.ctx = ctx;

    err = libre_init();
    if (err)
        return -err;

    dbg_init(DBG_INFO, DBG_NONE);
    dbg_handler_set(dbg_print, NULL);
    log_register_handler(&log_entry);
    log_enable_debug(true);
    log_enable_info(true);

    err = conf_configure_buf((const uint8_t *)config, strlen(config));
    if (err)
        goto fail;

    err = baresip_init(conf_config());
    if (err)
        goto fail;

    err = ua_init("Dialler", false, true, true); /* udp=off, tcp, tls */
    if (err)
        goto fail;

    err = conf_modules();
    if (err)
        goto fail;

    err = bevent_register(event_handler, NULL);
    if (err)
        goto fail;

    /* The queue's pipe is registered with the (global) loop before the loop
     * thread starts, so pushes from any thread wake it. */
    err = mqueue_alloc(&g.mq, mq_handler, NULL);
    if (err)
        goto fail;

    g.running = true;
    if (pthread_create(&g.thread, NULL, loop_thread, NULL) != 0) {
        g.running = false;
        err = EAGAIN;
        goto fail;
    }
    return 0;

fail:
    g.mq = mem_deref(g.mq);
    ua_close();
    baresip_close();
    libre_close();
    return -err;
}

void cb_stop(void)
{
    if (!g.running)
        return;

    run_op(OP_STOP, NULL);      /* loop thread: hang up, cancel */
    if (!on_loop_thread())
        pthread_join(g.thread, NULL);
    g.running = false;

    g.mq = mem_deref(g.mq);
    log_unregister_handler(&log_entry);
    ua_close();
    baresip_close();
    dbg_handler_set(NULL, NULL);
    libre_close();
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
