package b2bua

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
)

// The relay must forward packets the moment they arrive, carrying the
// source's timing. The library's paced io.Writer could only emit one packet
// per 20 ms, so a burst of N packets — a startup backlog, a stall on the
// network — was smoothed into N×20 ms of permanent delay on the far end.
// These tests drive the real RTP reader/writer with a fake source and sink.

const ulawFrame = 160 // samples per 20 ms G.711 packet, RTP clock 8 kHz

// fakeSource hands out prepared packets; delayBefore[i] stalls before the
// i-th packet to emulate a network hiccup followed by a burst.
type fakeSource struct {
	pkts        []rtp.Packet
	delayBefore map[int]time.Duration
	i           int
}

func (f *fakeSource) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	if f.i >= len(f.pkts) {
		return 0, io.EOF
	}
	if d, ok := f.delayBefore[f.i]; ok {
		time.Sleep(d)
	}
	b, err := f.pkts[f.i].Marshal()
	if err != nil {
		return 0, err
	}
	f.i++
	n := copy(buf, b)
	if err := p.Unmarshal(buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

type sinkPacket struct {
	hdr rtp.Header
	at  time.Time
}

type fakeSink struct {
	mu   sync.Mutex
	pkts []sinkPacket
}

func (s *fakeSink) WriteRTP(p *rtp.Packet) error {
	s.mu.Lock()
	s.pkts = append(s.pkts, sinkPacket{hdr: p.Header, at: time.Now()})
	s.mu.Unlock()
	return nil
}

func (s *fakeSink) all() []sinkPacket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkPacket(nil), s.pkts...)
}

func packet(seq uint16, ts, ssrc uint32, marker bool) rtp.Packet {
	return rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc, Marker: marker},
		Payload: make([]byte, ulawFrame),
	}
}

// runPump relays everything the source has and returns the sink and the
// wall time it took.
func runPump(t *testing.T, src *fakeSource) (*fakeSink, time.Duration) {
	t.Helper()
	sink := &fakeSink{}
	rr := media.NewRTPPacketReader(src, media.CodecAudioUlaw)
	rw := media.NewRTPPacketWriter(sink, media.CodecAudioUlaw)
	p := &pump{r: rr, w: rw}
	p.setPacketPath(rr, rw, media.CodecAudioUlaw)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		p.run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not finish")
	}
	if got, want := int(p.written.Load()), len(src.pkts); got != want {
		t.Fatalf("forwarded %d of %d packets", got, want)
	}
	return sink, time.Since(start)
}

func TestRelayForwardsABurstAtOnce(t *testing.T) {
	src := &fakeSource{}
	for i := 0; i < 50; i++ {
		src.pkts = append(src.pkts, packet(uint16(i), 1000+uint32(i)*ulawFrame, 0xabc, i == 0))
	}
	sink, took := runPump(t, src)
	// The paced writer took 1.0 s for exactly this (measured); a relay must
	// not add anything like a packet time per packet.
	if took > 250*time.Millisecond {
		t.Fatalf("50-packet burst took %v to forward; the relay is pacing", took)
	}
	out := sink.all()
	if !out[0].hdr.Marker {
		t.Fatal("first packet of the stream must carry the marker")
	}
	for i := 1; i < len(out); i++ {
		if d := out[i].hdr.Timestamp - out[i-1].hdr.Timestamp; d != ulawFrame {
			t.Fatalf("packet %d: timestamp step %d, want %d (source timing not carried)", i, d, ulawFrame)
		}
		if out[i].hdr.SequenceNumber != out[i-1].hdr.SequenceNumber+1 {
			t.Fatalf("packet %d: sequence not contiguous", i)
		}
	}
}

