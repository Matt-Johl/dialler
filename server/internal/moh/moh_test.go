package moh

import "testing"

// Every codec a leg can be held on has music, and it is whole 20 ms frames
// of the same length — a leg held on G.722 must not get a shorter loop than
// one held on Opus.
func TestEveryClipHasEveryCodecAtTheSameLength(t *testing.T) {
	// A leg held on G.722 must not get a shorter loop than one on Opus, and
	// the cadences must be the ETSI ones the app's own tones use: ring-back
	// 1 s on / 4 s off, busy 0.5 / 0.5.
	for _, c := range []struct {
		clip string
		get  func(string) Frames
		secs float64
	}{
		{"music", Music, 8}, {"ringback", Ringback, 5}, {"busy", Busy, 1},
	} {
		for _, codec := range []string{"PCMU", "PCMA", "G722", "opus"} {
			f := c.get(codec)
			if f == nil {
				t.Fatalf("%s/%s: missing", c.clip, codec)
			}
			if got := f.Seconds(); got != c.secs {
				t.Errorf("%s/%s: loop is %.2fs, want %gs", c.clip, codec, got, c.secs)
			}
			for i, frame := range f {
				if len(frame) == 0 {
					t.Fatalf("%s/%s: frame %d is empty", c.clip, codec, i)
				}
			}
		}
	}
}

// The 8 kHz and 16 kHz codecs are constant bitrate at 160 octets per 20 ms;
// a different size means the generator resampled or repacketised wrongly
// and the far end would hear it at the wrong speed.
func TestNarrowbandFramesAreOnePacketOf160(t *testing.T) {
	for _, codec := range []string{"PCMU", "PCMA", "G722"} {
		for i, frame := range Music(codec) {
			if len(frame) != 160 {
				t.Fatalf("%s: frame %d is %dB, want 160", codec, i, len(frame))
			}
		}
	}
}

// An unknown codec is silence, not a crash: a leg negotiated on something
// we never pre-encoded still holds, it just has nothing to play.
func TestUnknownCodecIsSilent(t *testing.T) {
	if Music("speex") != nil || Ringback("speex") != nil || Busy("speex") != nil {
		t.Error("speex should have no hold music")
	}
}

func TestCodecNameIsCaseInsensitive(t *testing.T) {
	if Music("g722") == nil || Music("G722") == nil {
		t.Error("G722 must resolve either way: diago names it G722, SDP lowercases")
	}
}
