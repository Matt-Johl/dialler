package b2bua

import (
	"net"
	"testing"

	"github.com/emiago/diago/media"
)

// A PBX answer in any of the shapes Asterisk produces negotiates against the
// default trunk offer: PCMU only, PCMU+PCMA, or G.722.
func TestTrunkOfferAcceptsPBXAnswers(t *testing.T) {
	for _, ans := range []string{
		"v=0\r\no=- 1 2 IN IP4 10.18.0.5\r\ns=Asterisk\r\nc=IN IP4 10.18.0.5\r\nt=0 0\r\nm=audio 18104 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16\r\na=ptime:20\r\na=maxptime:150\r\na=sendrecv\r\n",
		"v=0\r\no=- 1 2 IN IP4 10.18.0.5\r\ns=Asterisk\r\nc=IN IP4 10.18.0.5\r\nt=0 0\r\nm=audio 18104 RTP/AVP 0 8 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16\r\na=sendrecv\r\n",
		"v=0\r\no=- 1 2 IN IP4 10.18.0.5\r\ns=Asterisk\r\nc=IN IP4 10.18.0.5\r\nt=0 0\r\nm=audio 18104 RTP/AVP 9 101\r\na=rtpmap:9 G722/8000\r\na=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n",
	} {
		ms := &media.MediaSession{Codecs: append([]media.Codec(nil), DefaultTrunkCodecs...), Laddr: net.UDPAddr{IP: net.IPv4(10, 18, 0, 212), Port: 20000}, Mode: "sendrecv"}
		_ = ms.LocalSDP()
		if err := ms.RemoteSDP([]byte(ans)); err != nil {
			t.Errorf("answer %q → %v", ans, err)
		} else {
			t.Logf("negotiated %v", media.CodecAudioFromSession(ms))
		}
	}
}
