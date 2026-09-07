# Local changes to vendored modules

The server vendors its dependencies (`go mod vendor` is not run in the
sandbox; the module proxy is unreachable). The files below differ from the
upstream release named in `modules.txt`; re-apply when bumping.

## github.com/emiago/diago v0.32.0

- `dialog_client_session.go`: `func (d *DialogClientSession) SetCodecs([]media.Codec)`
  — per-dialog codec offer for an outgoing leg. Used by `internal/b2bua` to
  offer only G.711 towards the PBX trunk, so the raw relay never needs to
  transcode (the app leg is answered with the codec the trunk leg took).
