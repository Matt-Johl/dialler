// CBaresip: a minimal C surface over libre/baresip for the Swift engine.
// It owns the libre main loop thread and serialises API calls onto it.
#ifndef CBARESIP_H
#define CBARESIP_H

#include <stdbool.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef enum {
    CB_EVENT_REGISTER_OK = 1,
    CB_EVENT_REGISTER_FAIL,
    CB_EVENT_CALL_INCOMING,
    CB_EVENT_CALL_RINGING,
    CB_EVENT_CALL_ESTABLISHED,
    CB_EVENT_CALL_CLOSED,
    /// Our outgoing INVITE was sent / the far end is ringing (180) or
    /// sending early media (183).
    CB_EVENT_CALL_OUTGOING,
    CB_EVENT_CALL_PROGRESS,
    /// Our REFER was rejected or the transfer target failed; `text` has the
    /// status. The call continues.
    CB_EVENT_CALL_TRANSFER_FAILED,
    CB_EVENT_OTHER,
    /// A line from libre/baresip's own log; `text` is the message.
    CB_EVENT_LOG
} cb_event_t;

/// Event callback. `peer` is the remote URI for call events ("" otherwise);
/// `text` is baresip's event text (reason, error) or "" — except for
/// CB_EVENT_CALL_INCOMING, where it is the caller's From display name ("" if
/// the caller sent none), so the app can name the call from it.
typedef void (*cb_event_cb)(void *ctx, cb_event_t event, const char *peer, const char *text);

/// Initialise libre + baresip with the given config text (baresip `config`
/// file syntax) and start the main loop on a background thread.
/// Returns 0 or a negative errno.
int cb_start(const char *config, cb_event_cb cb, void *ctx);

/// Stop the main loop, tear everything down. Safe to call twice.
void cb_stop(void);

/// Create the user agent for `aor` (baresip account line, e.g.
/// "<sip:201@dialler;transport=tls>;outbound=\"sip:10.0.0.5:5061;transport=tls\";regint=300;answermode=manual")
/// and start registering. Returns 0 or a negative errno.
int cb_ua_alloc(const char *aor);

/// Send a fresh REGISTER for the existing user agent now (new flow if the
/// old connection is dead). Returns 0, or a negative errno; -ENOENT if no
/// user agent exists.
int cb_ua_register(void);

/// Answer the current incoming call (no-op if none).
int cb_answer(void);

/// Place an outgoing call to `uri` (full SIP URI) from the user agent.
/// Progress arrives as CB_EVENT_CALL_OUTGOING / RINGING / PROGRESS /
/// ESTABLISHED / CLOSED. Returns 0 or a negative errno.
int cb_dial(const char *uri);

/// Mute / unmute the microphone of the current call (no-op if none).
void cb_mute(bool muted);

/// Put the current call on hold / resume it (re-INVITE). Returns 0 or a
/// negative errno.
int cb_hold(bool hold);

/// Blind transfer: REFER the far end of the current call to `uri` (the
/// server, as B2BUA, connects the other party there and ends our call).
/// Returns 0 or a negative errno.
int cb_transfer(const char *uri);

/// Hang up the current call (no-op if none).
void cb_hangup(void);

/// Unregister and free the user agent.
void cb_ua_free(void);

/// Whether the UA holds a live registration.
bool cb_registered(void);

/// Manual audio hold from the host (CallKit): begin=true holds — running
/// units stop and units allocated later are initialised but not started;
/// begin=false releases — all units start. The (patched) audiounit module
/// never starts audio on its own; see ios/vendor/patches/README.md.
void cb_audio_interrupt(bool begin);

/// Frames delivered by CoreAudio through the driver's callbacks since the
/// stack started (play = output rendered, rec = input captured), and the
/// summed |sample| energy of rendered output — zero energy with frames
/// flowing means the far end is silent (or we are decoding nothing).
void cb_audio_stats(uint64_t *play_frames, uint64_t *rec_frames, uint64_t *play_energy);

/// Media-path counters for the current call's audio stream (all zero when
/// there is no call). Together with cb_audio_stats this locates a silent
/// or glitchy call: units rendering but rx=0 is the media path, not audio;
/// rx flowing with lost/late/underflow climbing is jitter or loss upstream.
typedef struct {
    uint32_t tx_packets;   ///< RTP sent
    uint32_t rx_packets;   ///< RTP received
    uint32_t rx_errors;    ///< receive errors (decode/parse)
    int32_t  rx_lost;      ///< packets lost on the way to us (RTCP)
    uint32_t rx_jitter_us; ///< inter-arrival jitter (RTCP)
    uint32_t jb_late;      ///< jitter buffer: frames arriving too late
    uint32_t jb_lost;      ///< jitter buffer: frames lost
    uint32_t jb_underflow; ///< jitter buffer: player starved (an audible gap)
    uint32_t jb_overflow;  ///< jitter buffer: dropped as too many queued
} cb_media_stats_t;
void cb_media_stats(cb_media_stats_t *out);

/// Test hooks (audio-probe): allocate / free a player + source through
/// baresip's device layer without a SIP call, so the driver's start/hold
/// behaviour can be asserted directly. 48 kHz mono 20 ms S16LE.
int cb_audio_test_alloc(void);
void cb_audio_test_free(void);

/// libre/baresip version string, for logs.
const char *cb_version(void);

#ifdef __cplusplus
}
#endif

#endif
