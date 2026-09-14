# Local changes to vendored modules

The server vendors its dependencies (`go mod vendor` is not run in the
sandbox; the module proxy is unreachable). The files below differ from the
upstream release named in `modules.txt`; re-apply when bumping.

## github.com/emiago/diago v0.32.0

- `dialog_client_session.go`: `func (d *DialogClientSession) SetCodecs([]media.Codec)`
  — per-dialog codec offer for an outgoing leg. Used by `internal/b2bua` to
  offer only G.711 towards the PBX trunk, so the raw relay never needs to
  transcode (the app leg is answered with the codec the trunk leg took).
- `media/sdp/utils.go` + `media/media_session.go`: the Opus `a=fmtp:96`
  line comes from `sdp.OpusFmtp` instead of a fixed `useinbandfec=0`.
  Upstream's value told every phone's encoder to disable in-band FEC
  (baresip configures its encoder from the peer's fmtp), so no app leg
  carried FEC and every lost packet was an audible gap. Ours asks for FEC,
  mono and 32 kbit/s (2026-09-12, audio-quality plan Phase A; measured on
  the impaired echo path).
- `media/codec.go`, `media/sdp/formats.go`, `media/media_session.go`,
  `media/sdp/utils.go`: G.722 (`CodecAudioG722`, static PT 9, RTP clock
  8000 per RFC 3551 though the audio is 16 kHz) is known to the codec
  table, parsed from an offer with or without its rtpmap, and emitted as
  `a=rtpmap:9 G722/8000`. The server only offers, matches and relays it
  (plan Phase C, wideband on PBX calls without transcoding).
- `media/media_session.go` (`MediaSession.SetCodecs`), `dialog_media.go`
  (`DialogMedia.SetCodecs`, `applyReInviteAnswer`),
  `dialog_client_session.go` + `dialog_server_session.go` (`ReInvite` applies
  the answer's SDP): a re-INVITE can move an established leg onto another
  codec. Upstream's ReInvite re-sent the current SDP and ignored the
  answer's body (fine for hold, not for a codec change). The B2BUA uses it
  when a transfer target answers with a codec the remaining leg does not
  have — an app on Opus transferred to the PBX — instead of failing the
  transfer with 488 (plan Phase C).
- `dialog_client_session.go` (`invite`, originator handling): the callee
  is offered every audio codec common to the originator and the dialog
  (originator order), not only the first one. Upstream's single-codec
  offer gave the far end no fallback: with G.722 preferred, a G.711-only
  PBX was offered G.722 alone and the call failed ("no supported codecs
  found", user's Asterisk, 2026-09-12). Both legs still end on one common
  codec — the caller is answered with whatever the callee took.
- `media/rtp_packet_writer.go`: `RTPPacketWriter.WriteSamplesSeq` — like
  `WriteSamples` with the sequence number chosen by the caller (and the
  writer's sequencer moved to it). The relay rebases the source's sequence
  numbers onto its own instead of renumbering every packet, so upstream
  loss and reordering reach the far end's jitter buffer and concealment
  (plan Phase D; before, a lost packet became a silent hole in a
  seamlessly numbered stream that no decoder could conceal).
- `media/media_session.go` (`media.ListenConfig`) — QoS: RTP/RTCP sockets
  are opened through a package-level `net.ListenConfig` so the B2BUA can
  mark them DSCP EF from its Control hook (plan Phase E; std lib only).

- `media/media_session.go` (`OriginatorCodecs`, `SecureRTPActive`) and
  `dialog_client_session.go` (`invite`, originator handling) — SRTP on the
  app leg (plan Phase F): the originator's SDP is applied to the callee's
  session for codec filtering only. Upstream ran it through `RemoteSDP`,
  which also took the originator's address, direction and SDES key: an app
  caller on RTP/SAVP made a plain trunk callee fail ("remote requested
  secure RTP, but no context is created") and planted the caller's key as
  the callee's remote context. `SecureRTPActive` is what the B2BUA logs per
  leg (`caller_srtp`/`callee_srtp`).
- `media/media_session.go` (`Fork`, `LocalSDP`, `RemoteSDP`) — SRTP across
  re-INVITEs: `Fork` (the session after any re-INVITE: hold, a codec
  change, the peer's own re-INVITE) now carries the SRTP configuration,
  both contexts and the key material; upstream dropped them, so the forked
  session refused the peer's RTP/SAVP answer ("remote requested secure RTP,
  but no context is created" — the transfer-to-trunk codec change failed).
  `LocalSDP` re-offers the session's existing local key instead of
  generating a new one per call, and `RemoteSDP` keeps the remote context
  when the peer's inline key is unchanged: SRTP contexts hold the rollover
  counter, and a fresh context on an unchanged key fails after the first
  sequence wrap (~22 min of audio).

## github.com/emiago/sipgo

- `server.go` (`sipgo.ListenConfig`, `listenUDP/TCP/TLS`) — QoS: every SIP
  listener is opened through a package-level `net.ListenConfig` so the
  B2BUA can mark them DSCP CS3; accepted connections inherit it.

