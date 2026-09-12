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