func TestRelayPreservesSilenceGapsAndRebasesNewStreams(t *testing.T) {
	src := &fakeSource{}
	ts := uint32(5000)
	for i := 0; i < 40; i++ {
		if i == 10 {
			ts += 25 * ulawFrame // half a second of silence: no packets sent
		}
		ssrc := uint32(0x111)
		if i >= 30 {
			ssrc = 0x222 // the source restarted its stream (new SSRC, new timeline)
			if i == 30 {
				ts = 9_000_000
			}
		}
		src.pkts = append(src.pkts, packet(uint16(i), ts, ssrc, false))
		ts += ulawFrame
	}
	sink, _ := runPump(t, src)
	out := sink.all()
	if d := out[10].hdr.Timestamp - out[9].hdr.Timestamp; d != 26*ulawFrame {
		t.Fatalf("silence gap not preserved: step %d, want %d", d, 26*ulawFrame)
	}
	// A new source stream continues our timeline one frame on, marked.
	if d := out[30].hdr.Timestamp - out[29].hdr.Timestamp; d != ulawFrame {
		t.Fatalf("new stream did not continue our timeline: step %d", d)
	}
	if !out[30].hdr.Marker {
		t.Fatal("first packet of a new source stream must carry the marker")
	}
	if out[29].hdr.Marker || out[31].hdr.Marker {
		t.Fatal("marker set on a packet that does not start a stream")
	}
	if d := out[31].hdr.Timestamp - out[30].hdr.Timestamp; d != ulawFrame {
		t.Fatalf("timing after the new stream: step %d", d)
	}
}

// The relay counts what the source's own sequence numbers and timing say —
// loss, reordering, interarrival jitter — so a lossy leg can be placed from
// the server log without RTCP (audio-quality gates, SPEC §7.2).
func TestRelayCountsLossReorderingAndJitter(t *testing.T) {
	src := &fakeSource{delayBefore: map[int]time.Duration{15: 80 * time.Millisecond}}
	order := []int{0, 1, 2, 3, 4, 5, 6, 8, 9, 10, 12, 11, 13, 14, 15, 16, 17, 18, 19} // 7 lost; 11 late
	for _, i := range order {
		src.pkts = append(src.pkts, packet(uint16(i), uint32(i)*ulawFrame, 0x999, false))
	}
	sink := &fakeSink{}
	rr := media.NewRTPPacketReader(src, media.CodecAudioUlaw)
	rw := media.NewRTPPacketWriter(sink, media.CodecAudioUlaw)
	p := &pump{r: rr, w: rw}
	p.setPacketPath(rr, rw, media.CodecAudioUlaw)
	p.run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := p.lost.Load(); got != 1 {
		t.Fatalf("lost = %d, want 1 (packet 7; the late 11 must not count)", got)
	}
	if got := p.reordered.Load(); got != 1 {
		t.Fatalf("reordered = %d, want 1 (packet 11 after 12)", got)
	}
	// Packets arrive back to back (far faster than their 20 ms spacing) with
	// one 80 ms stall: the jitter estimate must be clearly non-zero.
	if j := p.jitterMs(); j <= 1 {
		t.Fatalf("jitter_ms = %.2f; a burst with a stall should register", j)
	}
	if !strings.Contains(p.String(), "lost=1 reordered=1") {
		t.Fatalf("summary lacks the counters: %s", p.String())
	}
}

func TestRelayReportsArrivalSkew(t *testing.T) {
	// Ten packets at once, a 300 ms stall, then twenty at once: what a
	// network hiccup looks like. The stalled packet is late against the
	// source's timeline; the burst after it is early.
	src := &fakeSource{delayBefore: map[int]time.Duration{10: 300 * time.Millisecond}}
	for i := 0; i < 31; i++ {
		src.pkts = append(src.pkts, packet(uint16(i), uint32(i)*ulawFrame, 0x777, false))
	}
	sink := &fakeSink{}
	rr := media.NewRTPPacketReader(src, media.CodecAudioUlaw)
	rw := media.NewRTPPacketWriter(sink, media.CodecAudioUlaw)
	p := &pump{r: rr, w: rw}
	p.setPacketPath(rr, rw, media.CodecAudioUlaw)
	start := time.Now()
	p.run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	took := time.Since(start)
	if took > 600*time.Millisecond {
		t.Fatalf("31 packets with one 300 ms stall took %v; the burst after the stall was paced", took)
	}
	if late := p.lateMaxMs.Load(); late < 50 {
		t.Fatalf("late_max_ms = %d; the stalled packet should read as late", late)
	}
	if early := p.earlyMaxMs.Load(); early < 200 {
		t.Fatalf("early_max_ms = %d; the burst after the stall should read as early", early)
	}
	// And the burst went out as one: the last packet left within a few ms
	// of the stalled one, not 400 ms later.
	out := sink.all()
	if spread := out[len(out)-1].at.Sub(out[10].at); spread > 100*time.Millisecond {
		t.Fatalf("burst spread over %v on the way out", spread)
	}
}
